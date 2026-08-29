package runtime_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// watchingProvider records the model it was asked for.
//
// A fake that answers rather than a mock that asserts: what is being checked
// is what would go over the wire to a real provider, and a test that asserts
// on a call to its own code proves nothing about that.
type watchingProvider struct {
	mu       sync.Mutex
	asked    []string
	prompted []string
}

func (p *watchingProvider) Name() string { return "watching" }

func (p *watchingProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "small"}, {ID: "large"}}, nil
}

func (p *watchingProvider) Generate(
	_ context.Context,
	req provider.Request,
) (provider.Stream, error) {
	p.mu.Lock()
	p.asked = append(p.asked, req.Model)
	p.prompted = append(p.prompted, systemText(req))
	p.mu.Unlock()

	return &oneWord{}, nil
}

func (p *watchingProvider) models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.asked...)
}

// prompts is what the model was actually told, per turn.
func (p *watchingProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.prompted...)
}

func systemText(req provider.Request) string {
	var out strings.Builder
	for _, block := range req.System {
		if text, ok := block.(provider.TextBlock); ok {
			out.WriteString(text.Text)
			out.WriteString("\n")
		}
	}
	return out.String()
}

type oneWord struct{ done bool }

func (s *oneWord) Recv(context.Context) (provider.Event, error) {
	if s.done {
		return provider.Completed{StopReason: domain.StopEndTurn}, nil
	}
	s.done = true
	return provider.TextDelta{Text: "ok"}, nil
}

func (s *oneWord) Close() error { return nil }

func newModelRuntime(t *testing.T) (*runtime.Runtime, *watchingProvider) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := memory.New()
	watching := &watchingProvider{}

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	return runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      watching,
		Model:         "small",
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		NewPlanItemID: planSteps(),
		Now:           time.Now,
		Logger:        slog.New(slog.DiscardHandler),
	}), watching
}

func waitForModels(t *testing.T, watching *watchingProvider, want int) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if asked := watching.models(); len(asked) >= want {
			return asked
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d model calls after waiting, want %d", len(watching.models()), want)
	return nil
}

// A session that chose a model must actually get it. A choice recorded and
// then ignored is worse than no choice at all: everything says one model is
// answering while another one is.
func TestASessionsChoiceIsWhatTheProviderIsAsked(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "choosing")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := rt.SetSessionModel(ctx, session.ID, "large"); err != nil {
		t.Fatalf("set model: %v", err)
	}

	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	asked := waitForModels(t, watching, 1)
	if asked[0] != "large" {
		t.Errorf("the provider was asked for %q, want the session's choice", asked[0])
	}
}

// A session that never chose gets the configured one, which is what almost
// every session does.
func TestASessionThatNeverChoseGetsTheDefault(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "not choosing")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	asked := waitForModels(t, watching, 1)
	if asked[0] != "small" {
		t.Errorf("the provider was asked for %q, want the configured model", asked[0])
	}
}

// Clearing the choice goes back to the default, so a session can be undone
// without a separate call nobody would find.
func TestClearingTheChoiceGoesBackToTheDefault(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "changing its mind")
	if _, err := rt.SetSessionModel(ctx, session.ID, "large"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "one"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForModels(t, watching, 1)

	if _, err := rt.SetSessionModel(ctx, session.ID, ""); err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "two"}); err != nil {
		t.Fatalf("send again: %v", err)
	}

	asked := waitForModels(t, watching, 2)
	if asked[1] != "small" {
		t.Errorf("after clearing, the provider was asked for %q", asked[1])
	}
}

// A choice belongs to its session. One that leaked into every session would
// be a configuration change with no way back.
func TestAChoiceBelongsToItsSession(t *testing.T) {
	rt, watching := newModelRuntime(t)
	ctx := context.Background()

	chose, _ := rt.CreateSession(ctx, "chose")
	other, _ := rt.CreateSession(ctx, "did not")

	if _, err := rt.SetSessionModel(ctx, chose.ID, "large"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if _, _, err := rt.SendTurnTo(ctx, other.ID, domain.Turn{Text: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	asked := waitForModels(t, watching, 1)
	if asked[0] != "small" {
		t.Errorf("a session that never chose was asked for %q", asked[0])
	}
}

// planSteps names plan items the way the daemon does: short, countable, and
// separate from every other id, because these are shown to the model and
// typed back by it.
func planSteps() runtime.IDGenerator {
	var counter atomic.Uint64
	return func() string { return fmt.Sprintf("todo_%d", counter.Add(1)) }
}
