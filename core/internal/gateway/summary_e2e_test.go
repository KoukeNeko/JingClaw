package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// A model that calls a tool and then answers, so a run with something to
// account for can be driven without a real provider deciding otherwise.
type summaryProvider struct {
	turns [][]provider.Event
	next  int
}

func (p *summaryProvider) Name() string { return "scripted" }

func (p *summaryProvider) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func (p *summaryProvider) Generate(context.Context, provider.Request) (provider.Stream, error) {
	if p.next >= len(p.turns) {
		return nil, fmt.Errorf("ran out of turns after %d calls", p.next)
	}
	events := p.turns[p.next]
	p.next++
	return &summaryStream{events: events}, nil
}

type summaryStream struct {
	events []provider.Event
	index  int
}

func (s *summaryStream) Recv(context.Context) (provider.Event, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	next := s.events[s.index]
	s.index++
	return next, nil
}

func (s *summaryStream) Close() error { return nil }

// summaryHarness is the projector test harness driven by a scripted model.
func newSummaryHarness(t *testing.T, turns [][]provider.Event) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("what the file says"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	// The same tools a deployment has, so a test that expects a permission
	// decision gets one rather than a tool that was never registered.
	observed := builtin.NewObserver()
	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		builtin.NewWriteFile(ws, observed, builtin.NewFileLocks()),
		&builtin.ExecCommand{Workspace: ws},
	)

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	permissions := permission.New(permission.LocalProfile())
	projector := gateway.NewProjector(store,
		func() string { return fmt.Sprintf("dsp_%d", counter.Add(1)) },
		time.Now)

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      &summaryProvider{turns: turns},
		Model:         "scripted",
		Tools:         registry,
		Permissions:   permissions,
		Delivery:      projector,
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

	return &harness{
		ingress: &gateway.Ingress{
			Store:         store,
			Runtime:       rt,
			Binder:        permissions,
			Console:       rt,
			NewDispatchID: func() string { return fmt.Sprintf("dsp_%d", counter.Add(1)) },
			Now:           func() time.Time { return time.Unix(0, 0).UTC() },
			Logger:        slog.New(slog.DiscardHandler),
		},
		store:   store,
		runtime: rt,
	}
}

// The whole path, through the real runtime and a real store: a run that reads
// a file has to arrive in the channel accounted for.
//
// The accumulator is unit-tested on its own, but that proves nothing about
// whether the events it needs ever reach it, or whether the usage report lands
// before the run says it is finished. Both are ordering questions that only a
// real run answers.
func TestAFinishedRunAccountsForItselfInTheChannel(t *testing.T) {
	arguments, err := json.Marshal(map[string]any{"path": "notes.md"})
	if err != nil {
		t.Fatal(err)
	}

	h := newSummaryHarness(t, [][]provider.Event{
		{
			provider.ToolCallRequested{ID: "call_1", Name: "read_file", Args: arguments},
			provider.UsageDelta{Usage: domain.Usage{InputTokens: 1200, OutputTokens: 40}},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "The note says something."},
			provider.UsageDelta{Usage: domain.Usage{InputTokens: 1500, OutputTokens: 90}},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})
	h.bind(t, "local", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "what does the note say", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	summary := finalSummary(t, h)
	if summary == nil {
		t.Fatal("the run ended without accounting for itself")
	}

	if len(summary.Tools) != 1 || summary.Tools[0].Name != "read_file" || summary.Tools[0].Calls != 1 {
		t.Errorf("tools: %+v", summary.Tools)
	}
	if len(summary.Sources) != 1 || summary.Sources[0].Ref != "notes.md" {
		t.Errorf("sources: %+v", summary.Sources)
	}
	if !summary.Sources[0].Retained {
		t.Error("a source read in a run that never compacted is reported as folded away")
	}

	// The ordering question: usage is reported by the provider mid-run, and
	// the summary is built when the run ends. Cumulative totals are what
	// should arrive, not the first turn's.
	if summary.InputTokens != 1500 || summary.OutputTokens != 90 {
		t.Errorf("usage is %d in / %d out, want the cumulative totals",
			summary.InputTokens, summary.OutputTokens)
	}
	if summary.Partial {
		t.Error("a run observed from its start reports itself partial")
	}
}

// finalSummary digs the summary out of the last status a channel would see.
func finalSummary(t *testing.T, h *harness) *gateway.RunSummary {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, dispatch := range h.dispatches(t) {
			if dispatch.Kind != gateway.DispatchStatus {
				continue
			}
			var payload gateway.StatusPayload
			if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if payload.State == "completed" {
				return payload.Summary
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	var seen []string
	for _, dispatch := range h.dispatches(t) {
		seen = append(seen, string(dispatch.Kind)+":"+dispatch.Payload)
	}
	t.Fatalf("no completed status appeared; saw:\n%s", strings.Join(seen, "\n"))
	return nil
}
