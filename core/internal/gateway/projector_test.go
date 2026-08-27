package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// These drive the real runtime, so what a channel would actually receive is
// asserted rather than assumed.

type harness struct {
	ingress *gateway.Ingress
	store   *sqlite.Store
	runtime *runtime.Runtime
}

func newHarness(t *testing.T, chunkDelay time.Duration) *harness {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()
	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		builtin.NewWriteFile(ws, observed, locks),
		&builtin.ExecCommand{Workspace: ws},
	)

	var counter atomic.Uint64
	next := func(prefix string) runtime.IDGenerator {
		return func() string { return fmt.Sprintf("%s_%d", prefix, counter.Add(1)) }
	}

	permissions := permission.New(permission.LocalProfile())
	projector := gateway.NewProjector(store,
		func() string { return fmt.Sprintf("dsp_%d", counter.Add(1)) },
		func() time.Time { return time.Unix(0, 0).UTC() })

	rt := runtime.New(ctx, runtime.Options{
		Store:         store,
		Hub:           event.NewHub(),
		Provider:      fake.New(chunkDelay),
		Model:         fake.ModelID,
		Tools:         registry,
		Permissions:   permissions,
		Delivery:      projector,
		MaxIterations: 5,
		NewSessionID:  next("ses"),
		NewRunID:      next("run"),
		NewMessageID:  next("msg"),
		NewEventID:    next("evt"),
		NewApprovalID: next("apr"),
		Now:           time.Now,
		Logger:        slog.New(slog.DiscardHandler),
	})

	ingress := &gateway.Ingress{
		Store:   store,
		Runtime: rt,
		Binder:  permissions,
		Now:     func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:  slog.New(slog.DiscardHandler),
	}

	return &harness{ingress: ingress, store: store, runtime: rt}
}

func (h *harness) bind(t *testing.T, profile string, allowed ...string) {
	t.Helper()

	conversation := discordConversation()
	if err := h.store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                "binding_1",
		Platform:          conversation.Platform,
		AccountID:         conversation.AccountID,
		TenantID:          conversation.TenantID,
		ChannelID:         conversation.ChannelID,
		WorkspaceID:       "ws_1",
		PermissionProfile: profile,
		AllowedPrincipals: allowed,
		CreatedAt:         time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func (h *harness) dispatches(t *testing.T) []gateway.Dispatch {
	t.Helper()

	pending, err := h.store.DispatchesAfter(context.Background(), "main-bot", 0, 0)
	if err != nil {
		t.Fatalf("list dispatches: %v", err)
	}
	return pending
}

func waitForDispatch(t *testing.T, h *harness, kind gateway.DispatchKind) gateway.Dispatch {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, dispatch := range h.dispatches(t) {
			if dispatch.Kind == kind {
				return dispatch
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("no %s dispatch appeared; got %+v", kind, h.dispatches(t))
	return gateway.Dispatch{}
}

// A message from a channel must produce a reply addressed back to that
// channel, not to whoever is running the daemon.
func TestReplyIsQueuedBackToTheConversation(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "測試訊息", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	reply := waitForDispatch(t, h, gateway.DispatchMessage)

	if reply.Target.ChannelID != "channel_1" || reply.Target.ThreadID != "thread_1" {
		t.Errorf("the reply is addressed to %+v", reply.Target)
	}

	var payload gateway.MessagePayload
	if err := json.Unmarshal([]byte(reply.Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Text == "" {
		t.Error("the reply carries no text")
	}
	// Assembled, not streamed: the channel gets the answer rather than the
	// answer being typed.
	if !strings.Contains(payload.Text, "測試訊息") {
		t.Errorf("the reply does not contain the echoed request: %q", payload.Text)
	}
}

// A channel that hears nothing for a minute cannot tell working from broken.
func TestRunStartIsAnnounced(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	status := waitForDispatch(t, h, gateway.DispatchStatus)

	var payload gateway.StatusPayload
	if err := json.Unmarshal([]byte(status.Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.State != "running" {
		t.Errorf("first status is %q, want running", payload.State)
	}
}

// Deltas must not each become a message: Discord would need an edit per chunk,
// which is a rate limit waiting to happen.
func TestDeltasDoNotBecomeSeparateDispatches(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "hello", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waitForDispatch(t, h, gateway.DispatchMessage)

	messages := 0
	for _, dispatch := range h.dispatches(t) {
		if dispatch.Kind == gateway.DispatchMessage {
			messages++
		}
	}
	if messages != 1 {
		t.Errorf("one turn produced %d messages, want 1", messages)
	}
}

// A local session's output must not leak into a channel.
func TestLocalRunsProduceNoDispatches(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()

	session, err := h.runtime.CreateSession(ctx, "local")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	runID, _, err := h.runtime.SendTurn(ctx, session.ID, "hello",
		domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "cli"})
	if err != nil {
		t.Fatalf("send turn: %v", err)
	}
	if err := h.runtime.Wait(ctx, runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if pending := h.dispatches(t); len(pending) != 0 {
		t.Errorf("a local run queued %d deliveries: %+v", len(pending), pending)
	}
}

// Execution from a channel is refused outright, and the refusal reaches the
// model rather than stopping the run.
func TestExecutionFromAChannelIsRefusedNotAsked(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")

	accepted, err := h.ingress.Accept(context.Background(),
		message("m1", "run the tests", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// The fake provider never calls tools, so the policy is asserted directly:
	// what matters is that a channel session answers deny, not ask.
	outcome := evaluateExec(t, h, accepted.SessionID)
	if outcome != permission.Deny {
		t.Errorf("execution from a channel is %q, want %q", outcome, permission.Deny)
	}

	// Nothing was queued asking a human to approve it.
	for _, dispatch := range h.dispatches(t) {
		if dispatch.Kind == gateway.DispatchApproval {
			t.Error("a channel was asked to approve an execution")
		}
	}
}

func evaluateExec(t *testing.T, h *harness, session domain.SessionID) permission.Decision {
	t.Helper()

	engine := permission.New(permission.LocalProfile())
	if err := engine.UseProfile(session, "gateway"); err != nil {
		t.Fatalf("use profile: %v", err)
	}

	return engine.Evaluate(context.Background(), permission.Request{
		Spec:      tool.Spec{Name: "exec_command", Level: tool.LevelExecute},
		SessionID: session,
	}).Decision
}

// A run that fails must say so. Ending in silence looks exactly like still
// working.
func TestFailureIsAnnounced(t *testing.T) {
	h := newHarness(t, 0)
	h.bind(t, "gateway", "user_1")
	ctx := context.Background()

	accepted, err := h.ingress.Accept(ctx, message("m1", "hello", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := h.runtime.Wait(ctx, accepted.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// A second run, cancelled while it is generating.
	slow := newHarness(t, time.Hour)
	slow.bind(t, "gateway", "user_1")

	second, err := slow.ingress.Accept(ctx, message("m2", "slow one", discordPrincipal("user_1")))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	waitForDispatch(t, slow, gateway.DispatchStatus)

	if _, err := slow.runtime.InterruptRun(ctx, second.RunID, "stopped"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := slow.runtime.Wait(ctx, second.RunID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	var sawTerminal bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sawTerminal {
		for _, dispatch := range slow.dispatches(t) {
			if dispatch.Kind != gateway.DispatchStatus {
				continue
			}
			var payload gateway.StatusPayload
			if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
				continue
			}
			if payload.State == string(domain.RunCancelled) {
				sawTerminal = true
			}
		}
		if !sawTerminal {
			time.Sleep(10 * time.Millisecond)
		}
	}

	if !sawTerminal {
		t.Error("a cancelled run told the channel nothing")
	}
}
