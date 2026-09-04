package runtime_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// gatedProvider answers only when told to, one call at a time, and counts
// what it was asked. It is how a test holds one run open while another
// arrives.
type gatedProvider struct {
	release chan struct{}
	calls   atomic.Int32
}

func (p *gatedProvider) Name() string { return "gated" }
func (p *gatedProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "gated"}}, nil
}
func (p *gatedProvider) Generate(ctx context.Context, _ provider.Request) (provider.Stream, error) {
	p.calls.Add(1)
	select {
	case <-p.release:
		return &oneLine{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type oneLine struct{ done bool }

func (s *oneLine) Recv(context.Context) (provider.Event, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return provider.Completed{StopReason: domain.StopEndTurn}, nil
}
func (s *oneLine) Close() error { return nil }

func newQueueRuntime(t *testing.T, model provider.Provider) (*runtime.Runtime, *memory.Store) {
	t.Helper()
	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return prefix + "_" + itoa(counter.Add(1)) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := memory.New()
	rt := runtime.New(ctx, runtime.Options{
		Store: store, Hub: event.NewHub(), Provider: model, Model: "gated",
		Tools: tool.NewRegistry(), MaxIterations: 3,
		NewSessionID: next("ses"), NewRunID: next("run"), NewMessageID: next("msg"),
		NewEventID: next("evt"), NewApprovalID: next("apr"), NewPlanItemID: next("todo"),
		NewQuestionID: next("qst"), NewScheduleID: next("sch"),
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
	})
	return rt, store
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func statusOf(t *testing.T, store *memory.Store, id domain.RunID) domain.RunStatus {
	t.Helper()
	run, err := store.Run(context.Background(), id)
	if err != nil {
		t.Fatalf("run %s: %v", id, err)
	}
	return run.Status
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never happened: %s", what)
}

// One run at a time per session. A second message while the first is being
// answered waits, says so in the log, and starts the moment the first is
// done — in the order the messages arrived.
func TestASessionAnswersOneMessageAtATime(t *testing.T) {
	model := &gatedProvider{release: make(chan struct{})}
	rt, store := newQueueRuntime(t, model)
	ctx := context.Background()
	session, _ := rt.CreateSession(ctx, "busy channel")

	first, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first run reached the model", func() bool { return model.calls.Load() == 1 })

	second, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := model.calls.Load(); got != 1 {
		t.Fatalf("the second message reached the model while the first was still being answered (%d calls)", got)
	}
	if got := statusOf(t, store, second); got != domain.RunQueued {
		t.Errorf("the waiting run is %s, want queued", got)
	}
	queuedSaid := false
	events, _ := store.ListAfter(ctx, session.ID, 0, 0)
	for _, ev := range events {
		if changed, ok := ev.Payload.(domain.RunStateChanged); ok && ev.RunID == second && changed.Status == domain.RunQueued {
			queuedSaid = true
		}
	}
	if !queuedSaid {
		t.Error("the log does not say the second run had to wait")
	}

	model.release <- struct{}{}
	waitForRun(t, rt, first)
	eventually(t, "the second run reached the model after the first finished", func() bool { return model.calls.Load() == 2 })
	model.release <- struct{}{}
	waitForRun(t, rt, second)
	if got := statusOf(t, store, second); got != domain.RunCompleted {
		t.Errorf("the second run ended as %s", got)
	}
}

// Two sessions do not wait for each other.
func TestSessionsDoNotWaitForEachOther(t *testing.T) {
	model := &gatedProvider{release: make(chan struct{})}
	rt, _ := newQueueRuntime(t, model)
	ctx := context.Background()
	a, _ := rt.CreateSession(ctx, "a")
	b, _ := rt.CreateSession(ctx, "b")

	if _, _, err := rt.SendTurnTo(ctx, a.ID, domain.Turn{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.SendTurnTo(ctx, b.ID, domain.Turn{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both sessions reached the model at once", func() bool { return model.calls.Load() == 2 })
	model.release <- struct{}{}
	model.release <- struct{}{}
}

// A run interrupted while it waits is cancelled where it stands, and the one
// behind it is not held up by it.
func TestAWaitingRunCanBeInterrupted(t *testing.T) {
	model := &gatedProvider{release: make(chan struct{})}
	rt, store := newQueueRuntime(t, model)
	ctx := context.Background()
	session, _ := rt.CreateSession(ctx, "changed my mind")

	first, _, _ := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "one"})
	eventually(t, "the first run reached the model", func() bool { return model.calls.Load() == 1 })
	second, _, _ := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "two"})
	third, _, _ := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "three"})

	if status, err := rt.InterruptRun(ctx, second, "never mind"); err != nil || status != domain.RunCancelled {
		t.Fatalf("interrupting a waiting run: status %s, err %v", status, err)
	}
	if got := statusOf(t, store, second); got != domain.RunCancelled {
		t.Errorf("the interrupted run is %s", got)
	}

	model.release <- struct{}{}
	waitForRun(t, rt, first)
	eventually(t, "the third run took its turn", func() bool { return model.calls.Load() == 2 })
	model.release <- struct{}{}
	waitForRun(t, rt, third)
	if got := statusOf(t, store, third); got != domain.RunCompleted {
		t.Errorf("the run behind the cancelled one ended as %s", got)
	}
}
