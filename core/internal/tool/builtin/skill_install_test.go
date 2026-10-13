package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/skill"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// fakeInstaller stands in for the real one, so these tests are about the tools
// — what they refuse, what they show a person deciding — and not about git.
type fakeInstaller struct {
	stagedReturn skill.Staged
	stageErr     error
	stagedSource skill.Source

	activateErr error
	activated   string

	readStaged skill.Staged
	readSkill  skill.Skill
	readErr    error
}

func (f *fakeInstaller) Stage(_ context.Context, source skill.Source) (skill.Staged, error) {
	f.stagedSource = source
	if f.stageErr != nil {
		return skill.Staged{}, f.stageErr
	}
	return f.stagedReturn, nil
}

func (f *fakeInstaller) Activate(name string) (skill.Locked, error) {
	f.activated = name
	if f.activateErr != nil {
		return skill.Locked{}, f.activateErr
	}
	return skill.Locked{Name: name, TreeDigest: "sha256:abcdef0123456789", From: f.readStaged.Source}, nil
}

func (f *fakeInstaller) Staged(string) (skill.Staged, skill.Skill, error) {
	if f.readErr != nil {
		return skill.Staged{}, skill.Skill{}, f.readErr
	}
	return f.readStaged, f.readSkill, nil
}

func mkCall(t *testing.T, arguments any) tool.Call {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("arguments: %v", err)
	}
	return tool.Call{Arguments: raw}
}

// Staging is a fetch that steers nothing, and it is offered at network-read:
// where an operator is present it is allowed, and on the untrusted gateway it
// asks. Activating is the act that changes how the agent behaves, and it is
// remember — the level every attended profile stops for.
func TestTheTwoToolsAreAtTheLevelsTheirBlastRadiusDeserves(t *testing.T) {
	if got := (&builtin.SkillStage{}).Spec().Level; got != tool.LevelNetworkRead {
		t.Errorf("skill_stage is at %s, not network_read", got)
	}
	if got := (&builtin.SkillActivate{}).Spec().Level; got != tool.LevelRemember {
		t.Errorf("skill_activate is at %s, not remember", got)
	}
	// What skill_stage returns came from a repository, so it is foreign.
	if !(&builtin.SkillStage{}).Spec().Capabilities.ForeignContent {
		t.Error("skill_stage does not mark its result as foreign, though it echoes a repository")
	}
}

func TestStagingReportsWhatArrived(t *testing.T) {
	installer := &fakeInstaller{stagedReturn: skill.Staged{
		Name:        "release",
		Source:      skill.Source{Repository: "https://x/y", Commit: "abc", Path: "release"},
		Description: "How this repository is released.",
		TreeDigest:  "sha256:deadbeef",
		Size:        420,
	}}
	stage := &builtin.SkillStage{Installer: installer}

	result, err := stage.Execute(context.Background(), mkCall(t, map[string]string{
		"source": "git:https://x/y#0123456789012345678901234567890123456789:release",
	}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.IsError {
		t.Fatalf("staging a valid source was refused: %s", result.Content)
	}
	if installer.stagedSource.Commit != "0123456789012345678901234567890123456789" {
		t.Errorf("the source was not parsed and passed through: %+v", installer.stagedSource)
	}
	for _, want := range []string{"release", "How this repository is released.", "activate"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("the result does not mention %q: %s", want, result.Content)
		}
	}
}

// A source that does not parse is refused as a tool result the model can act
// on, not as an error that ends the run.
func TestStagingRefusesAnUnparseableSource(t *testing.T) {
	stage := &builtin.SkillStage{Installer: &fakeInstaller{}}
	result, err := stage.Execute(context.Background(), mkCall(t, map[string]string{"source": "not a source"}))
	if err != nil {
		t.Fatalf("execute returned an error rather than a refusal: %v", err)
	}
	if !result.IsError {
		t.Error("an unparseable source was not refused")
	}
}

// An ssh address is fetched with the keys, the configuration and the known hosts
// of the account the daemon runs as. Staging is allowed without asking where an
// operator is present, so a run is not given that; the address is fine to the
// parser, because a person may stage one.
func TestStagingDoesNotFetchOverSSHOnTheAgentsOwnAccount(t *testing.T) {
	installer := &fakeInstaller{}
	stage := &builtin.SkillStage{Installer: installer}

	source := "git:ssh://git@example.com/x/y.git#0123456789abcdef0123456789abcdef01234567"
	if _, err := skill.ParseSource(source); err != nil {
		t.Fatalf("the parser refuses an ssh address: %v", err)
	}

	result, err := stage.Execute(context.Background(), mkCall(t, map[string]string{"source": source}))
	if err != nil {
		t.Fatalf("execute returned an error rather than a refusal: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "ssh") {
		t.Errorf("an ssh address was not refused, or not said why: %+v", result)
	}
	if installer.stagedSource.Repository != "" {
		t.Errorf("it fetched %q", installer.stagedSource.Repository)
	}

	// And an address that is not ssh still goes through.
	over := "git:https://example.com/x/y#0123456789abcdef0123456789abcdef01234567"
	if result, _ := stage.Execute(context.Background(), mkCall(t, map[string]string{"source": over})); result.IsError {
		t.Errorf("an https address was refused: %+v", result)
	}
}

// Activating a skill puts it over the one of the same name that is there, and
// does not ask. The person deciding is told, the way the dashboard's review is.
func TestActivatePreviewSaysWhenItWouldReplaceASkill(t *testing.T) {
	skills := t.TempDir()
	staged := skill.Staged{
		Name:   "release",
		Source: skill.Source{Repository: "https://github.com/x/y", Commit: "0123456789abcdef"},
		Dir:    filepath.Join(skills, ".staged", "release"),
	}
	activate := &builtin.SkillActivate{Installer: &fakeInstaller{
		readStaged: staged, readSkill: skill.Skill{Name: "release", Body: "Tag it."},
	}}
	call := mustJSON(t, map[string]string{"name": "release"})

	if preview := activate.Preview(call); strings.Contains(preview, "replaces") {
		t.Errorf("nothing is there, and the preview says it replaces something:\n%s", preview)
	}

	if err := os.Mkdir(filepath.Join(skills, "release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if preview := activate.Preview(call); !strings.Contains(preview, "replaces") || !strings.Contains(preview, "removed") {
		t.Errorf("a skill is there, and the preview does not say it is replaced:\n%s", preview)
	}
}

// The activate preview is the whole point: a person deciding sees where the
// skill came from, an exact commit, a digest of the whole directory, its size,
// and the instructions themselves — with it said plainly that this becomes
// standing instructions. A description alone would be the author vouching for
// their own skill.
func TestActivatePreviewShowsTheRealThing(t *testing.T) {
	installer := &fakeInstaller{
		readStaged: skill.Staged{
			Name:        "release",
			Source:      skill.Source{Repository: "https://github.com/x/y", Commit: "0123456789abcdef", Path: "release"},
			Description: "How this repository is released.",
			TreeDigest:  "sha256:feedface",
			Size:        512,
		},
		readSkill: skill.Skill{
			Name: "release",
			Body: "Tag the commit, then push the tag.",
		},
	}
	activate := &builtin.SkillActivate{Installer: installer}

	preview := activate.Preview(mustJSON(t, map[string]string{"name": "release"}))

	for _, want := range []string{
		"https://github.com/x/y", // where it came from
		"0123456789abcdef",       // the exact commit
		"sha256:feedface",        // the whole-tree digest
		"512",                    // the size
		"every future session",   // the blast radius, said plainly
		"Tag the commit",         // the instructions themselves
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not show %q:\n%s", want, preview)
		}
	}
}

// What a person is shown is not a place to repeat a password that an older
// version let into the address of a skill that is still waiting.
func TestActivatePreviewDoesNotRepeatACredentialInTheAddress(t *testing.T) {
	installer := &fakeInstaller{
		readStaged: skill.Staged{
			Name:   "release",
			Source: skill.Source{Repository: "https://user:hunter2@github.com/x/y", Commit: "0123456789abcdef"},
		},
		readSkill: skill.Skill{Name: "release", Body: "Tag it."},
	}

	preview := (&builtin.SkillActivate{Installer: installer}).Preview(mustJSON(t, map[string]string{"name": "release"}))

	if strings.Contains(preview, "hunter2") || !strings.Contains(preview, "github.com/x/y") {
		t.Errorf("the preview shows the address as:\n%s", preview)
	}
}

// A preview for a name nothing is staged under says so rather than panicking
// or rendering an empty, reassuring box.
func TestActivatePreviewOfNothingStaged(t *testing.T) {
	installer := &fakeInstaller{readErr: errStub}
	activate := &builtin.SkillActivate{Installer: installer}
	preview := activate.Preview(mustJSON(t, map[string]string{"name": "ghost"}))
	if !strings.Contains(preview, "staged") {
		t.Errorf("the preview does not say nothing is staged: %q", preview)
	}
}

// Activating with no name is refused as a result, not run.
func TestActivateRefusesAnEmptyName(t *testing.T) {
	installer := &fakeInstaller{}
	activate := &builtin.SkillActivate{Installer: installer}
	result, err := activate.Execute(context.Background(), mkCall(t, map[string]string{"name": "  "}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !result.IsError {
		t.Error("an empty name was accepted")
	}
	if installer.activated != "" {
		t.Errorf("it tried to activate %q from an empty name", installer.activated)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return raw
}

type stubError struct{}

func (stubError) Error() string { return "nothing is staged" }

var errStub = stubError{}
