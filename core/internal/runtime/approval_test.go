package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// These cases cover the gate that stands in front of anything that can change
// the workspace. The property that matters most is that a pause is durable:
// waiting for a human is a legitimate state, not a stalled run to clean up.

func newGatedHarness(t *testing.T, turns [][]provider.Event) (*runtime.Runtime, *memory.Store, *scriptedProvider) {
	t.Helper()

	rt, store, scripted := newToolHarness(t, turns)
	rt.SetPermissions(permission.New(permission.LocalProfile()))

	return rt, store, scripted
}

func onlyPendingApproval(t *testing.T, rt *runtime.Runtime, session domain.SessionID) domain.Approval {
	t.Helper()

	pending, err := rt.PendingApprovals(context.Background(), session)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending approvals, want 1", len(pending))
	}
	return pending[0]
}

func TestWriteSuspendsTheRunUntilApproved(t *testing.T) {
	rt, store, _ := newGatedHarness(t, [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{
			"path": "src/new.go", "content": "package main\n",
		})},
		{provider.TextDelta{Text: "Wrote it."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "gated")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Create a file", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	// The run has parked rather than finished. Nothing has been written.
	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunAwaitingApproval {
		t.Fatalf("status %q, want %q", run.Status, domain.RunAwaitingApproval)
	}

	approval := onlyPendingApproval(t, rt, session.ID)
	if approval.ToolName != "write_file" {
		t.Errorf("approval is for %q", approval.ToolName)
	}
	// A reviewer needs the real arguments, not a description of them.
	if !strings.Contains(approval.Arguments, "src/new.go") {
		t.Errorf("approval does not carry the arguments: %s", approval.Arguments)
	}
	if len(approval.Effects) == 0 {
		t.Error("approval states no effects, so a reviewer has nothing to judge")
	}

	// No result was recorded, because the tool never ran.
	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == domain.EventToolCallCompleted {
			t.Fatal("the tool ran before anyone approved it")
		}
	}

	if _, err := rt.DecideApproval(ctx, approval.ID, true, domain.RememberOnce, domain.FromTheMachine("test")); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitForRun(t, rt, runID)

	run, err = rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Errorf("status %q after approval, want %q", run.Status, domain.RunCompleted)
	}
}

// A refusal has to read to the model as a decision, not a malfunction, or it
// will treat the call as flaky and try again.
func TestDenialIsReportedToTheModelAsFinal(t *testing.T) {
	rt, store, scripted := newGatedHarness(t, [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{
			"path": "src/new.go", "content": "package main\n",
		})},
		{provider.TextDelta{Text: "Understood, I will not write it."},
			provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "denied")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Create a file", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	approval := onlyPendingApproval(t, rt, session.ID)
	if _, err := rt.DecideApproval(ctx, approval.ID, false, domain.RememberOnce, domain.FromTheMachine("test")); err != nil {
		t.Fatalf("deny: %v", err)
	}
	waitForRun(t, rt, runID)

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var denial domain.ToolCallCompleted
	for _, ev := range events {
		if completed, ok := ev.Payload.(domain.ToolCallCompleted); ok {
			denial = completed
		}
	}
	if !denial.IsError {
		t.Fatal("the denial was not recorded as a failed call")
	}
	if !strings.Contains(denial.Content, "permission_denied") {
		t.Errorf("the model cannot tell this was a decision: %s", denial.Content)
	}
	if !strings.Contains(denial.Content, "Do not retry") {
		t.Errorf("the model is not told to stop retrying: %s", denial.Content)
	}

	// And it kept going rather than treating the refusal as fatal.
	if len(scripted.requests) != 2 {
		t.Errorf("provider called %d times, want 2", len(scripted.requests))
	}
}

// Two clients answering the same prompt must not run the tool twice.
func TestSecondDecisionIsRejected(t *testing.T) {
	rt, _, _ := newGatedHarness(t, [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{
			"path": "src/new.go", "content": "package main\n",
		})},
		{provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "race")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Create a file", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	approval := onlyPendingApproval(t, rt, session.ID)

	if _, err := rt.DecideApproval(ctx, approval.ID, true, domain.RememberOnce, domain.FromTheMachine("first")); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	_, err = rt.DecideApproval(ctx, approval.ID, true, domain.RememberOnce, domain.FromTheMachine("second"))
	if !errors.Is(err, runtime.ErrApprovalNotPending) {
		t.Fatalf("second decision returned %v, want ErrApprovalNotPending", err)
	}
}

// Approving for the session means the next call of the same tool runs
// unattended; it must not silently unlock anything else.
func TestSessionScopeSkipsLaterPromptsForThatToolOnly(t *testing.T) {
	rt, _, _ := newGatedHarness(t, [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{
			"path": "src/a.go", "content": "package main\n",
		})},
		{toolCall("call_2", "write_file", map[string]any{
			"path": "src/b.go", "content": "package main\n",
		})},
		{provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "scoped")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Create two files", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	approval := onlyPendingApproval(t, rt, session.ID)
	if _, err := rt.DecideApproval(ctx, approval.ID, true, domain.RememberSession, domain.FromTheMachine("test")); err != nil {
		t.Fatalf("approve for session: %v", err)
	}
	waitForRun(t, rt, runID)

	// The second write went through without asking again.
	pending, err := rt.PendingApprovals(ctx, session.ID)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("still waiting on %d approvals after a session-scoped grant", len(pending))
	}

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Errorf("status %q, want %q", run.Status, domain.RunCompleted)
	}
}

// Read-only work must not stop to ask. An agent that prompts before every file
// it looks at is unusable.
func TestReadsAreNotGated(t *testing.T) {
	rt, _, _ := newGatedHarness(t, [][]provider.Event{
		{toolCall("call_1", "grep", map[string]any{"query": "main"})},
		{provider.TextDelta{Text: "Found it."}, provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "reads")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Find main", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	pending, err := rt.PendingApprovals(ctx, session.ID)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a read-only tool asked for approval")
	}

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Errorf("status %q, want %q", run.Status, domain.RunCompleted)
	}
}

// Later calls in the same turn stay outstanding while one is under review:
// they may depend on it, and running them first would act on an assumption the
// human has not confirmed.
func TestLaterCallsWaitForTheOneUnderReview(t *testing.T) {
	rt, store, _ := newGatedHarness(t, [][]provider.Event{
		{
			toolCall("call_1", "write_file", map[string]any{"path": "src/a.go", "content": "package main\n"}),
			toolCall("call_2", "grep", map[string]any{"query": "main"}),
		},
		{provider.Completed{StopReason: domain.StopEndTurn}},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "ordering")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "Write then search", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == domain.EventToolCallCompleted {
			t.Fatal("a later call ran while an earlier one was still under review")
		}
	}
}

// The point of persisting an approval is that the answer may arrive long
// after the process that asked for it has gone. A pause is a legitimate state,
// not a stalled run for startup to clean up.
func TestApprovalSurvivesADaemonRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	turns := [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{
			"path": "notes.md", "content": "written after approval\n",
		})},
		{provider.TextDelta{Text: "Done."}, provider.Completed{StopReason: domain.StopEndTurn}},
	}

	first, workspaceRoot := newPersistentGatedRuntime(t, dbPath, "", "a", turns)

	session, err := first.CreateSession(ctx, "restart")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := first.SendTurn(ctx, session.ID, "Write notes", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, first, runID)

	approval := onlyPendingApproval(t, first, session.ID)

	// The process goes away without anyone answering.
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// The replacement process picks up mid-conversation: the write has already
	// been requested, so its script starts at the turn that follows it.
	second, _ := newPersistentGatedRuntime(t, dbPath, workspaceRoot, "b", turns[1:])

	// Startup must leave the paused run alone rather than failing it.
	recovered, err := second.RecoverOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 0 {
		t.Errorf("startup resolved %d runs; a run waiting on a human is not an orphan", recovered)
	}

	run, err := second.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunAwaitingApproval {
		t.Fatalf("status %q after restart, want %q", run.Status, domain.RunAwaitingApproval)
	}

	// The prompt is still answerable, by a client that never saw the original.
	stillPending := onlyPendingApproval(t, second, session.ID)
	if stillPending.ID != approval.ID {
		t.Errorf("a different approval survived: %s then %s", approval.ID, stillPending.ID)
	}

	if _, err := second.DecideApproval(ctx, stillPending.ID, true, domain.RememberOnce, domain.FromTheMachine("later")); err != nil {
		t.Fatalf("approve after restart: %v", err)
	}
	waitForRun(t, second, runID)

	run, err = second.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("status %q, want %q", run.Status, domain.RunCompleted)
	}

	// And the work the human authorised actually happened.
	written, err := os.ReadFile(filepath.Join(workspaceRoot, "notes.md"))
	if err != nil {
		t.Fatalf("the approved write never ran: %v", err)
	}
	if string(written) != "written after approval\n" {
		t.Errorf("file contains %q", string(written))
	}
}

// newPersistentGatedRuntime builds a gated runtime over a database and
// workspace that outlive it, so a restart can be simulated. An empty
// workspaceRoot creates a fresh one.
func newPersistentGatedRuntime(
	t *testing.T,
	dbPath, workspaceRoot, instance string,
	turns [][]provider.Event,
) (*runtime.Runtime, string) {
	t.Helper()

	if workspaceRoot == "" {
		workspaceRoot = t.TempDir()
	}

	ws, err := workspace.Open(workspaceRoot)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()
	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		&builtin.GlobFiles{Workspace: ws},
		&builtin.Grep{Workspace: ws},
		builtin.NewWriteFile(ws, observed, locks),
		builtin.NewEditFile(ws, observed, locks),
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Namespaced by instance so a restarted process cannot reuse an identifier
	// the previous one already wrote, the way real ULIDs never would.
	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%s%d", prefix, instance, counter.Add(1)) }
	}

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      &scriptedProvider{turns: turns},
		Model:         "scripted",
		Tools:         registry,
		Permissions:   permission.New(permission.LocalProfile()),
		MaxIterations: 5,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: next("todo"),
		NewQuestionID: next("qst"),
		Now:           time.Now,
		Logger:        slog.New(slog.DiscardHandler),
	})

	return rt, workspaceRoot
}
