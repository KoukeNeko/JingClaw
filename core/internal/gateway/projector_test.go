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

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
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
	ingress   *gateway.Ingress
	store     *sqlite.Store
	runtime   *runtime.Runtime
	artifacts *artifact.Store
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

	artifacts, err := artifact.Open(filepath.Join(t.TempDir(), "artifacts"), 64<<20)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	ingress := &gateway.Ingress{
		Store:         store,
		Runtime:       rt,
		Binder:        permissions,
		Artifacts:     artifacts,
		Console:       rt,
		NewDispatchID: func() string { return fmt.Sprintf("dsp_%d", counter.Add(1)) },
		Now:           func() time.Time { return time.Unix(0, 0).UTC() },
		Logger:        slog.New(slog.DiscardHandler),
	}

	return &harness{ingress: ingress, store: store, runtime: rt, artifacts: artifacts}
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

// The tests below drive the projector directly rather than through a run,
// because what is under test is when it decides to speak.

func newProjectorFixture(t *testing.T) (*gateway.Projector, *sqlite.Store, *time.Time) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "projector.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	clock := time.Unix(1_700_000_000, 0).UTC()

	var counter atomic.Uint64
	projector := gateway.NewProjector(store,
		func() string { return fmt.Sprintf("dsp_%d", counter.Add(1)) },
		func() time.Time { return clock })

	// A dispatch belongs to a run, and the schema says so. Inventing ids the
	// database has never seen would test the projector against a store that
	// does not behave like the real one.
	run := gatewayRun(clock)
	if err := store.CreateSession(ctx, domain.Session{
		ID: run.SessionID, CreatedAt: clock, UpdatedAt: clock,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	return projector, store, &clock
}

// gatewayRun is a run whose reply is owed to a conversation rather than to
// whoever is holding a terminal.
func gatewayRun(startedAt time.Time) domain.Run {
	conversation := gateway.ConversationRef{
		Platform:  gateway.PlatformDiscord,
		AccountID: "main",
		ChannelID: "channel_1",
	}

	return domain.Run{
		ID:              "run_1",
		SessionID:       "ses_1",
		CreatedAt:       startedAt,
		DeliveryTargets: []domain.DeliveryTarget{conversation.DeliveryTarget()},
	}
}

func enqueued(t *testing.T, store *sqlite.Store) []gateway.Dispatch {
	t.Helper()

	dispatches, err := store.DispatchesAfter(context.Background(), "main", 0, 100)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	return dispatches
}

// Silence while a run reads four files and waits on a test suite is
// indistinguishable from silence because something broke.
func TestToolCallsSayWhatIsHappening(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)

	if err := projector.Observe(context.Background(), gatewayRun(*clock), domain.Event{
		Kind: domain.EventToolCallRequested,
		Payload: domain.ToolCallRequested{
			CallID: "call_1", Name: "read_file", Arguments: `{"path":"notes.txt"}`,
		},
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	dispatches := enqueued(t, store)
	if len(dispatches) != 1 {
		t.Fatalf("%d dispatches, want one", len(dispatches))
	}
	if dispatches[0].Kind != gateway.DispatchStatus {
		t.Errorf("kind is %s, want a status line", dispatches[0].Kind)
	}
	if !strings.Contains(dispatches[0].Payload, "read_file notes.txt") {
		t.Errorf("the line does not say what it is doing: %s", dispatches[0].Payload)
	}
}

// A run that reads six files in a second must not send six lines: more than a
// person can read, and more than a platform will take.
func TestWhatItIsDoingIsThrottled(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)
	projector.WorkingInterval = 2 * time.Second

	started := *clock
	call := func() {
		t.Helper()
		if err := projector.Observe(context.Background(), gatewayRun(started), domain.Event{
			Kind: domain.EventToolCallRequested,
			Payload: domain.ToolCallRequested{
				CallID: "call", Name: "read_file", Arguments: `{"path":"a.txt"}`,
			},
		}); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}

	call()
	for range 5 {
		*clock = clock.Add(100 * time.Millisecond)
		call()
	}
	if got := len(enqueued(t, store)); got != 1 {
		t.Errorf("six calls in half a second produced %d lines", got)
	}

	// And it starts speaking again once enough time has passed — otherwise a
	// long run goes quiet after its first line, which is the whole problem.
	*clock = clock.Add(3 * time.Second)
	call()
	if got := len(enqueued(t, store)); got != 2 {
		t.Errorf("after the interval it produced %d lines, want 2", got)
	}
}

// The line that said what it was doing must not sit above the answer still
// claiming the agent is busy.
func TestCompletionReplacesTheWorkingLine(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)

	started := *clock
	*clock = clock.Add(12 * time.Second)

	if err := projector.Observe(context.Background(), gatewayRun(started), domain.Event{
		Kind:    domain.EventRunStateChanged,
		Payload: domain.RunStateChanged{Status: domain.RunCompleted},
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	dispatches := enqueued(t, store)
	if len(dispatches) != 1 {
		t.Fatalf("%d dispatches, want one", len(dispatches))
	}
	if !strings.Contains(dispatches[0].Payload, "completed") {
		t.Errorf("payload is %s", dispatches[0].Payload)
	}
	// How long it took is the one thing worth saying that the answer does not.
	if !strings.Contains(dispatches[0].Payload, "12s") {
		t.Errorf("the line does not say how long it took: %s", dispatches[0].Payload)
	}
}

// deltas feeds an answer to the projector one chunk at a time, advancing the
// clock between them.
func deltas(t *testing.T, projector *gateway.Projector, clock *time.Time,
	run domain.Run, chunks []string, apart time.Duration,
) {
	t.Helper()

	for _, chunk := range chunks {
		if err := projector.Observe(context.Background(), run, domain.Event{
			Kind:    domain.EventAssistantTextDelta,
			Payload: domain.AssistantTextDelta{MessageID: "msg_1", Text: chunk},
		}); err != nil {
			t.Fatalf("observe a delta: %v", err)
		}
		*clock = clock.Add(apart)
	}

	if err := projector.Observe(context.Background(), run, domain.Event{
		Kind:    domain.EventAssistantMessageCompleted,
		Payload: domain.AssistantMessageCompleted{MessageID: "msg_1"},
	}); err != nil {
		t.Fatalf("observe completion: %v", err)
	}
}

func payloadOf(t *testing.T, dispatch gateway.Dispatch) gateway.MessagePayload {
	t.Helper()

	var payload gateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

// An answer finished inside one interval arrives whole. Streaming it would
// mean posting a paragraph and rewriting it a moment later, which is worse
// than waiting.
func TestAShortAnswerIsNotStreamed(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)
	projector.StreamInterval = 2 * time.Second

	deltas(t, projector, clock, gatewayRun(*clock),
		[]string{"All ", "three ", "tests ", "pass."}, 100*time.Millisecond)

	dispatches := enqueued(t, store)
	if len(dispatches) != 1 {
		t.Fatalf("%d dispatches for a short answer, want one", len(dispatches))
	}

	payload := payloadOf(t, dispatches[0])
	if !payload.Final {
		t.Error("the only dispatch is not marked final")
	}
	if payload.Text != "All three tests pass." {
		t.Errorf("text is %q", payload.Text)
	}
}

// A long answer appears as it is written. Waiting for the whole thing means a
// channel watches nothing happen for as long as the model takes.
func TestALongAnswerIsStreamedAsItGrows(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)
	projector.StreamInterval = time.Second

	deltas(t, projector, clock, gatewayRun(*clock),
		[]string{"one ", "two ", "three ", "four ", "five"}, 2*time.Second)

	dispatches := enqueued(t, store)
	if len(dispatches) < 3 {
		t.Fatalf("%d dispatches; an answer written over ten seconds should show progress",
			len(dispatches))
	}

	// Every version names the same answer, so a platform can keep rewriting
	// one message rather than posting each.
	var previous string
	for index, dispatch := range dispatches {
		payload := payloadOf(t, dispatch)

		if payload.MessageID != "msg_1" {
			t.Errorf("dispatch %d does not name the answer it belongs to", index)
		}
		if len(payload.Text) < len(previous) {
			t.Errorf("dispatch %d went backwards: %q after %q", index, payload.Text, previous)
		}
		previous = payload.Text

		if final := index == len(dispatches)-1; payload.Final != final {
			t.Errorf("dispatch %d has final=%v, want %v", index, payload.Final, final)
		}
	}

	if previous != "one two three four five" {
		t.Errorf("the last version is %q, not the whole answer", previous)
	}
}

// The first delta only starts the clock. Emitting on it would post three
// characters and then rewrite them.
func TestTheFirstDeltaDoesNotPostAnything(t *testing.T) {
	projector, store, clock := newProjectorFixture(t)
	projector.StreamInterval = time.Second

	if err := projector.Observe(context.Background(), gatewayRun(*clock), domain.Event{
		Kind:    domain.EventAssistantTextDelta,
		Payload: domain.AssistantTextDelta{MessageID: "msg_1", Text: "I"},
	}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	if got := len(enqueued(t, store)); got != 0 {
		t.Errorf("the first delta produced %d dispatches", got)
	}
}
