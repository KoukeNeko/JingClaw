package control_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

const testToken = "test-token"

// These tests drive the real Connect stack over a real socket rather than
// calling handlers directly, because the things most likely to break — auth,
// streaming, resume — only exist at that layer.
// harness is a server with the pieces behind it reachable, for the checks
// that have to arrange the store directly.
type harness struct {
	client  controlv1connect.SessionServiceClient
	store   *memory.Store
	runtime *runtime.Runtime
}

func newServer(t *testing.T, chunkDelay time.Duration) controlv1connect.SessionServiceClient {
	t.Helper()
	return newHarness(t, chunkDelay).client
}

func newHarness(t *testing.T, chunkDelay time.Duration) harness {
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
		Provider:      fake.New(chunkDelay),
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

	artifacts, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifacts: %v", err)
	}

	offline := fake.New(chunkDelay)

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewSessionServiceHandler(
		control.NewServer(rt, store, hub, artifacts, offline, fake.ModelID)))

	// The port is unknown until the test server starts, so host validation is
	// exercised separately in TestAuthMiddleware.
	handler := control.AuthMiddleware(
		[]control.Token{{Value: testToken, Scope: control.ScopeControl}},
		nil, "", mux)
	server := httptest.NewUnstartedServer(h2c.NewHandler(handler, &http2.Server{}))
	server.EnableHTTP2 = true
	server.Start()

	t.Cleanup(func() {
		server.Close()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = rt.Shutdown(shutdownCtx)
	})

	httpClient := &http.Client{Transport: &bearerTransport{token: testToken, base: server.Client().Transport}}
	return harness{
		client:  controlv1connect.NewSessionServiceClient(httpClient, server.URL),
		store:   store,
		runtime: rt,
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(clone)
}

func TestEndToEndTurnStreamsToCompletion(t *testing.T) {
	client := newServer(t, 0)
	ctx := context.Background()

	created, err := client.CreateSession(ctx, connect.NewRequest(&controlv1.CreateSessionRequest{Title: "e2e"}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := created.Msg.GetSession().GetId()

	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := client.SubscribeEvents(streamCtx, connect.NewRequest(&controlv1.SubscribeEventsRequest{
		SessionId: sessionID,
		ClientId:  "test",
	}))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("expected a hello frame: %v", stream.Err())
	}
	if stream.Msg().GetHello() == nil {
		t.Fatalf("first frame is %T, want hello", stream.Msg().GetValue())
	}

	if _, err := client.SendTurn(ctx, connect.NewRequest(&controlv1.SendTurnRequest{
		SessionId: sessionID,
		Text:      "hello",
	})); err != nil {
		t.Fatalf("send turn: %v", err)
	}

	var seqs []uint64
	for stream.Receive() {
		ev := stream.Msg().GetEvent()
		if ev == nil {
			continue
		}
		seqs = append(seqs, ev.GetSeq())

		if changed := ev.GetRunStateChanged(); changed != nil &&
			changed.GetStatus() == controlv1.RunStatus_RUN_STATUS_COMPLETED {
			break
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}

	if len(seqs) == 0 {
		t.Fatal("no events received")
	}
	for i, seq := range seqs {
		if want := uint64(i + 1); seq != want {
			t.Fatalf("event %d has seq %d, want %d", i, seq, want)
		}
	}
}

// The resume contract at the RPC layer: this is what lets a UI reconnect after
// a dropped connection without replaying or losing anything.
func TestSubscribeAfterSeqResumesExactly(t *testing.T) {
	client := newServer(t, 0)
	ctx := context.Background()

	created, err := client.CreateSession(ctx, connect.NewRequest(&controlv1.CreateSessionRequest{Title: "resume"}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := created.Msg.GetSession().GetId()

	if _, err := client.SendTurn(ctx, connect.NewRequest(&controlv1.SendTurnRequest{
		SessionId: sessionID,
		Text:      "hello",
	})); err != nil {
		t.Fatalf("send turn: %v", err)
	}

	all := drainUntilCompleted(t, client, sessionID, 0)
	if len(all) < 4 {
		t.Fatalf("expected several events, got %d", len(all))
	}

	const after = uint64(3)
	tail := drainUntilCompleted(t, client, sessionID, after)

	if want := len(all) - int(after); len(tail) != want {
		t.Fatalf("resuming after seq %d gave %d events, want %d", after, len(tail), want)
	}
	if tail[0] != after+1 {
		t.Fatalf("resume started at seq %d, want %d", tail[0], after+1)
	}
}

// Detaching is not cancelling: the daemon owns the run, so a client hanging up
// mid-stream must leave it running.
func TestDetachingDoesNotCancelRun(t *testing.T) {
	client := newServer(t, 50*time.Millisecond)
	ctx := context.Background()

	created, err := client.CreateSession(ctx, connect.NewRequest(&controlv1.CreateSessionRequest{Title: "detach"}))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionID := created.Msg.GetSession().GetId()

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := client.SubscribeEvents(streamCtx, connect.NewRequest(&controlv1.SubscribeEventsRequest{
		SessionId: sessionID,
		ClientId:  "detacher",
	}))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if _, err := client.SendTurn(ctx, connect.NewRequest(&controlv1.SendTurnRequest{
		SessionId: sessionID,
		Text:      "hello",
	})); err != nil {
		t.Fatalf("send turn: %v", err)
	}

	// Hang up as soon as the first real event arrives, mid-generation.
	for stream.Receive() {
		if stream.Msg().GetEvent() != nil {
			break
		}
	}
	cancel()
	_ = stream.Close()

	// Reattaching must show the run reached completion regardless.
	seqs := drainUntilCompleted(t, client, sessionID, 0)
	if len(seqs) == 0 {
		t.Fatal("run produced no events after the client detached")
	}
}

func TestSendTurnToUnknownSessionIsNotFound(t *testing.T) {
	client := newServer(t, 0)

	_, err := client.SendTurn(context.Background(), connect.NewRequest(&controlv1.SendTurnRequest{
		SessionId: "ses_missing",
		Text:      "hello",
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("got code %v, want %v (err: %v)", got, connect.CodeNotFound, err)
	}
}

func TestSendTurnWithoutSessionIDIsInvalidArgument(t *testing.T) {
	client := newServer(t, 0)

	_, err := client.SendTurn(context.Background(), connect.NewRequest(&controlv1.SendTurnRequest{Text: "hello"}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("got code %v, want %v (err: %v)", got, connect.CodeInvalidArgument, err)
	}
}

// drainUntilCompleted subscribes from after and returns the sequence numbers
// seen up to and including the run's completion.
func drainUntilCompleted(t *testing.T, client controlv1connect.SessionServiceClient, sessionID string, after uint64) []uint64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.SubscribeEvents(ctx, connect.NewRequest(&controlv1.SubscribeEventsRequest{
		SessionId: sessionID,
		AfterSeq:  after,
		ClientId:  "drain",
	}))
	if err != nil {
		t.Fatalf("subscribe after %d: %v", after, err)
	}
	defer func() { _ = stream.Close() }()

	var seqs []uint64
	for stream.Receive() {
		ev := stream.Msg().GetEvent()
		if ev == nil {
			continue
		}
		seqs = append(seqs, ev.GetSeq())

		if changed := ev.GetRunStateChanged(); changed != nil &&
			changed.GetStatus() == controlv1.RunStatus_RUN_STATUS_COMPLETED {
			return seqs
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream after %d: %v", after, err)
	}
	return seqs
}

// A run outlives the client that began it, so a client that arrives later has
// to be able to find what is going on without having been there. Without this
// the web console opened after the fact would have nothing to attach to.
func TestAClientThatArrivesLaterCanFindTheSession(t *testing.T) {
	client := newServer(t, 0)
	ctx := context.Background()

	first, err := client.CreateSession(ctx, connect.NewRequest(&controlv1.CreateSessionRequest{
		Meta: &controlv1.RequestMeta{ClientId: "test"}, Title: "started elsewhere",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sessionID := first.Msg.GetSession().GetId()

	if _, err := client.SendTurn(ctx, connect.NewRequest(&controlv1.SendTurnRequest{
		Meta: &controlv1.RequestMeta{ClientId: "test"}, SessionId: sessionID, Text: "hello",
	})); err != nil {
		t.Fatalf("send: %v", err)
	}

	listed, err := client.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}

	found := false
	for _, session := range listed.Msg.GetSessions() {
		if session.GetId() == sessionID {
			found = true
			if session.GetTitle() != "started elsewhere" {
				t.Errorf("title is %q", session.GetTitle())
			}
		}
	}
	if !found {
		t.Fatalf("%s is not in the listing", sessionID)
	}

	runs, err := client.ListRuns(ctx, connect.NewRequest(&controlv1.ListRunsRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Msg.GetRuns()) == 0 {
		t.Fatal("the run started a moment ago is not listed")
	}
	if runs.Msg.GetRuns()[0].GetSessionId() != sessionID {
		t.Error("a run was listed for the wrong session")
	}
}

// Asking for the runs of nothing is a mistake worth reporting, not an empty
// list that looks like a session with no runs.
func TestListingRunsWithoutASessionIsRefused(t *testing.T) {
	client := newServer(t, 0)

	_, err := client.ListRuns(context.Background(),
		connect.NewRequest(&controlv1.ListRunsRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code is %s, want invalid_argument", connect.CodeOf(err))
	}
}

// A decision that says nothing must be refused, not read as a deny.
//
// A boolean could not tell the two apart, which is how a client with a
// mistyped field name came to refuse tools on the operator's behalf and report
// success. That is a very quiet way to be wrong about a security decision.
func TestADecisionThatSaysNothingIsRefused(t *testing.T) {
	client := newServer(t, 0)

	_, err := client.DecideApproval(context.Background(),
		connect.NewRequest(&controlv1.DecideApprovalRequest{
			Meta:       &controlv1.RequestMeta{ClientId: "test"},
			ApprovalId: "apr_whatever",
			Remember:   controlv1.RememberScope_REMEMBER_SCOPE_ONCE,
		}))

	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code is %s, want invalid_argument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "APPROVAL_DECISION_ALLOW") {
		t.Errorf("the error does not say what to send instead: %v", err)
	}
}

// A client resuming from before what is kept is told to start again.
//
// Sending the remainder would draw a conversation missing its middle with
// nothing marking the gap, and that reads as the agent having forgotten
// rather than as history discarded on purpose.
func TestResumingFromDiscardedHistoryAsksForAResync(t *testing.T) {
	harness := newHarness(t, 0)

	session, err := harness.runtime.CreateSession(context.Background(), "pruned")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runID, _, err := harness.runtime.SendTurn(
		context.Background(), session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := harness.runtime.Wait(context.Background(), runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Discard the early events the way retention does.
	events, err := harness.store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) < 3 {
		t.Skipf("only %d events, too few to prune", len(events))
	}
	cut := events[len(events)-2].Seq
	if _, err := harness.store.PruneEvents(context.Background(), session.ID, cut); err != nil {
		t.Fatalf("prune: %v", err)
	}

	stream, err := harness.client.SubscribeEvents(context.Background(),
		connect.NewRequest(&controlv1.SubscribeEventsRequest{
			SessionId: string(session.ID),
			AfterSeq:  1,
		}))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var askedToResync bool
	for stream.Receive() {
		if resync := stream.Msg().GetResyncRequired(); resync != nil {
			askedToResync = true
			if resync.GetOldestSeq() == 0 {
				t.Error("the resync does not say what the oldest kept event is")
			}
			if resync.GetHeadSeq() < resync.GetOldestSeq() {
				t.Errorf("head %d is below oldest %d",
					resync.GetHeadSeq(), resync.GetOldestSeq())
			}
			break
		}
		if stream.Msg().GetEvent() != nil {
			t.Fatal("events were sent for a range that had been discarded")
		}
	}

	if !askedToResync {
		t.Error("a client resuming from discarded history was not asked to resync")
	}
}

// A client resuming from inside what is kept is served normally: the resync is
// for a gap, not for every reconnect.
func TestResumingFromKeptHistoryStreamsNormally(t *testing.T) {
	harness := newHarness(t, 0)

	session, err := harness.runtime.CreateSession(context.Background(), "intact")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	runID, _, err := harness.runtime.SendTurn(
		context.Background(), session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := harness.runtime.Wait(context.Background(), runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	stream, err := harness.client.SubscribeEvents(context.Background(),
		connect.NewRequest(&controlv1.SubscribeEventsRequest{
			SessionId: string(session.ID),
			AfterSeq:  0,
		}))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	for stream.Receive() {
		if stream.Msg().GetResyncRequired() != nil {
			t.Fatal("a client with intact history was asked to resync")
		}
		if stream.Msg().GetEvent() != nil {
			return
		}
	}
	t.Error("no events arrived")
}
