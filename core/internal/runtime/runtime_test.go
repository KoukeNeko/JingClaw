package runtime_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// A fixed clock and counter-based IDs keep every assertion below exact. The
// whole point of the fake provider is that the walking skeleton has no
// nondeterminism left to explain away.
func newHarness(t *testing.T, modelProvider provider.Provider) (*runtime.Runtime, *memory.Store, *event.Hub) {
	t.Helper()

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	store := memory.New()
	hub := event.NewHub()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           hub,
		Provider:      modelProvider,
		Model:         fake.ModelID,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: next("todo"),
		Now:           clock,
		Logger:        slog.New(slog.DiscardHandler),
	})

	return rt, store, hub
}

func waitForRun(t *testing.T, rt *runtime.Runtime, id domain.RunID) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rt.Wait(ctx, id); err != nil {
		t.Fatalf("waiting for run %s: %v", id, err)
	}
}

func TestSendTurnProducesOrderedEventLog(t *testing.T) {
	rt, store, _ := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{
		Kind:     domain.OriginLocalClient,
		ClientID: "test",
	})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	want := []domain.EventKind{
		domain.EventUserMessageAdded,
		domain.EventRunStateChanged, // running
		// The provider's two chunks coalesce into one persisted delta.
		domain.EventAssistantTextDelta,
		domain.EventUsageChanged,
		domain.EventAssistantMessageCompleted,
		domain.EventRunStateChanged, // completed
	}

	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, kind := range want {
		if events[i].Kind != kind {
			t.Errorf("event %d: got kind %q, want %q", i, events[i].Kind, kind)
		}
	}
}

// Sequence numbers are the contract every client resumes against, so a gap or
// a repeat would silently corrupt every materialized view.
func TestEventSeqIsDenseAndMonotonic(t *testing.T) {
	rt, store, _ := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	for i := range 3 {
		runID, _, err := rt.SendTurn(ctx, session.ID, fmt.Sprintf("turn %d", i), domain.RunOrigin{})
		if err != nil {
			t.Fatalf("send turn %d: %v", i, err)
		}
		waitForRun(t, rt, runID)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	for i, ev := range events {
		if want := domain.Seq(i + 1); ev.Seq != want {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, want)
		}
	}
}

// The resume contract: ask for everything after N, get exactly that.
func TestListAfterReturnsOnlyLaterEvents(t *testing.T) {
	rt, store, _ := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	all, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}

	const after = domain.Seq(3)
	tail, err := store.ListAfter(ctx, session.ID, after, 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}

	if want := len(all) - int(after); len(tail) != want {
		t.Fatalf("got %d events after seq %d, want %d", len(tail), after, want)
	}
	for _, ev := range tail {
		if ev.Seq <= after {
			t.Fatalf("event with seq %d leaked into results after seq %d", ev.Seq, after)
		}
	}
}

// A run must reach a terminal state and record why, even when interrupted
// mid-generation. Losing the terminal event would leave clients showing a run
// that never ends.
func TestInterruptMidStreamCancelsAndRecordsTerminalState(t *testing.T) {
	rt, store, _ := newHarness(t, fake.New(2*time.Second))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}

	// Wait until the run is actually generating, so the interrupt arrives
	// mid-stream rather than before the provider was even called.
	waitFor(t, func() bool {
		run, err := rt.Run(ctx, runID)
		return err == nil && run.Status == domain.RunRunning
	})

	if _, err := rt.InterruptRun(ctx, runID, "user pressed stop"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	waitForRun(t, rt, runID)

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunCancelled {
		t.Errorf("got status %q, want %q", run.Status, domain.RunCancelled)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	last := events[len(events)-1]
	changed, isStateChange := last.Payload.(domain.RunStateChanged)
	if !isStateChange {
		t.Fatalf("last event is %T, want RunStateChanged", last.Payload)
	}
	if changed.Status != domain.RunCancelled {
		t.Errorf("last event records status %q, want %q", changed.Status, domain.RunCancelled)
	}

	// Coalescing buffers deltas, so an interrupt must still flush what the
	// model already produced. Losing it would be worse than not batching.
	var sawText bool
	for _, ev := range events {
		if ev.Kind == domain.EventAssistantTextDelta {
			sawText = true
		}
	}
	if !sawText {
		t.Error("interrupting discarded the partial output that had already been generated")
	}
}

// Interrupting a finished run is a normal race — a user clicking stop as the
// answer arrives — and must not be an error.
func TestInterruptAfterCompletionIsNoOp(t *testing.T) {
	rt, _, _ := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	status, err := rt.InterruptRun(ctx, runID, "too late")
	if err != nil {
		t.Fatalf("interrupt completed run: %v", err)
	}
	if status != domain.RunCompleted {
		t.Errorf("got status %q, want %q", status, domain.RunCompleted)
	}
}

// A provider failure is a recorded outcome, not a crash.
func TestProviderErrorFailsRunWithoutKillingRuntime(t *testing.T) {
	failing := provider.Func(func(context.Context, provider.Request) (provider.Stream, error) {
		return nil, io.ErrUnexpectedEOF
	})

	rt, _, _ := newHarness(t, failing)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	waitForRun(t, rt, runID)

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != domain.RunFailed {
		t.Errorf("got status %q, want %q", run.Status, domain.RunFailed)
	}

	// The runtime must still accept work.
	if _, _, err := rt.SendTurn(ctx, session.ID, "again", domain.RunOrigin{}); err != nil {
		t.Errorf("runtime rejected work after a failed run: %v", err)
	}
}

func TestSendTurnToUnknownSessionFails(t *testing.T) {
	rt, _, _ := newHarness(t, fake.New(0))

	_, _, err := rt.SendTurn(context.Background(), "ses_nope", "hello", domain.RunOrigin{})
	if err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

// Shutdown must interrupt outstanding work and wait for it, rather than
// leaving goroutines behind or exiting mid-write.
func TestShutdownDrainsActiveRuns(t *testing.T) {
	rt, _, _ := newHarness(t, fake.New(2*time.Second))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := rt.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	run, err := rt.Run(ctx, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !run.Status.IsTerminal() {
		t.Errorf("run left in non-terminal state %q after shutdown", run.Status)
	}

	if _, err := rt.CreateSession(ctx, "after"); err == nil {
		t.Error("runtime accepted a new session while draining")
	}
}

// The hub carries wake-ups, not events, precisely so that a subscriber which
// never reads cannot hold up the runtime.
func TestSlowSubscriberDoesNotBlockRuntime(t *testing.T) {
	rt, _, hub := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Subscribe and deliberately never drain the channel.
	sub := hub.Subscribe(session.ID)
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runID, err := func() (domain.RunID, error) {
			id, _, err := rt.SendTurn(ctx, session.ID, "hello", domain.RunOrigin{})
			return id, err
		}()
		if err != nil {
			t.Errorf("send turn: %v", err)
			return
		}
		waitForRun(t, rt, runID)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime blocked on a subscriber that never reads")
	}
}

func TestConcurrentTurnsKeepSequenceConsistent(t *testing.T) {
	rt, store, _ := newHarness(t, fake.New(0))
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const turns = 8
	var wg sync.WaitGroup
	ids := make([]domain.RunID, turns)

	for i := range turns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := rt.SendTurn(ctx, session.ID, fmt.Sprintf("turn %d", i), domain.RunOrigin{})
			if err != nil {
				t.Errorf("send turn %d: %v", i, err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if id != "" {
			waitForRun(t, rt, id)
		}
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i, ev := range events {
		if want := domain.Seq(i + 1); ev.Seq != want {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// A wiring mistake must stop the daemon at startup. Finding it through a nil
// call halfway through a run crashes in the middle of someone's work, and
// points the stack trace at the symptom rather than the cause.
func TestNewPanicsOnMissingCollaborators(t *testing.T) {
	complete := func() runtime.Options {
		return runtime.Options{
			Store:         memory.New(),
			Hub:           event.NewHub(),
			Provider:      fake.New(0),
			NewSessionID:  func() string { return "ses" },
			NewRunID:      func() string { return "run" },
			NewMessageID:  func() string { return "msg" },
			NewEventID:    func() string { return "evt" },
			NewApprovalID: func() string { return "apr" },
			NewPlanItemID: func() string { return "apr" },
			Logger:        slog.New(slog.DiscardHandler),
		}
	}

	clear := map[string]func(*runtime.Options){
		"Store":         func(o *runtime.Options) { o.Store = nil },
		"Hub":           func(o *runtime.Options) { o.Hub = nil },
		"Provider":      func(o *runtime.Options) { o.Provider = nil },
		"NewSessionID":  func(o *runtime.Options) { o.NewSessionID = nil },
		"NewRunID":      func(o *runtime.Options) { o.NewRunID = nil },
		"NewMessageID":  func(o *runtime.Options) { o.NewMessageID = nil },
		"NewEventID":    func(o *runtime.Options) { o.NewEventID = nil },
		"NewApprovalID": func(o *runtime.Options) { o.NewApprovalID = nil },
	}

	for name, drop := range clear {
		t.Run(name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("New accepted Options with no %s", name)
				}
				if message, ok := recovered.(string); ok && !strings.Contains(message, name) {
					t.Errorf("the panic does not name the missing option: %s", message)
				}
			}()

			opts := complete()
			drop(&opts)
			runtime.New(context.Background(), opts)
		})
	}
}
