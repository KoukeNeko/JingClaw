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
	"reflect"
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
		"MemoryRoundTrip":             testMemoryRoundTrip,
		"MemoryScopesAreSeparate":     testMemoryScopesAreSeparate,
		"MemoryCorrectionSupersedes":  testMemoryCorrectionSupersedes,
		"MemorySearchFindsByWord":     testMemorySearchFindsByWord,
		"MemorySearchRespectsScope":   testMemorySearchRespectsScope,
		"MemoryForgetActuallyRemoves": testMemoryForgetActuallyRemoves,
		"MemoryProvenanceSurvives":    testMemoryProvenanceSurvives,
		"PlanRoundTrip":               testPlanRoundTrip,
		"PlanIsEmptyBeforeOneIsMade":  testPlanIsEmptyBeforeOneIsMade,
		"PlanReplacesRatherThanAdds":  testPlanReplacesRatherThanAdds,
		"PlansAreSeparatePerSession":  testPlansAreSeparatePerSession,

		"MemoryValidityIsSeparateFromBelief": testMemoryValidityIsSeparateFromBelief,
		"SupersedingClosesTheOldValidity":    testSupersedingClosesTheOldValidity,
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
			Attachments: []domain.Attachment{{
				ArtifactID: "sha256-abc", Name: "screenshot.png",
				MediaType: "image/png", Size: 1234,
			}},
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
		// DeepEqual rather than ==: a payload with a slice in it is not
		// comparable, and == on an interface holding one panics at runtime
		// rather than failing to compile.
		if !reflect.DeepEqual(got.Payload, want.payload) {
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

// --- memory -----------------------------------------------------------------
//
// Memory is the one store whose contents reach a model in a later session, so
// what it returns is a security property rather than a convenience.

func newMemory(id, text string, scope domain.MemoryScope, ref string) domain.Memory {
	return domain.Memory{
		ID:         domain.MemoryID(id),
		Scope:      scope,
		ScopeRef:   ref,
		Activation: domain.MemoryRetrieval,
		Text:       text,
		Trust:      domain.TrustUser,
		Origin: domain.RunOrigin{
			Kind:     domain.OriginLocalClient,
			ClientID: "jingclaw-cli",
		},
		SourceSession: "ses_1",
		SourceSeq:     7,
		ApprovedBy:    "operator",
		CreatedAt:     fixedTime(),
	}
}

func remember(t *testing.T, store storage.Store, memory domain.Memory, supersedes domain.MemoryID) {
	t.Helper()

	if err := store.Remember(context.Background(), memory, supersedes); err != nil {
		t.Fatalf("remember %s: %v", memory.ID, err)
	}
}

func testMemoryRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	written := newMemory("mem_1", "the deploy script needs sudo",
		domain.ScopeWorkspace, "/srv/app")
	remember(t, store, written, "")

	found, err := store.Memory(ctx, "mem_1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if found.Text != written.Text || found.Scope != written.Scope {
		t.Errorf("read back %+v", found)
	}
	if !found.IsCurrent() {
		t.Error("a memory just written is not current")
	}

	if _, err := store.Memory(ctx, "mem_absent"); !errors.Is(err, storage.ErrMemoryNotFound) {
		t.Errorf("a missing memory gave %v", err)
	}
}

// A Discord account and the operator of this machine are different people.
// Returning one's memories for the other is the whole failure this scoping
// exists to prevent.
func testMemoryScopesAreSeparate(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "prefers table-driven tests",
		domain.ScopePrincipal, "discord:user_1"), "")
	remember(t, store, newMemory("mem_2", "the operator's own note",
		domain.ScopePrincipal, "local:operator"), "")
	remember(t, store, newMemory("mem_3", "the project uses buf",
		domain.ScopeWorkspace, "/srv/app"), "")

	found, err := store.Memories(ctx, storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{
			{Scope: domain.ScopePrincipal, Ref: "discord:user_1"},
		},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(found) != 1 || found[0].ID != "mem_1" {
		t.Fatalf("a principal's query returned %d memories: %+v", len(found), found)
	}

	// Asking for two scopes gets both and nothing else.
	both, err := store.Memories(ctx, storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{
			{Scope: domain.ScopeWorkspace, Ref: "/srv/app"},
			{Scope: domain.ScopePrincipal, Ref: "local:operator"},
		},
	})
	if err != nil {
		t.Fatalf("list two scopes: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("two scopes returned %d memories: %+v", len(both), both)
	}
	for _, candidate := range both {
		if candidate.ID == "mem_1" {
			t.Error("another principal's memory came back")
		}
	}
}

// A correction replaces rather than duplicates, and what was believed before
// is still answerable.
func testMemoryCorrectionSupersedes(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "the API is at api.example.com",
		domain.ScopeWorkspace, "/srv/app"), "")

	corrected := newMemory("mem_2", "the API is at api.example.net",
		domain.ScopeWorkspace, "/srv/app")
	corrected.CreatedAt = fixedTime().Add(time.Hour)
	remember(t, store, corrected, "mem_1")

	current, err := store.Memories(ctx, storage.MemoryQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(current) != 1 || current[0].ID != "mem_2" {
		t.Fatalf("what is believed now is %+v", current)
	}

	// The old one is still there, marked, and points at what replaced it.
	old, err := store.Memory(ctx, "mem_1")
	if err != nil {
		t.Fatalf("read the superseded memory: %v", err)
	}
	if old.IsCurrent() {
		t.Error("the corrected memory is still believed")
	}
	if old.SupersededBy != "mem_2" {
		t.Errorf("the old memory points at %q", old.SupersededBy)
	}

	// And it comes back when somebody asks what changed.
	all, err := store.Memories(ctx, storage.MemoryQuery{IncludeInvalidated: true})
	if err != nil {
		t.Fatalf("list with history: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("the history has %d entries", len(all))
	}

	// Correcting something already corrected is a mistake, not a quiet no-op.
	again := newMemory("mem_3", "third guess", domain.ScopeWorkspace, "/srv/app")
	if err := store.Remember(ctx, again, "mem_1"); err == nil {
		t.Error("superseding an already superseded memory was accepted")
	}
}

func testMemorySearchFindsByWord(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "the deploy script needs sudo",
		domain.ScopeWorkspace, "/srv/app"), "")
	remember(t, store, newMemory("mem_2", "tests run with go test -race",
		domain.ScopeWorkspace, "/srv/app"), "")

	found, err := store.SearchMemories(ctx, "deploy", storage.MemoryQuery{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].ID != "mem_1" {
		t.Fatalf("searching for deploy returned %+v", found)
	}

	// Nothing matching is empty rather than everything.
	none, err := store.SearchMemories(ctx, "kubernetes", storage.MemoryQuery{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a word nobody wrote returned %d memories", len(none))
	}
}

// Search must not be a way around the scope filter. The text reaching it comes
// from a model, and a query language is a way to ask for other people's rows.
func testMemorySearchRespectsScope(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "the operator's private note about deploys",
		domain.ScopePrincipal, "local:operator"), "")

	mine := storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{
			{Scope: domain.ScopePrincipal, Ref: "discord:user_1"},
		},
	}

	for _, attempt := range []string{
		"deploys",
		`deploys OR scope_ref:"local:operator"`,
		`" OR "`,
		"*",
	} {
		found, err := store.SearchMemories(ctx, attempt, mine)
		if err != nil {
			// Refusing is fine. Returning somebody else's memory is not.
			continue
		}
		if len(found) != 0 {
			t.Errorf("%q reached %d memories outside its scope", attempt, len(found))
		}
	}
}

// A person who asks the agent to forget something and gets "that stopped being
// true" has not been answered.
func testMemoryForgetActuallyRemoves(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "something regrettable",
		domain.ScopeWorkspace, "/srv/app"), "")

	corrected := newMemory("mem_2", "something better", domain.ScopeWorkspace, "/srv/app")
	corrected.CreatedAt = fixedTime().Add(time.Hour)
	remember(t, store, corrected, "mem_1")

	if err := store.Forget(ctx, "mem_1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if _, err := store.Memory(ctx, "mem_1"); !errors.Is(err, storage.ErrMemoryNotFound) {
		t.Errorf("a forgotten memory is still there: %v", err)
	}

	// Not through the history either.
	all, err := store.Memories(ctx, storage.MemoryQuery{IncludeInvalidated: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, candidate := range all {
		if candidate.ID == "mem_1" {
			t.Error("a forgotten memory is still in the history")
		}
		if candidate.SupersededBy == "mem_1" {
			t.Error("a memory still points at the one that was forgotten")
		}
	}

	// Nor through search, which would be the quiet way for it to survive.
	found, err := store.SearchMemories(ctx, "regrettable", storage.MemoryQuery{
		IncludeInvalidated: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a forgotten memory is still searchable: %+v", found)
	}

	if err := store.Forget(ctx, "mem_absent"); !errors.Is(err, storage.ErrMemoryNotFound) {
		t.Errorf("forgetting nothing gave %v", err)
	}
}

// Provenance is the difference between a claim and a claim you can check, and
// it is what stops untrusted text becoming a trusted fact by being summarised.
func testMemoryProvenanceSurvives(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	fromDiscord := newMemory("mem_1", "the user said they prefer Go",
		domain.ScopePrincipal, "discord:user_1")
	fromDiscord.Trust = domain.TrustUntrusted
	fromDiscord.Origin = domain.RunOrigin{
		Kind: domain.OriginGateway,
		Principal: &domain.ExternalPrincipal{
			Platform:    "discord",
			PrincipalID: "user_1",
		},
	}
	fromDiscord.SourceSession = "ses_42"
	fromDiscord.SourceSeq = 118
	fromDiscord.ApprovedBy = "operator"
	remember(t, store, fromDiscord, "")

	found, err := store.Memory(ctx, "mem_1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if found.Trust != domain.TrustUntrusted {
		t.Errorf("trust came back as %q", found.Trust)
	}
	if found.Origin.Kind != domain.OriginGateway {
		t.Errorf("origin came back as %q", found.Origin.Kind)
	}
	if found.Origin.Principal == nil || found.Origin.Principal.PrincipalID != "user_1" {
		t.Errorf("the principal was lost: %+v", found.Origin.Principal)
	}
	if found.SourceSession != "ses_42" || found.SourceSeq != 118 {
		t.Errorf("the memory cannot say where it came from: %s/%d",
			found.SourceSession, found.SourceSeq)
	}
	if found.ApprovedBy != "operator" {
		t.Errorf("who approved it came back as %q", found.ApprovedBy)
	}
}

// A plan has to survive being written and read back, with every field. A
// status that came back empty would make a finished step look pending.
func testPlanRoundTrip(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)
	session := mustCreateSession(t, store, "ses_plan")

	want := []domain.PlanItem{
		{ID: "todo_1", Title: "read the failing test", Status: domain.PlanCompleted},
		{ID: "todo_2", Title: "fix it", Status: domain.PlanInProgress},
		{ID: "todo_3", Title: "check the others still pass", Status: domain.PlanPending},
		{ID: "todo_4", Title: "rewrite the module", Status: domain.PlanAbandoned,
			Note: "not needed once the timeout was raised"},
	}
	if err := store.SetPlan(ctx, session.ID, want, time.Unix(1, 0)); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	got, err := store.Plan(ctx, session.ID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d items, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Most sessions never make a plan. That is an empty list, not an error.
func testPlanIsEmptyBeforeOneIsMade(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)
	session := mustCreateSession(t, store, "ses_noplan")

	items, err := store.Plan(ctx, session.ID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("a session that made no plan has %d items", len(items))
	}
}

// Writing a plan replaces the one before it. Appending would make a step
// removed from the plan reappear in the next write.
func testPlanReplacesRatherThanAdds(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)
	session := mustCreateSession(t, store, "ses_changing")

	first := []domain.PlanItem{{ID: "todo_1", Title: "one", Status: domain.PlanPending}}
	if err := store.SetPlan(ctx, session.ID, first, time.Unix(1, 0)); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	second := []domain.PlanItem{{ID: "todo_2", Title: "two", Status: domain.PlanPending}}
	if err := store.SetPlan(ctx, session.ID, second, time.Unix(2, 0)); err != nil {
		t.Fatalf("set plan again: %v", err)
	}

	got, err := store.Plan(ctx, session.ID)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(got) != 1 || got[0].ID != "todo_2" {
		t.Errorf("the second write did not replace the first: %+v", got)
	}
}

// A plan belongs to its session. One that leaked would have the agent working
// from another conversation's list.
func testPlansAreSeparatePerSession(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)
	mine := mustCreateSession(t, store, "ses_mine")
	theirs := mustCreateSession(t, store, "ses_theirs")

	if err := store.SetPlan(ctx, mine.ID,
		[]domain.PlanItem{{ID: "todo_1", Title: "mine", Status: domain.PlanPending}},
		time.Unix(1, 0)); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	other, err := store.Plan(ctx, theirs.ID)
	if err != nil {
		t.Fatalf("read the other plan: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a plan leaked into another session: %+v", other)
	}
}

// A fact that was true and stopped being true is not a fact the agent was
// wrong about. Keeping both timelines is what lets a store answer "what did
// it believe when that run happened".
func testMemoryValidityIsSeparateFromBelief(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)

	learned := fixedTime()
	ends := learned.Add(48 * time.Hour)

	memory := newMemory("mem_freeze", "the deploy freeze is on", domain.ScopeWorkspace, "/ws")
	memory.CreatedAt = learned
	memory.ValidFrom = learned
	memory.ValidUntil = &ends

	if err := store.Remember(ctx, memory, ""); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// While it holds.
	during, err := store.Memories(ctx, storage.MemoryQuery{At: learned.Add(time.Hour)})
	if err != nil {
		t.Fatalf("read during: %v", err)
	}
	if len(during) != 1 {
		t.Fatalf("a memory inside its validity was not returned: %d", len(during))
	}

	// After it stops holding — still stored, no longer believed.
	after, err := store.Memories(ctx, storage.MemoryQuery{At: ends.Add(time.Hour)})
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a memory past its validity is still offered: %+v", after)
	}

	// And the history still has it. A fact that expired is not one that was
	// never true.
	history, err := store.Memories(ctx, storage.MemoryQuery{
		At: ends.Add(time.Hour), IncludeInvalidated: true,
	})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("the history lost a memory that stopped being true: %d", len(history))
	}

	// Before it began, either. A fact recorded now about next month is not
	// true now.
	before, err := store.Memories(ctx, storage.MemoryQuery{At: learned.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("a memory was believed before it began: %+v", before)
	}
}

// A correction records two different facts, and losing either one loses an
// answer somebody will want.
//
// invalidated_at is when this agent stopped carrying the old memory.
// valid_until is when the thing it described stopped being true — which is
// where the replacement's validity begins, and is rarely the same moment.
func testSupersedingClosesTheOldValidity(t *testing.T, newStore Factory) {
	ctx, store := context.Background(), newStore(t)

	// Learned in January, true since January.
	learnedInJanuary := fixedTime()
	old := newMemory("mem_v1", "the project is on Go 1.24", domain.ScopeWorkspace, "/ws")
	old.CreatedAt = learnedInJanuary
	old.ValidFrom = learnedInJanuary
	if err := store.Remember(ctx, old, ""); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// Learned in June, about a change that happened in May.
	changedInMay := learnedInJanuary.AddDate(0, 4, 0)
	learnedInJune := learnedInJanuary.AddDate(0, 5, 0)

	replacement := newMemory("mem_v2", "the project is on Go 1.25", domain.ScopeWorkspace, "/ws")
	replacement.CreatedAt = learnedInJune
	replacement.ValidFrom = changedInMay
	if err := store.Remember(ctx, replacement, "mem_v1"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	history, err := store.Memories(ctx, storage.MemoryQuery{IncludeInvalidated: true})
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var corrected *domain.Memory
	for i := range history {
		if history[i].ID == "mem_v1" {
			corrected = &history[i]
		}
	}
	if corrected == nil {
		t.Fatal("the superseded memory is gone")
	}

	if corrected.InvalidatedAt == nil || !corrected.InvalidatedAt.Equal(learnedInJune) {
		t.Errorf("stopped being carried at %v, want when the correction was made", corrected.InvalidatedAt)
	}
	if corrected.ValidUntil == nil || !corrected.ValidUntil.Equal(changedInMay) {
		t.Errorf("stopped being true at %v, want when the world changed", corrected.ValidUntil)
	}

	// The question the two timelines exist to answer: what was true in April,
	// which is after the old fact was learned and before it stopped holding.
	inApril := learnedInJanuary.AddDate(0, 3, 0)
	then, err := store.Memories(ctx, storage.MemoryQuery{
		At: inApril, IncludeInvalidated: true,
	})
	if err != nil {
		t.Fatalf("read history: %v", err)
	}

	var held bool
	for _, memory := range then {
		if memory.ID == "mem_v1" && memory.CurrentAt(inApril) {
			held = true
		}
	}
	if !held {
		t.Error("the old fact does not read as true in April, when it was")
	}
}
