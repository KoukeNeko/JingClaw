package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

// These tests exercise the reason the daemon persists anything at all: a
// process that dies must not take the conversation with it, and must not leave
// clients watching a run that can never finish.

// newRestartableRuntime builds a runtime over a database that survives between
// calls, simulating a daemon restart against the same data directory.
func newRestartableRuntime(t *testing.T, dbPath string, chunkDelay time.Duration) (*runtime.Runtime, storage.Store) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      fake.New(chunkDelay),
		Model:         fake.ModelID,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		Now:           time.Now,
		Logger:        slog.New(slog.DiscardHandler),
	})

	return rt, store
}

func TestSessionAndEventsSurviveRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	first, _ := newRestartableRuntime(t, dbPath, 0)

	session, err := first.CreateSession(ctx, "durable")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := first.SendTurn(ctx, session.ID, "測試訊息", domain.RunOrigin{
		Kind:     domain.OriginLocalClient,
		ClientID: "test",
	})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, first, runID)

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// A second process opens the same database.
	second, secondStore := newRestartableRuntime(t, dbPath, 0)

	recovered, err := second.RecoverOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 0 {
		t.Errorf("recovered %d runs, want 0 after a clean shutdown", recovered)
	}

	restored, err := second.Session(ctx, session.ID)
	if err != nil {
		t.Fatalf("session did not survive restart: %v", err)
	}
	if restored.Title != "durable" {
		t.Errorf("title %q, want %q", restored.Title, "durable")
	}

	run, err := second.Run(ctx, runID)
	if err != nil {
		t.Fatalf("run did not survive restart: %v", err)
	}
	if run.Status != domain.RunCompleted {
		t.Errorf("status %q, want %q", run.Status, domain.RunCompleted)
	}

	// The whole point of resume: a client comes back with the sequence it last
	// applied and picks up from there.
	events, err := secondStore.ListAfter(ctx, session.ID, 3, 0)
	if err != nil {
		t.Fatalf("list events after restart: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events survived the restart")
	}
	if events[0].Seq != 4 {
		t.Errorf("resume started at seq %d, want 4", events[0].Seq)
	}

	// New turns continue the same sequence rather than restarting it.
	nextRun, _, err := second.SendTurn(ctx, session.ID, "第二則", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn after restart: %v", err)
	}
	waitForRun(t, second, nextRun)

	all, err := secondStore.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list all events: %v", err)
	}
	for i, ev := range all {
		if want := domain.Seq(i + 1); ev.Seq != want {
			t.Fatalf("event %d has seq %d, want %d; the sequence restarted", i, ev.Seq, want)
		}
	}
}

// A crash leaves runs marked running with nothing driving them. Startup must
// resolve them, or every client sits on a spinner forever.
func TestRecoverResolvesOrphanedRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	// A slow provider guarantees the run is still live when the process
	// "dies" without a graceful shutdown.
	first, _ := newRestartableRuntime(t, dbPath, time.Hour)

	session, err := first.CreateSession(ctx, "crashed")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := first.SendTurn(ctx, session.ID, "will be orphaned", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}

	waitFor(t, func() bool {
		run, err := first.Run(ctx, runID)
		return err == nil && run.Status == domain.RunRunning
	})

	// Deliberately no Shutdown: this models a kill -9.

	second, secondStore := newRestartableRuntime(t, dbPath, 0)

	recovered, err := second.RecoverOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered %d runs, want 1", recovered)
	}

	run, err := second.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunFailed {
		t.Errorf("status %q, want %q; an orphan was left live", run.Status, domain.RunFailed)
	}
	if run.FinishedAt == nil {
		t.Error("recovered run has no finish time")
	}

	// A client resuming from its cursor must learn the outcome from the log,
	// not by polling the run.
	events, err := secondStore.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	last := events[len(events)-1]
	changed, ok := last.Payload.(domain.RunStateChanged)
	if !ok {
		t.Fatalf("last event is %T, want RunStateChanged", last.Payload)
	}
	if changed.Status != domain.RunFailed {
		t.Errorf("terminal event records %q, want %q", changed.Status, domain.RunFailed)
	}
	if changed.Reason == "" {
		t.Error("recovery did not record why the run failed")
	}
}

// Recovery must be safe to run repeatedly: a daemon restarted twice should not
// append a second terminal event to an already-resolved run.
func TestRecoverIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	first, firstStore := newRestartableRuntime(t, dbPath, time.Hour)
	session, err := first.CreateSession(ctx, "crashed twice")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := first.SendTurn(ctx, session.ID, "orphan", domain.RunOrigin{}); err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitFor(t, func() bool {
		runs, err := firstStore.ListAfter(ctx, session.ID, 0, 0)
		return err == nil && len(runs) >= 2
	})

	second, secondStore := newRestartableRuntime(t, dbPath, 0)
	if _, err := second.RecoverOrphanedRuns(ctx); err != nil {
		t.Fatalf("first recover: %v", err)
	}

	before, err := secondStore.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	recovered, err := second.RecoverOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if recovered != 0 {
		t.Errorf("second recovery touched %d runs, want 0", recovered)
	}

	after, err := secondStore.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("second recovery appended %d events, want 0", len(after)-len(before))
	}
}

// Interrupting a run this process is not driving must not look like success.
func TestInterruptUnknownRunFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()

	rt, _ := newRestartableRuntime(t, dbPath, 0)

	if _, err := rt.InterruptRun(ctx, "run_missing", "stop"); err == nil {
		t.Fatal("interrupting an unknown run reported success")
	}
}

func TestStorageErrorsSurfaceFromSendTurn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	rt, _ := newRestartableRuntime(t, dbPath, 0)

	_, _, err := rt.SendTurn(context.Background(), "ses_missing", "hello", domain.RunOrigin{})
	if err == nil {
		t.Fatal("sending to an unknown session reported success")
	}
	if !errors.Is(err, storage.ErrSessionNotFound) {
		t.Errorf("got %v, want ErrSessionNotFound", err)
	}
}
