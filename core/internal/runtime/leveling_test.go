package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// twoFacedTool is cheap or expensive depending on what it is asked to do,
// which is the case the level split exists for.
type twoFacedTool struct{ ran atomic.Bool }

func (t *twoFacedTool) Spec() tool.Spec {
	return tool.Spec{
		Name:        "two_faced",
		Description: "Cheap unless asked for the expensive thing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"expensive":{"type":"boolean"}}}`),
		Level:       tool.LevelInternal,
	}
}

func (t *twoFacedTool) LevelFor(call tool.Call) tool.Level {
	var args struct {
		Expensive bool `json:"expensive"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err == nil && args.Expensive {
		return tool.LevelRemember
	}
	return tool.LevelInternal
}

func (t *twoFacedTool) Execute(context.Context, tool.Call) (tool.Result, error) {
	t.ran.Store(true)
	return tool.Result{Content: "done"}, nil
}

// The declared floor being the cheap case only works if the thing that asks
// the policy uses the raised level. If it read Spec().Level instead, the
// expensive call would run unattended — which is the failure this whole split
// would otherwise introduce.
func TestThePolicySeesTheRaisedLevel(t *testing.T) {
	for _, expensive := range []bool{false, true} {
		t.Run(fmt.Sprintf("expensive=%v", expensive), func(t *testing.T) {
			invoked := &twoFacedTool{}
			store := memory.New()
			rt := newLevelingHarness(t, store, invoked, map[string]any{"expensive": expensive})

			session, err := rt.CreateSession(context.Background(), "")
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			runID, _, err := rt.SendTurn(context.Background(), session.ID, "go", domain.RunOrigin{})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			waitForRun(t, rt, runID)

			events, err := store.ListAfter(context.Background(), session.ID, 0, 0)
			if err != nil {
				t.Fatalf("list: %v", err)
			}

			asked := false
			for _, e := range events {
				if _, ok := e.Payload.(domain.ApprovalRequested); ok {
					asked = true
				}
			}

			switch {
			case expensive && !asked:
				t.Error("the expensive call ran without stopping for anybody")
			case expensive && invoked.ran.Load():
				t.Error("the expensive call ran before it was approved")
			case !expensive && asked:
				t.Error("the cheap call stopped for a person")
			case !expensive && !invoked.ran.Load():
				t.Error("the cheap call did not run")
			}
		})
	}
}

func newLevelingHarness(
	t *testing.T,
	store *memory.Store,
	invoked tool.Tool,
	arguments map[string]any,
) *runtime.Runtime {
	t.Helper()

	registry := tool.NewRegistry()
	registry.MustRegister(invoked)

	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	scripted := &scriptedProvider{turns: [][]provider.Event{
		{provider.ToolCallRequested{ID: "call_1", Name: "two_faced", Args: encoded}, provider.Completed{}},
		{provider.TextDelta{Text: "done"}, provider.Completed{}},
	}}

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      scripted,
		Model:         "scripted",
		Tools:         registry,
		Permissions:   permission.New(permission.LocalProfile()),
		MaxIterations: 3,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:        slog.New(slog.DiscardHandler),
	})
}
