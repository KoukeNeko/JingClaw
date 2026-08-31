package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// searchTurns is a run that greps its way through a workspace.
//
// Four calls per turn, because the harness allows five model turns and a
// search of any length has to fit inside them — which is also what a model
// really does: the traces this was built from show several greps asked for at
// once.
func searchTurns(count int) [][]provider.Event {
	const perTurn = 4

	var turns [][]provider.Event
	for asked := 0; asked < count; asked += perTurn {
		var turn []provider.Event
		for i := asked; i < min(asked+perTurn, count); i++ {
			turn = append(turn, toolCall(
				fmt.Sprintf("call_%d", i), "grep", map[string]any{"query": "x"}))
		}
		turns = append(turns, turn)
	}
	return append(turns, []provider.Event{
		provider.TextDelta{Text: "Found it."},
		provider.Completed{StopReason: domain.StopEndTurn},
	})
}

// toolResults is every result this run was handed.
func toolResults(t *testing.T, store *memory.Store, session domain.SessionID) []domain.ToolCallCompleted {
	t.Helper()

	events, err := store.ListAfter(context.Background(), session, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var results []domain.ToolCallCompleted
	for _, event := range events {
		if done, ok := event.Payload.(domain.ToolCallCompleted); ok {
			results = append(results, done)
		}
	}
	return results
}

// TestALongSearchIsToldWhatItHasSpent is the one thing a model cannot see for
// itself.
//
// It asks for a grep, reads a file, asks for another, and nothing anywhere
// tells it this has become a long search — so the tool that exists to make
// long searches cheap is one it never has a reason to reach for. Four
// different ways of saying "use investigate" in the prompt changed nothing,
// because none of them was news at the moment it mattered.
func TestALongSearchIsToldWhatItHasSpent(t *testing.T) {
	rt, store, _, registry := newToolHarness(t, searchTurns(9))
	registry.MustRegister(investigateFor(rt))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "searching")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "where is everything")

	var told int
	for _, result := range toolResults(t, store, session.ID) {
		if strings.Contains(result.Content, "searches so far this turn") {
			told++
		}
	}

	// Once. Said on every result from the threshold onwards it would be
	// nagging, and a model that has decided not to delegate would be told to
	// again on every call for the rest of the run.
	if told != 1 {
		t.Errorf("the run was told %d times, want once", told)
	}
}

// TestAShortSearchIsNotInterrupted keeps this from arriving during an
// ordinary lookup.
func TestAShortSearchIsNotInterrupted(t *testing.T) {
	rt, store, _, registry := newToolHarness(t, searchTurns(2))
	registry.MustRegister(investigateFor(rt))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "brief")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "where is one thing")

	for _, result := range toolResults(t, store, session.ID) {
		if strings.Contains(result.Content, "searches so far this turn") {
			t.Errorf("a two-call lookup was interrupted: %q", result.Content)
		}
	}
}

// TestAWorkerIsNotToldToDelegate covers the run that is already the answer.
//
// A delegated search cannot delegate again, so suggesting it would be
// suggesting the one thing it is refused.
func TestAWorkerIsNotToldToDelegate(t *testing.T) {
	turns := [][]provider.Event{
		{toolCall("call_0", "investigate", map[string]any{"question": "where is everything"})},
	}
	turns = append(turns, searchTurns(9)...) // the worker's own searching
	turns = append(turns, []provider.Event{
		provider.TextDelta{Text: "Right."}, provider.Completed{StopReason: domain.StopEndTurn},
	})

	rt, store, _, registry := newToolHarness(t, turns)
	registry.MustRegister(investigateFor(rt))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "worker")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "find out")

	runs, err := store.ListRuns(ctx, session.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	var worker domain.RunID
	for _, run := range runs {
		if run.Kind == domain.RunWorker {
			worker = run.ID
		}
	}
	if worker == "" {
		t.Fatal("no worker ran, so this proves nothing")
	}

	// The precondition: the worker searched far enough that it would have
	// been told, had it been anything else. Without this the check below
	// passes against a worker that made two calls.
	var searched int

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events {
		if event.RunID != worker {
			continue
		}
		if done, ok := event.Payload.(domain.ToolCallCompleted); ok {
			if done.Name == "grep" {
				searched++
			}
			if strings.Contains(done.Content, "searches so far this turn") {
				t.Error("a worker was told to delegate, which it cannot do")
			}
		}
	}

	if searched < 7 {
		t.Fatalf("the worker made %d searches, too few to have been told", searched)
	}
}

// investigateFor is the delegating tool, wired to this runtime.
func investigateFor(rt *runtime.Runtime) tool.Tool {
	return &builtin.Investigate{Delegator: rt}
}

// TestTheCountIsTheTruth catches an off-by-one that is invisible in the
// behaviour and wrong in the sentence.
//
// The notice is written before the call it is attached to has been recorded,
// so counting only the log makes the threshold mean one more than it says and
// the number it reports one less than what happened.
func TestTheCountIsTheTruth(t *testing.T) {
	rt, store, _, registry := newToolHarness(t, searchTurns(8))
	registry.MustRegister(investigateFor(rt))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "counting")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "look everywhere")

	// Which result carries it, and what it claims.
	var searched, carried int
	var said string
	for _, result := range toolResults(t, store, session.ID) {
		if searching(result.Name) {
			searched++
		}
		if strings.Contains(result.Content, "searches so far this turn") {
			carried, said = searched, result.Content
		}
	}

	if said == "" {
		t.Fatalf("nothing was told, after %d searches", searched)
	}
	if !strings.Contains(said, fmt.Sprintf("%d searches", carried)) {
		t.Errorf("it carries the %dth result and says something else: %q", carried, said)
	}
}

// searching mirrors the runtime's own list, for counting in a test.
func searching(name string) bool {
	switch name {
	case "grep", "glob_files", "read_file":
		return true
	}
	return false
}
