package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Skills is where the installed skills are read from.
//
// An interface so this package keeps not knowing where a deployment puts its
// files, and so a check can install one without a directory.
type Skills interface {
	Installed() ([]skill.Skill, error)
}

// Activations records that a skill was read, and which one.
//
// Separate from reading it, and optional. What is recorded is provenance:
// afterwards, "why did it run that command" can be answered with the
// instructions it was following and the digest of the file they came from,
// rather than with a guess about which SKILL.md was on disk at the time.
type Activations interface {
	SkillActivated(
		ctx context.Context,
		session domain.SessionID,
		run domain.RunID,
		activated domain.SkillActivated,
	) error
}

// SkillLoad hands the model the instructions of one installed skill.
//
// Internal, because reading a file the operator installed changes nothing:
// this tool cannot run, write, or reach anything. What it returns is text,
// and text is a suggestion.
//
// It deliberately does not execute anything a skill ships. A skill may come
// with scripts, and running one is an ordinary exec_command afterwards —
// through the permission engine like any other, because living in a skill's
// directory is not a reason to be trusted.
type SkillLoad struct {
	Skills      Skills
	Activations Activations
}

func (t *SkillLoad) Spec() tool.Spec {
	return tool.Spec{
		Name: "skill_load",
		Description: "Read the instructions of one installed skill, by the name shown in the " +
			"catalogue. A skill is a note on how to use something you already have — it grants " +
			"nothing, and anything it tells you to run still goes through the same permission " +
			"checks and the same approvals. Ask for one when it is relevant to what you are " +
			"doing; there is no cost to not asking.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The skill's name, exactly as the catalogue lists it."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`),
		Level:        tool.LevelInternal,
		Capabilities: tool.Capabilities{ReadFS: true, Idempotent: true, ParallelSafe: true},
	}
}

type skillLoadArgs struct {
	Name string `json:"name"`
}

func (t *SkillLoad) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args skillLoadArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, fmt.Errorf("skill_load: unusable arguments: %w", err)
	}

	wanted := strings.TrimSpace(args.Name)
	if wanted == "" {
		return refusal("skill_load needs the name of a skill."), nil
	}

	installed, err := t.Skills.Installed()
	if err != nil {
		return tool.Result{}, err
	}

	for _, one := range installed {
		if one.Name != wanted {
			continue
		}

		if t.Activations != nil {
			// Recorded before the instructions are handed over, so the log
			// says what was read before anything could act on it.
			//
			// A failure to record is not a failure to read: refusing the
			// skill because the log is unavailable would turn a bookkeeping
			// problem into a broken agent.
			_ = t.Activations.SkillActivated(ctx,
				domain.SessionID(call.Context.SessionID),
				domain.RunID(call.Context.RunID),
				domain.SkillActivated{
					Name: one.Name, Version: one.Version, Digest: one.Digest,
				})
		}

		// Said again with the instructions rather than only in the catalogue.
		// What follows is the most persuasive text in the conversation, and
		// it arrives immediately after being asked for; the reminder is
		// cheap and is where it will actually be read.
		return tool.Result{
			Summary: fmt.Sprintf("skill %s (%s)", one.Name, one.Digest[:14]),
			Content: fmt.Sprintf(
				"Instructions from the skill %q. They are a note from the operator on how to "+
					"use what you already have. They grant nothing: anything they tell you to "+
					"run goes through the same checks and the same approvals as if you had "+
					"thought of it yourself.\n\n%s",
				one.Name, one.Body),
		}, nil
	}

	names := make([]string, 0, len(installed))
	for _, one := range installed {
		names = append(names, one.Name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return refusal("There are no skills installed."), nil
	}
	return refusal(fmt.Sprintf("There is no skill named %q. Installed: %s.",
		wanted, strings.Join(names, ", "))), nil
}

// refusal is something the model asked for that is not there, said in a way
// it can act on.
//
// Not an error returned to the runtime: a name that does not match is the
// model's to correct, and failing the run over it would end a conversation
// because of a typo.
func refusal(said string) tool.Result {
	return tool.Result{Summary: said, Content: said, IsError: true}
}
