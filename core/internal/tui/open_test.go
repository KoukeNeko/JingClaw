package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// A call that left output says so.
//
// The whole reason the id travels: reopening a session otherwise loses every
// way of reaching the build log that explains why something failed.
func TestACallThatLeftOutputSaysSo(t *testing.T) {
	drawn := drawSession(t, Screen{Messages: []Message{{
		Role: domain.RoleAssistant,
		ToolCalls: []ToolCall{{
			ID: "call_1", Name: "exec_command", Completed: true,
			IsError: true, Artifact: "sha256-abc", MediaType: "text/plain",
		}},
	}}})

	// On the call's own line. The key line says "open output" too, so
	// looking anywhere on the screen would find that instead and pass with
	// the marker gone.
	line := lineOf(t, drawn, "exec_command")
	if !strings.Contains(strings.ToLower(strings.Split(drawn, "\n")[line]), "output") {
		t.Errorf("a call that stored output drew nothing about it:\n%s", drawn)
	}
}

// And one that left none does not.
func TestACallThatLeftNothingSaysNothing(t *testing.T) {
	drawn := drawSession(t, Screen{Messages: []Message{{
		Role:      domain.RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Completed: true}},
	}}})

	line := lineOf(t, drawn, "read_file")
	if strings.Contains(strings.ToLower(strings.Split(drawn, "\n")[line]), "output") {
		t.Errorf("a call that stored nothing was marked as having output:\n%s", drawn)
	}
}

// Opening writes the bytes somewhere and hands the path to the machine.
//
// Not rendered here. A terminal is a poor image viewer and a worse PDF
// reader, and the thing that already knows how to open a file is the one the
// person configured for it.
func TestOpeningHandsTheFileToTheMachine(t *testing.T) {
	into := t.TempDir()
	opened := &recordingOpener{}
	model := withOutput(t, opened, into, ToolCall{
		ID: "call_1", Name: "exec_command", Completed: true,
		Artifact: "sha256-abc", MediaType: "text/plain",
	})

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if opened.path == "" {
		t.Fatal("nothing was handed to the machine to open")
	}
	written, err := os.ReadFile(opened.path)
	if err != nil {
		t.Fatalf("the file handed over is not there: %v", err)
	}
	if string(written) != "the build log" {
		t.Errorf("what was written is %q", written)
	}
	if filepath.Dir(opened.path) != into {
		t.Errorf("it was written to %s rather than into the run's own directory",
			filepath.Dir(opened.path))
	}

	// Never executable, whatever it is. A mode is not a judgement about the
	// contents, and the difference here is between opening a document and
	// running one.
	about, err := os.Stat(opened.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if about.Mode().Perm()&0o111 != 0 {
		t.Errorf("the file handed to the machine is executable: %v", about.Mode())
	}
}

// The name comes from the media type, not from anything the agent chose.
//
// What an extension does is pick the program that opens the file. Taking one
// from a tool's own output would let a run decide which program runs on the
// operator's machine, which is a larger thing than showing them a log.
func TestTheNameComesFromTheMediaTypeAndNotFromTheAgent(t *testing.T) {
	opened := &recordingOpener{}
	model := withOutput(t, opened, t.TempDir(), ToolCall{
		ID: "call_1", Name: "exec_command", Completed: true,
		Artifact: "sha256-abc", MediaType: "text/plain",
	})

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if extension := filepath.Ext(opened.path); extension != ".txt" {
		t.Errorf("a text/plain artifact was written as %q", filepath.Base(opened.path))
	}
}

// A type nobody should be handed to a default program is refused.
//
// The refusal is the point rather than an inconvenience: an artifact is
// whatever a tool produced, which includes whatever a page the run read
// suggested it produce. Handing that to the machine's default program for it
// is running somebody else's file.
func TestATypeThatShouldNotBeOpenedIsRefused(t *testing.T) {
	opened := &recordingOpener{}
	model := withOutput(t, opened, t.TempDir(), ToolCall{
		ID: "call_1", Name: "exec_command", Completed: true,
		Artifact: "sha256-abc", MediaType: "application/x-mach-binary",
	})

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if opened.path != "" {
		t.Fatalf("an executable was handed to the machine at %s", opened.path)
	}
	if drawn := view(model); !strings.Contains(drawn, "application/x-mach-binary") {
		t.Errorf("the refusal does not say what was refused:\n%s", drawn)
	}
}

// It opens the newest output, which is the one being read about.
func TestItOpensTheNewestOutput(t *testing.T) {
	opened := &recordingOpener{}
	model := start(t, &recordingSessions{artifact: "the build log", opener: opened,
		into: t.TempDir()}, listed{sessions: []Summary{{ID: "ses_1"}}})
	// In separate turns as well as separate calls, because a session is
	// mostly one call per turn and a search that walked the turns the wrong
	// way would still find the right call inside a single one.
	model = after(model, showing{id: "ses_1", screen: Screen{Messages: []Message{
		{
			Role: domain.RoleAssistant,
			ToolCalls: []ToolCall{{ID: "call_1", Name: "first", Completed: true,
				Artifact: "sha256-old", MediaType: "text/plain"}},
		},
		{
			Role: domain.RoleAssistant,
			ToolCalls: []ToolCall{{ID: "call_2", Name: "second", Completed: true,
				Artifact: "sha256-new", MediaType: "text/plain"}},
		},
	}}})

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if opened.wanted != "sha256-new" {
		t.Errorf("it opened %q rather than the newest output", opened.wanted)
	}
}

// With nothing stored, the key does nothing.
func TestOpeningWithNothingStoredDoesNothing(t *testing.T) {
	opened := &recordingOpener{}
	model := start(t, &recordingSessions{opener: opened, into: t.TempDir()},
		listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1"})

	runCommand(t, secondOf(model.Update(key("o"))))

	if opened.path != "" {
		t.Errorf("a session with no stored output opened %s", opened.path)
	}
}

// A read the daemon refused is said, not swallowed.
func TestAnOutputThatCannotBeReadIsSaidOutLoud(t *testing.T) {
	refusing := &recordingSessions{refuse: errRefused, opener: &recordingOpener{},
		into: t.TempDir()}
	model := start(t, refusing, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{Messages: []Message{{
		Role: domain.RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call_1", Name: "exec_command", Completed: true,
			Artifact: "sha256-abc", MediaType: "text/plain"}},
	}}}})

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if drawn := view(model); !strings.Contains(drawn, errRefused.Error()) {
		t.Errorf("an output that could not be read drew:\n%s", drawn)
	}
}

// What kind of output it is survives the trip in.
//
// The screens above are handed a media type already on them. It arrives on
// the event, and losing it there would leave every stored output refused as
// an unknown kind — which reads exactly like the refusal working.
func TestTheKindOfOutputSurvivesTheFold(t *testing.T) {
	opened := &recordingOpener{}
	model := start(t, &recordingSessions{artifact: "the build log", opener: opened,
		into: t.TempDir()}, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{HeadSeq: 4}})

	for _, event := range []domain.Event{
		{Seq: 5, Kind: domain.EventToolCallRequested,
			Payload: domain.ToolCallRequested{CallID: "call_1", Name: "exec_command"}},
		{Seq: 6, Kind: domain.EventToolCallCompleted,
			Payload: domain.ToolCallCompleted{
				CallID: "call_1", Name: "exec_command",
				Artifact: &domain.Artifact{
					ID: "sha256-abc", Size: 900, MediaType: "text/plain",
				},
			}},
	} {
		model = after(model, arrived{update: Update{Event: &event}})
	}

	model = after(model, runCommand(t, secondOf(model.Update(key("o")))))

	if opened.path == "" {
		t.Fatalf("output stored while watching could not be opened:\n%s", view(model))
	}
	if extension := filepath.Ext(opened.path); extension != ".txt" {
		t.Errorf("the kind of the output did not survive: %q", filepath.Base(opened.path))
	}
}

// Writing over an existing file still leaves it unopenable as a program.
//
// The mode passed to a write applies when the file is created and not when it
// is replaced. These names are a digest and an extension, and the directory
// they go in outlives a run, so the second time an artifact is opened is a
// write over a file that is already there — and the promise is about the file
// handed over, not about the first one.
func TestWritingOverAnExistingFileStillLeavesItUnrunnable(t *testing.T) {
	into := t.TempDir()

	// One left behind by something else, executable.
	path := filepath.Join(into, "sha256-abc.txt")
	if err := os.WriteFile(path, []byte("stale"), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}

	written, err := writeForOpening(into, "sha256-abc", ".txt", []byte("the build log"))
	if err != nil {
		t.Fatalf("writing over it: %v", err)
	}

	about, err := os.Stat(written)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if about.Mode().Perm()&0o111 != 0 {
		t.Errorf("the file written over an executable one is still executable: %v",
			about.Mode())
	}
}

// An id cannot become a path.
//
// An artifact id is a digest and looks nothing like a path, which is exactly
// why this is checked: the day one does not, a name with a slash in it writes
// wherever the slash points, and the panel is the thing holding the pen.
func TestAnIdCannotBecomeAPath(t *testing.T) {
	for _, id := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"sha256-abc/../../..",
		"..",
	} {
		named := safeName(id)
		if strings.ContainsAny(named, `/\`) || strings.Contains(named, "..") {
			t.Errorf("%q became %q, which still points somewhere", id, named)
		}
	}

	// And an ordinary digest keeps its own name, or the files nobody can tell
	// apart are the ones somebody has to choose between.
	if named := safeName("sha256-abc123"); named != "sha256-abc123" {
		t.Errorf("an ordinary id was rewritten to %q", named)
	}

	// Nothing at all still has to be a name.
	if named := safeName("!!!"); named == "" {
		t.Error("an id of nothing but punctuation left no name at all")
	}
}

// withOutput is a panel showing a session whose last call stored something.
func withOutput(t *testing.T, opened *recordingOpener, into string, call ToolCall) tea.Model {
	t.Helper()

	sessions := &recordingSessions{
		artifact: "the build log", opener: opened, into: into,
	}
	model := start(t, sessions, listed{sessions: []Summary{{ID: "ses_1"}}})
	return after(model, showing{id: "ses_1", screen: Screen{Messages: []Message{{
		Role: domain.RoleAssistant, ToolCalls: []ToolCall{call},
	}}}})
}

// recordingOpener remembers what it was asked to open instead of opening it.
type recordingOpener struct {
	path   string
	wanted string
}

func (r *recordingOpener) Open(_ context.Context, path string) error {
	r.path = path
	return nil
}
