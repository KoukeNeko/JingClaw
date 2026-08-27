// Package storagetest holds a conformance suite every storage.Store must pass.
//
// Two implementations exist — in-memory for fast tests, SQLite for the real
// daemon — and the failure mode to guard against is them drifting apart, so
// that code passes its tests against one and misbehaves against the other. The
// suite is the contract; both run it.
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// Factory produces a fresh, empty store for each test.
type Factory func(t *testing.T) storage.Store

// Run executes the whole suite against the store produced by newStore.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	tests := map[string]func(*testing.T, Factory){
		"SessionRoundTrip":            testSessionRoundTrip,
		"DuplicateSessionRejected":    testDuplicateSessionRejected,
		"MissingSessionNotFound":      testMissingSessionNotFound,
		"ListSessionsNewestFirst":     testListSessionsNewestFirst,
		"RunRoundTrip":                testRunRoundTrip,
		"UpdateRunPersistsTerminal":   testUpdateRunPersistsTerminal,
		"UpdateMissingRunNotFound":    testUpdateMissingRunNotFound,
		"UnfinishedRunsExcludeDone":   testUnfinishedRunsExcludeDone,
		"GatewayOriginRoundTrip":      testGatewayOriginRoundTrip,
		"EventSeqDenseFromOne":        testEventSeqDenseFromOne,
		"EventPayloadsRoundTrip":      testEventPayloadsRoundTrip,
		"ListAfterRespectsCursor":     testListAfterRespectsCursor,
		"ListAfterRespectsLimit":      testListAfterRespectsLimit,
		"HeadTracksLatest":            testHeadTracksLatest,
		"EventsForMissingSession":     testEventsForMissingSession,
		"SessionsHaveIndependentSeqs": testSessionsHaveIndependentSeqs,
		"ConcurrentAppendsAreDense":   testConcurrentAppendsAreDense,
	}

	for name, fn := range tests {
		t.Run(name, func(t *testing.T) { fn(t, newStore) })
	}
}

func fixedTime() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func newSession(id string) domain.Session {
	return domain.Session{
		ID:        domain.SessionID(id),
		Title:     "title " + id,
		CreatedAt: fixedTime(),
		UpdatedAt: fixedTime(),
	}
}

func mustCreateSession(t *testing.T, store storage.Store, id string) domain.Session {
	t.Helper()

	session := newSession(id)
	if err := store.CreateSession(context.Background(), session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func appendEvent(t *testing.T, store storage.Store, session domain.SessionID, kind domain.EventKind, payload domain.EventPayload) domain.Seq {
	t.Helper()

	seq, err := store.Append(context.Background(), domain.Event{
		ID:         domain.EventID(fmt.Sprintf("evt_%s_%d", session, time.Now().UnixNano())),
		SessionID:  session,
		RunID:      "run_1",
		OccurredAt: fixedTime(),
		Kind:       kind,
		Payload:    payload,
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	return seq
}

func testSessionRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	want := mustCreateSession(t, store, "ses_1")

	got, err := store.Session(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}

	if got.ID != want.ID || got.Title != want.Title {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func testDuplicateSessionRejected(t *testing.T, newStore Factory) {
	store := newStore(t)
	session := mustCreateSession(t, store, "ses_1")

	err := store.CreateSession(context.Background(), session)
	if !errors.Is(err, storage.ErrDuplicateSession) {
		t.Fatalf("got %v, want ErrDuplicateSession", err)
	}
}

func testMissingSessionNotFound(t *testing.T, newStore Factory) {
	store := newStore(t)

	_, err := store.Session(context.Background(), "ses_missing")
	if !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatalf("got %v, want ErrSessionNotFound", err)
	}
}

func testListSessionsNewestFirst(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	for i, offset := range []time.Duration{0, time.Hour, 2 * time.Hour} {
		session := newSession(fmt.Sprintf("ses_%d", i))
		session.CreatedAt = fixedTime().Add(offset)
		session.UpdatedAt = session.CreatedAt
		if err := store.CreateSession(ctx, session); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	sessions, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}

	for i := 1; i < len(sessions); i++ {
		if sessions[i-1].CreatedAt.Before(sessions[i].CreatedAt) {
			t.Fatalf("sessions are not newest first: %v then %v",
				sessions[i-1].CreatedAt, sessions[i].CreatedAt)
		}
	}
}

func testRunRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	want := domain.Run{
		ID:        "run_1",
		SessionID: session.ID,
		Status:    domain.RunRunning,
		Origin: domain.RunOrigin{
			Kind:     domain.OriginLocalClient,
			ClientID: "cli",
		},
		DeliveryTargets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: "cli"}},
		CreatedAt:       fixedTime(),
	}
	if err := store.CreateRun(ctx, want); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := store.Run(ctx, want.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}

	if got.Status != want.Status || got.SessionID != want.SessionID {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.Origin.Kind != want.Origin.Kind || got.Origin.ClientID != want.Origin.ClientID {
		t.Errorf("origin %+v, want %+v", got.Origin, want.Origin)
	}
	if len(got.DeliveryTargets) != 1 || got.DeliveryTargets[0].Kind != domain.DeliveryLocalClient {
		t.Errorf("delivery targets %+v, want one local_client", got.DeliveryTargets)
	}
	if got.FinishedAt != nil {
		t.Errorf("finished_at is %v, want nil for a running run", got.FinishedAt)
	}
}

func testUpdateRunPersistsTerminal(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	run := domain.Run{
		ID: "run_1", SessionID: session.ID,
		Status: domain.RunRunning, CreatedAt: fixedTime(),
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	finished := fixedTime().Add(time.Minute)
	run.Status = domain.RunCompleted
	run.FinishedAt = &finished
	if err := store.UpdateRun(ctx, run); err != nil {
		t.Fatalf("update run: %v", err)
	}

	got, err := store.Run(ctx, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.Status != domain.RunCompleted {
		t.Errorf("status %q, want %q", got.Status, domain.RunCompleted)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at %v, want %v", got.FinishedAt, finished)
	}
}

func testUpdateMissingRunNotFound(t *testing.T, newStore Factory) {
	store := newStore(t)

	err := store.UpdateRun(context.Background(), domain.Run{ID: "run_missing", Status: domain.RunFailed})
	if !errors.Is(err, storage.ErrRunNotFound) {
		t.Fatalf("got %v, want ErrRunNotFound", err)
	}
}

// The orphan query is what startup uses to resolve runs abandoned by a crash,
// so it must return exactly the live ones.
func testUnfinishedRunsExcludeDone(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	statuses := map[string]domain.RunStatus{
		"run_queued":    domain.RunQueued,
		"run_running":   domain.RunRunning,
		"run_approval":  domain.RunAwaitingApproval,
		"run_input":     domain.RunAwaitingInput,
		"run_cancelnow": domain.RunCancelling,
		"run_done":      domain.RunCompleted,
		"run_cancelled": domain.RunCancelled,
		"run_failed":    domain.RunFailed,
	}
	for id, status := range statuses {
		if err := store.CreateRun(ctx, domain.Run{
			ID: domain.RunID(id), SessionID: session.ID,
			Status: status, CreatedAt: fixedTime(),
		}); err != nil {
			t.Fatalf("create run %s: %v", id, err)
		}
	}

	unfinished, err := store.UnfinishedRuns(ctx)
	if err != nil {
		t.Fatalf("unfinished runs: %v", err)
	}

	const wantCount = 5 // queued, running, awaiting approval, awaiting input, cancelling
	if len(unfinished) != wantCount {
		t.Fatalf("got %d unfinished runs, want %d: %+v", len(unfinished), wantCount, unfinished)
	}
	for _, run := range unfinished {
		if run.Status.IsTerminal() {
			t.Errorf("terminal run %s (%s) reported as unfinished", run.ID, run.Status)
		}
	}
}

// Gateway origins are unused until M1b, but they must survive a round trip
// now; discovering otherwise later would mean a data migration.
func testGatewayOriginRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	want := domain.RunOrigin{
		Kind: domain.OriginGateway,
		Principal: &domain.ExternalPrincipal{
			Platform:    "discord",
			AccountID:   "main-bot",
			TenantID:    "guild_123",
			PrincipalID: "user_456",
			DisplayName: "someone",
		},
	}
	if err := store.CreateRun(ctx, domain.Run{
		ID: "run_1", SessionID: session.ID,
		Status: domain.RunRunning, Origin: want, CreatedAt: fixedTime(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := store.Run(ctx, "run_1")
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.Origin.Kind != domain.OriginGateway {
		t.Fatalf("origin kind %q, want %q", got.Origin.Kind, domain.OriginGateway)
	}
	if got.Origin.Principal == nil {
		t.Fatal("principal was dropped")
	}
	if got.Origin.Principal.PrincipalID != want.Principal.PrincipalID {
		t.Errorf("principal id %q, want %q", got.Origin.Principal.PrincipalID, want.Principal.PrincipalID)
	}
}

func testEventSeqDenseFromOne(t *testing.T, newStore Factory) {
	store := newStore(t)
	session := mustCreateSession(t, store, "ses_1")

	for i := 1; i <= 5; i++ {
		seq := appendEvent(t, store, session.ID, domain.EventAssistantTextDelta,
			domain.AssistantTextDelta{MessageID: "msg_1", Text: fmt.Sprintf("chunk %d", i)})
		if want := domain.Seq(i); seq != want {
			t.Fatalf("append %d returned seq %d, want %d", i, seq, want)
		}
	}
}

func testEventPayloadsRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	payloads := []struct {
		kind    domain.EventKind
		payload domain.EventPayload
	}{
		{domain.EventUserMessageAdded, domain.UserMessageAdded{
			MessageID: "msg_1", Text: "測試訊息", Trust: domain.TrustUser,
			Origin: domain.RunOrigin{Kind: domain.OriginLocalClient, ClientID: "cli"},
		}},
		{domain.EventRunStateChanged, domain.RunStateChanged{
			Status: domain.RunCancelled, Reason: "user pressed stop",
		}},
		{domain.EventAssistantTextDelta, domain.AssistantTextDelta{
			MessageID: "msg_2", Text: "收到：",
		}},
		{domain.EventAssistantMessageCompleted, domain.AssistantMessageCompleted{
			MessageID: "msg_2",
		}},
	}

	for _, p := range payloads {
		appendEvent(t, store, session.ID, p.kind, p.payload)
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != len(payloads) {
		t.Fatalf("got %d events, want %d", len(events), len(payloads))
	}

	for i, want := range payloads {
		got := events[i]
		if got.Kind != want.kind {
			t.Errorf("event %d kind %q, want %q", i, got.Kind, want.kind)
		}
		if got.Payload != want.payload {
			t.Errorf("event %d payload %#v, want %#v", i, got.Payload, want.payload)
		}
	}

	// Non-ASCII text is the case a naive encoding gets wrong.
	first, ok := events[0].Payload.(domain.UserMessageAdded)
	if !ok {
		t.Fatalf("first payload is %T", events[0].Payload)
	}
	if first.Text != "測試訊息" {
		t.Errorf("text %q survived as %q", "測試訊息", first.Text)
	}
}

func testListAfterRespectsCursor(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	for i := range 5 {
		appendEvent(t, store, session.ID, domain.EventAssistantTextDelta,
			domain.AssistantTextDelta{MessageID: "msg_1", Text: fmt.Sprintf("%d", i)})
	}

	events, err := store.ListAfter(ctx, session.ID, 3, 0)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Seq != 4 || events[1].Seq != 5 {
		t.Fatalf("got seqs %d,%d want 4,5", events[0].Seq, events[1].Seq)
	}

	// Asking past the end is a normal state for a caller that is fully caught
	// up, not an error.
	events, err = store.ListAfter(ctx, session.ID, 5, 0)
	if err != nil {
		t.Fatalf("list past end: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events past the end, want 0", len(events))
	}
}

func testListAfterRespectsLimit(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	for i := range 10 {
		appendEvent(t, store, session.ID, domain.EventAssistantTextDelta,
			domain.AssistantTextDelta{MessageID: "msg_1", Text: fmt.Sprintf("%d", i)})
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 3)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Seq != 1 || events[2].Seq != 3 {
		t.Fatalf("got seqs %d..%d want 1..3", events[0].Seq, events[2].Seq)
	}
}

func testHeadTracksLatest(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	head, err := store.Head(ctx, session.ID)
	if err != nil {
		t.Fatalf("head of empty session: %v", err)
	}
	if head != 0 {
		t.Fatalf("head %d, want 0 for an empty session", head)
	}

	appendEvent(t, store, session.ID, domain.EventAssistantTextDelta,
		domain.AssistantTextDelta{MessageID: "msg_1", Text: "x"})

	head, err = store.Head(ctx, session.ID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 1 {
		t.Fatalf("head %d, want 1", head)
	}
}

// An unknown session must be distinguishable from an empty one, or a client
// typo looks like a session that simply has nothing in it yet.
func testEventsForMissingSession(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.ListAfter(ctx, "ses_missing", 0, 0); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Errorf("ListAfter: got %v, want ErrSessionNotFound", err)
	}
	if _, err := store.Head(ctx, "ses_missing"); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Errorf("Head: got %v, want ErrSessionNotFound", err)
	}

	_, err := store.Append(ctx, domain.Event{
		ID: "evt_1", SessionID: "ses_missing", OccurredAt: fixedTime(),
		Kind: domain.EventRunStateChanged, Payload: domain.RunStateChanged{Status: domain.RunRunning},
	})
	if !errors.Is(err, storage.ErrSessionNotFound) {
		t.Errorf("Append: got %v, want ErrSessionNotFound", err)
	}
}

func testSessionsHaveIndependentSeqs(t *testing.T, newStore Factory) {
	store := newStore(t)
	first := mustCreateSession(t, store, "ses_1")
	second := mustCreateSession(t, store, "ses_2")

	appendEvent(t, store, first.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "a"})
	appendEvent(t, store, first.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "b"})

	seq := appendEvent(t, store, second.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "c"})
	if seq != 1 {
		t.Fatalf("second session started at seq %d, want 1", seq)
	}
}

// Sequence allocation reads the current maximum and then inserts. Done without
// a transaction that holds the write lock across both, concurrent appends
// collide on the same number and history silently loses an event.
func testConcurrentAppendsAreDense(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	const appends = 40
	var wg sync.WaitGroup
	seqs := make([]domain.Seq, appends)
	errs := make([]error, appends)

	for i := range appends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seqs[i], errs[i] = store.Append(ctx, domain.Event{
				ID:         domain.EventID(fmt.Sprintf("evt_%d", i)),
				SessionID:  session.ID,
				RunID:      "run_1",
				OccurredAt: fixedTime(),
				Kind:       domain.EventAssistantTextDelta,
				Payload:    domain.AssistantTextDelta{MessageID: "msg_1", Text: fmt.Sprintf("%d", i)},
			})
		}()
	}
	wg.Wait()

	seen := make(map[domain.Seq]bool, appends)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("sequence %d handed out twice", seqs[i])
		}
		seen[seqs[i]] = true
	}

	for i := 1; i <= appends; i++ {
		if !seen[domain.Seq(i)] {
			t.Fatalf("sequence %d was never allocated", i)
		}
	}

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != appends {
		t.Fatalf("stored %d events, want %d", len(events), appends)
	}
}
