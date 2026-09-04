package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// SkillInstaller fetches, stages and activates skills.
//
// An interface so this package keeps not knowing where a deployment puts its
// files. The two tools below are the two halves of installing one: staging
// fetches bytes that steer nothing, and activating puts standing instructions
// in front of the model for every future session.
type SkillInstaller interface {
	Stage(ctx context.Context, source skill.Source) (skill.Staged, error)
	Activate(name string) (skill.Locked, error)
	// Staged reads back one staged skill for an approval to be decided
	// against — the recorded source and the bytes now on disk.
	Staged(name string) (skill.Staged, skill.Skill, error)
}

// SkillStage fetches a skill and stages it, without putting it in front of the
// model.
//
// Network-read rather than execute: what it does is retrieve bytes into a
// place the catalogue does not read, so nothing it fetches steers anything.
// The act that matters — making those instructions standing — is skill_activate
// below, and it is gated on its own. Splitting them is the point: an operator
// present can let the agent fetch and look without also having authorised the
// agent to change how it behaves from the next turn on.
type SkillStage struct {
	Installer SkillInstaller
}

func (t *SkillStage) Spec() tool.Spec {
	return tool.Spec{
		Name: "skill_stage",
		Description: "Fetch a skill from a git repository and stage it for review, without " +
			"installing it. The source names a repository, an exact commit, and a path inside " +
			"it: git:https://host/owner/repo#<40-character commit>:path. A staged skill is not " +
			"in front of you and steers nothing until it is activated. This returns what " +
			"actually arrived — its name, what it says it is for, and a digest — so activating " +
			"it is a decision about the real thing. Use it when a skill you do not have would " +
			"help and you know where it lives.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "source": {
      "type": "string",
      "description": "git:<url>#<40-character commit>[:path] — a commit, never a branch or tag."
    }
  },
  "required": ["source"],
  "additionalProperties": false
}`),
		Level: tool.LevelNetworkRead,
		Capabilities: tool.Capabilities{
			// What it reports came from a remote repository — the name and
			// the description are the author's words, not the operator's. So
			// a run that staged a skill is marked as having read foreign
			// content, which a later approval shows.
			Provenance:     domain.ProvenanceExternal,
			ForeignContent: true,
			Network:        true,
			ReadFS:         true,
			WriteFS:        true,
		},
	}
}

type skillStageArgs struct {
	Source string `json:"source"`
}

func (t *SkillStage) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args skillStageArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, fmt.Errorf("skill_stage: unusable arguments: %w", err)
	}

	source, err := skill.ParseSource(strings.TrimSpace(args.Source))
	if err != nil {
		return refusal(err.Error()), nil
	}

	staged, err := t.Installer.Stage(ctx, source)
	if err != nil {
		return refusal(err.Error()), nil
	}

	return tool.Result{
		Summary: fmt.Sprintf("staged %s (%s)", staged.Name, shortDigest(staged.TreeDigest)),
		Content: fmt.Sprintf(
			"Staged the skill %q from %s.\n\n"+
				"It says it is for: %s\n\n"+
				"It is %d bytes, tree digest %s. It is not installed and steers nothing yet. "+
				"To install it, ask to activate %q — a person will be shown what it is and what "+
				"it would change before deciding.",
			staged.Name, staged.Source, staged.Description, staged.Size,
			staged.TreeDigest, staged.Name),
	}, nil
}

// SkillActivate installs a staged skill, putting its instructions in front of
// the model from the next turn on.
//
// Remember rather than a workspace write, because the reach is the same in
// kind: a skill is standing instructions read into every future session, and
// like a memory it is believed by an agent that has forgotten where it came
// from. Every attended profile stops for it, and its preview is built from the
// staged bytes so the person deciding sees the real thing.
type SkillActivate struct {
	Installer SkillInstaller
}

func (t *SkillActivate) Spec() tool.Spec {
	return tool.Spec{
		Name: "skill_activate",
		Description: "Install a skill that was staged with skill_stage, by its name. This puts " +
			"its instructions among the ones you follow, for this and every future session, so " +
			"a person is asked to approve it and is shown where it came from and what it says. " +
			"It grants nothing on its own: anything the skill tells you to do still goes through " +
			"the same permission checks.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The staged skill's name, as skill_stage reported it."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`),
		Level: tool.LevelRemember,
		Capabilities: tool.Capabilities{
			Provenance: domain.ProvenanceLocalUnknown,
			ReadFS:     true,
			WriteFS:    true,
		},
	}
}

type skillActivateArgs struct {
	Name string `json:"name"`
}

func (t *SkillActivate) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args skillActivateArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, fmt.Errorf("skill_activate: unusable arguments: %w", err)
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		return refusal("skill_activate needs the name of a staged skill."), nil
	}

	locked, err := t.Installer.Activate(name)
	if err != nil {
		return refusal(err.Error()), nil
	}

	return tool.Result{
		Summary: fmt.Sprintf("installed %s (%s)", locked.Name, shortDigest(locked.TreeDigest)),
		Content: fmt.Sprintf(
			"Installed the skill %q from %s. Its instructions are now among the ones you "+
				"follow. It grants nothing: anything it tells you to do still goes through the "+
				"same permission checks and the same approvals.",
			locked.Name, locked.From),
	}, nil
}

// Preview is what a person deciding whether to activate a skill is shown.
//
// Built from the staged bytes, not from the call: the name in the call is a
// key, and the decision is about the instructions on disk under it. A
// description alone would be theatre — the author wrote it — so this shows
// where it came from, an exact commit, a digest of the whole directory, its
// size, and says plainly that it becomes standing instructions.
func (t *SkillActivate) Preview(arguments json.RawMessage) string {
	var args skillActivateArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return ""
	}

	staged, one, err := t.Installer.Staged(name)
	if err != nil {
		return fmt.Sprintf("no skill named %q is staged to activate", name)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Install the skill %q as standing instructions.\n\n", staged.Name)
	fmt.Fprintf(&out, "  from        %s\n", staged.Source.Repository)
	if staged.Source.Path != "" {
		fmt.Fprintf(&out, "  path        %s\n", staged.Source.Path)
	}
	fmt.Fprintf(&out, "  commit      %s\n", staged.Source.Commit)
	fmt.Fprintf(&out, "  tree        %s\n", staged.TreeDigest)
	fmt.Fprintf(&out, "  size        %d bytes\n", staged.Size)
	fmt.Fprintf(&out, "  says it is  %s\n\n", staged.Description)
	out.WriteString(
		"This becomes an instruction the agent follows in this and every future session, " +
			"for everyone who uses this deployment. It grants nothing on its own. Read the " +
			"instructions below before approving.\n\n")
	out.WriteString("--- SKILL.md ---\n")
	out.WriteString(one.Body)
	return out.String()
}

// shortDigest is the first bytes of a sha256:... digest, enough to recognise
// and too few to retype.
func shortDigest(digest string) string {
	trimmed := strings.TrimPrefix(digest, "sha256:")
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}
