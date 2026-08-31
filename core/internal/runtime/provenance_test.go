package runtime_test

import (
	"context"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// resultsFrom is the provenance recorded on each tool result of a session.
func resultsFrom(t *testing.T, store *memory.Store, session domain.SessionID) map[string]domain.Provenance {
	t.Helper()

	events, err := store.ListAfter(context.Background(), session, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	from := map[string]domain.Provenance{}
	for _, event := range events {
		if done, ok := event.Payload.(domain.ToolCallCompleted); ok {
			from[done.Name] = done.From
		}
	}
	return from
}

// TestWhatEachToolReturnsSaysWhoWroteIt is the distinction the single boolean
// could not make.
//
// A compiler diagnostic and a stranger's web page are both "not the
// operator", and only one of them is somebody else's words. Judging them the
// same is why commands went unmarked for so long: marking them would have put
// the warning on almost every run, and a warning that is always on is one
// nobody reads.
func TestWhatEachToolReturnsSaysWhoWroteIt(t *testing.T) {
	rt, store, _, _ := newToolHarness(t, [][]provider.Event{
		{
			toolCall("call_1", "glob_files", map[string]any{"pattern": "**/*.go"}),
			toolCall("call_2", "read_file", map[string]any{"path": "src/main.go"}),
		},
		{provider.TextDelta{Text: "Read them."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "provenance")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "what is here")

	from := resultsFrom(t, store, session.ID)
	for _, name := range []string{"glob_files", "read_file"} {
		got, ran := from[name]
		if !ran {
			t.Fatalf("%s never ran, so this proves nothing", name)
		}
		// Not the operator, and not somebody else either: honest output from
		// this machine, which is nobody asking for anything.
		if got != domain.ProvenanceLocalUnknown {
			t.Errorf("%s returns %q, want local_unknown", name, got)
		}
	}
}

// TestARunCarriesTheWorstThingItHasRead is the invariant, as a function.
//
// Text may lose authority and may never gain it without a person or the
// runtime saying so. A run that read a page and then listed a directory has
// still read a page.
func TestARunCarriesTheWorstThingItHasRead(t *testing.T) {
	for _, one := range []struct {
		first, then, want domain.Provenance
	}{
		{domain.ProvenanceOperator, domain.ProvenanceLocalUnknown, domain.ProvenanceLocalUnknown},
		{domain.ProvenanceLocalUnknown, domain.ProvenanceExternal, domain.ProvenanceExternal},
		{domain.ProvenanceExternal, domain.ProvenanceLocalUnknown, domain.ProvenanceExternal},
		{domain.ProvenanceExternal, domain.ProvenanceOperator, domain.ProvenanceExternal},
		{domain.ProvenanceLocalUnknown, domain.ProvenanceOperator, domain.ProvenanceLocalUnknown},
	} {
		if got := one.first.Worse(one.then); got != one.want {
			t.Errorf("%q then %q gave %q, want %q", one.first, one.then, got, one.want)
		}
	}
}

// TestOnlyOneOfTheThreeIsTheOperator keeps the zero value meaning what it
// says, since a tool that declares nothing gets it.
func TestOnlyOneOfTheThreeIsTheOperator(t *testing.T) {
	if !domain.ProvenanceOperator.IsOperator() {
		t.Error("the operator's own words are not the operator's")
	}
	for _, other := range []domain.Provenance{
		domain.ProvenanceLocalUnknown, domain.ProvenanceExternal,
	} {
		if other.IsOperator() {
			t.Errorf("%q counts as the operator", other)
		}
		if other.Describe() == "" {
			t.Errorf("%q says nothing to somebody being asked to allow it", other)
		}
	}
	if domain.ProvenanceOperator.Describe() != "" {
		t.Error("the operator's own words are described as though they were somebody else's")
	}
}
