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
	"sync/atomic"
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
		"TheWholeLogHasOneOrder":      testTheWholeLogHasOneOrder,
		"TheLogHeadTracksAppends":     testTheLogHeadTracksAppends,
		"PruningRaisesTheWatermark":   testPruningRaisesTheWatermark,
		"ConcurrentAppendsAreDense":   testConcurrentAppendsAreDense,
		"MemoryRoundTrip":             testMemoryRoundTrip,
		"MemoryScopesAreSeparate":     testMemoryScopesAreSeparate,
		"MemoryCorrectionSupersedes":  testMemoryCorrectionSupersedes,
		"MemorySearchFindsByWord":     testMemorySearchFindsByWord,
		"MemorySearchRespectsScope":   testMemorySearchRespectsScope,
		"MemorySearchFindsChinese":    testMemorySearchFindsChinese,
		"MemoryNearestByVector":       testMemoryNearestByVector,
		"MemoryVectorsAreForgotten":   testMemoryVectorsAreForgotten,
		"MemoryUnembeddedPerModel":    testMemoryUnembeddedPerModel,
		"MemoryForgetActuallyRemoves": testMemoryForgetActuallyRemoves,
		"MemoryProvenanceSurvives":    testMemoryProvenanceSurvives,
		"PlanRoundTrip":               testPlanRoundTrip,
		"PlanIsEmptyBeforeOneIsMade":  testPlanIsEmptyBeforeOneIsMade,
		"PlanReplacesRatherThanAdds":  testPlanReplacesRatherThanAdds,
		"PlansAreSeparatePerSession":  testPlansAreSeparatePerSession,

		"MemoryValidityIsSeparateFromBelief": testMemoryValidityIsSeparateFromBelief,
		"SupersedingClosesTheOldValidity":    testSupersedingClosesTheOldValidity,

		"ScheduleRoundTrip":                  testScheduleRoundTrip,
		"ScheduleListIncludesPaused":         testScheduleListIncludesPaused,
		"EditingASchedulesCountsAsAChange":   testEditingASchedulesCountsAsAChange,
		"PausingIsNotAChange":                testPausingIsNotAChange,
		"OneOccasionResolvesOnce":            testOneOccasionResolvesOnce,
		"LastFiringIsPerRevision":            testLastFiringIsPerRevision,
		"DeletingAScheduleForgetsItsFirings": testDeletingAScheduleForgetsItsFirings,

		"ADocumentNeverWrittenIsTheZeroValue":               testADocumentNeverWrittenIsTheZeroValue,
		"ADocumentIsCreatedByExpectingNothing":              testADocumentIsCreatedByExpectingNothing,
		"ADocumentHashIsSha256HexAndEmptyForEmptyText":      testADocumentHashIsSha256HexAndEmptyForEmptyText,
		"AStaleHashIsRefusedAndWritesNothing":               testAStaleHashIsRefusedAndWritesNothing,
		"AClearedDocumentIsAnEmptyRowThatCanBeWrittenAgain": testAClearedDocumentIsAnEmptyRowThatCanBeWrittenAgain,
		"DocumentVersionsAreNewestFirstAndPrunedToTheLimit": testDocumentVersionsAreNewestFirstAndPrunedToTheLimit,
		"AVersionNeverWrittenOrAlreadyPrunedIsNotFound":     testAVersionNeverWrittenOrAlreadyPrunedIsNotFound,
		"OneHashHasOneWinnerAmongConcurrentWrites":          testOneHashHasOneWinnerAmongConcurrentWrites,
		"SeedingNeverOverwritesAndReturnsOnlyWhatItWrote":   testSeedingNeverOverwritesAndReturnsOnlyWhatItWrote,
		"SeedingDoesNotBringBackAClearedDocument":           testSeedingDoesNotBringBackAClearedDocument,
		"SeedingIsAllOrNothing":                             testSeedingIsAllOrNothing,
		"ANameThatIsNotADocumentIsRefusedEverywhere":        testANameThatIsNotADocumentIsRefusedEverywhere,
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

// eventIDs numbers the events this suite appends.
//
// A counter rather than the clock. time.Now has nanosecond units and not
// nanosecond resolution: on Windows it advances in steps of about a
// millisecond, so a loop appending five events in a row read the same instant
// five times and built the same id five times. What that produced was a
// unique-constraint failure in the store — a real one, about ids this helper
// made up, in a suite checking that the store keeps its promises.
var eventIDs atomic.Uint64

func nextEventID(session domain.SessionID) domain.EventID {
	return domain.EventID(fmt.Sprintf("evt_%s_%d", session, eventIDs.Add(1)))
}

func appendEvent(t *testing.T, store storage.Store, session domain.SessionID, kind domain.EventKind, payload domain.EventPayload) domain.Seq {
	t.Helper()

	seq, err := store.Append(context.Background(), domain.Event{
		ID:         nextEventID(session),
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

// Chinese has no spaces, so a search that only splits on them treats a whole
// sentence as one word: the memory is written, and then nothing short of that
// exact sentence finds it again.
func testMemorySearchFindsChinese(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "使用者偏好用繁體中文回覆",
		domain.ScopeWorkspace, "/srv/app"), "")
	remember(t, store, newMemory("mem_2", "部署腳本需要 sudo 權限",
		domain.ScopeWorkspace, "/srv/app"), "")

	for _, query := range []string{
		"繁體中文",
		"中文",
		"回覆要用什麼語言？請用中文",
	} {
		found, err := store.SearchMemories(ctx, query, storage.MemoryQuery{})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(found) != 1 || found[0].ID != "mem_1" {
			t.Errorf("searching for %q returned %+v", query, found)
		}
	}

	none, err := store.SearchMemories(ctx, "資料庫遷移", storage.MemoryQuery{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("words nobody wrote returned %d memories", len(none))
	}
}

// Nearest is closest first, nothing below the floor, nothing outside the
// query's scopes, and nothing from another model.
func testMemoryNearestByVector(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_near", "near", domain.ScopeWorkspace, "/srv/app"), "")
	remember(t, store, newMemory("mem_nearer", "nearer", domain.ScopeWorkspace, "/srv/app"), "")
	remember(t, store, newMemory("mem_far", "far", domain.ScopeWorkspace, "/srv/app"), "")
	remember(t, store, newMemory("mem_theirs", "theirs", domain.ScopePrincipal, "discord:1"), "")
	remember(t, store, newMemory("mem_other_model", "other", domain.ScopeWorkspace, "/srv/app"), "")

	for id, vector := range map[domain.MemoryID][]float32{
		"mem_near":   {1, 1, 0},
		"mem_nearer": {1, 0.1, 0},
		"mem_far":    {0, 0, 1},
		"mem_theirs": {1, 0, 0},
	} {
		if err := store.SetMemoryVector(ctx, id, "model-a", vector); err != nil {
			t.Fatalf("set vector %s: %v", id, err)
		}
	}
	if err := store.SetMemoryVector(ctx, "mem_other_model", "model-b", []float32{1, 0, 0}); err != nil {
		t.Fatalf("set vector: %v", err)
	}

	found, err := store.NearestMemories(ctx, "model-a", []float32{1, 0, 0}, 0.5, storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{{Scope: domain.ScopeWorkspace, Ref: "/srv/app"}},
	})
	if err != nil {
		t.Fatalf("nearest: %v", err)
	}
	var ids []domain.MemoryID
	for _, memory := range found {
		ids = append(ids, memory.ID)
	}
	if len(ids) != 2 || ids[0] != "mem_nearer" || ids[1] != "mem_near" {
		t.Errorf("nearest returned %v, want [mem_nearer mem_near]", ids)
	}
	if len(found) > 0 && found[0].Text != "nearer" {
		t.Errorf("nearest returned a memory without its text: %+v", found[0])
	}

	limited, err := store.NearestMemories(ctx, "model-a", []float32{1, 0, 0}, 0.5, storage.MemoryQuery{
		Scopes: []storage.MemoryScopeRef{{Scope: domain.ScopeWorkspace, Ref: "/srv/app"}},
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("nearest: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "mem_nearer" {
		t.Errorf("a limit of one returned %+v", limited)
	}
}

// A memory forgotten is forgotten by meaning too.
func testMemoryVectorsAreForgotten(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "something regrettable", domain.ScopeWorkspace, "/srv/app"), "")
	if err := store.SetMemoryVector(ctx, "mem_1", "model-a", []float32{1, 0}); err != nil {
		t.Fatalf("set vector: %v", err)
	}
	if err := store.Forget(ctx, "mem_1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	found, err := store.NearestMemories(ctx, "model-a", []float32{1, 0}, 0, storage.MemoryQuery{
		IncludeInvalidated: true,
	})
	if err != nil {
		t.Fatalf("nearest: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a forgotten memory is still found by meaning: %+v", found)
	}
}

// What is left to embed is per model, and leaves out what is no longer
// believed.
func testMemoryUnembeddedPerModel(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	remember(t, store, newMemory("mem_1", "first", domain.ScopeWorkspace, "/srv/app"), "")
	second := newMemory("mem_2", "second", domain.ScopeWorkspace, "/srv/app")
	second.CreatedAt = fixedTime().Add(time.Hour)
	remember(t, store, second, "")
	corrected := newMemory("mem_3", "third", domain.ScopeWorkspace, "/srv/app")
	corrected.CreatedAt = fixedTime().Add(2 * time.Hour)
	remember(t, store, corrected, "mem_2")

	if err := store.SetMemoryVector(ctx, "mem_1", "model-a", []float32{1}); err != nil {
		t.Fatalf("set vector: %v", err)
	}

	left, err := store.UnembeddedMemories(ctx, "model-a", 0)
	if err != nil {
		t.Fatalf("unembedded: %v", err)
	}
	if len(left) != 1 || left[0].ID != "mem_3" {
		t.Errorf("left to embed for model-a: %+v, want only mem_3", left)
	}

	other, err := store.UnembeddedMemories(ctx, "model-b", 1)
	if err != nil {
		t.Fatalf("unembedded: %v", err)
	}
	if len(other) != 1 || other[0].ID != "mem_1" {
		t.Errorf("left to embed for model-b with a limit of one: %+v, want mem_1", other)
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

// Two sessions both at seq 3 make "I have read up to 3" mean nothing, which
// is what a console watching every session at once has to be able to say.
func testTheWholeLogHasOneOrder(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	first := mustCreateSession(t, store, "ses_1")
	second := mustCreateSession(t, store, "ses_2")

	// Interleaved, which is the case a per-session number cannot describe.
	said := []string{"one", "two", "three", "four"}
	for index, text := range said {
		session := first.ID
		if index%2 == 1 {
			session = second.ID
		}
		appendEvent(t, store, session, domain.EventAssistantTextDelta,
			domain.AssistantTextDelta{Text: text})
	}

	everything, err := store.ListAllAfter(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list the log: %v", err)
	}
	if len(everything) != len(said) {
		t.Fatalf("the log holds %d events, want %d", len(everything), len(said))
	}

	for index, event := range everything {
		if want := domain.Seq(index + 1); event.GlobalSeq != want {
			t.Errorf("event %d is at %d, want %d", index, event.GlobalSeq, want)
		}
		if got := event.Payload.(domain.AssistantTextDelta).Text; got != said[index] {
			t.Errorf("position %d holds %q, want %q", index+1, got, said[index])
		}
	}

	rest, err := store.ListAllAfter(ctx, 2, 0)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(rest) != 2 || rest[0].GlobalSeq != 3 {
		t.Fatalf("resuming after 2 gave %d events starting at %d, want 2 starting at 3",
			len(rest), rest[0].GlobalSeq)
	}
}

func testTheLogHeadTracksAppends(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	session := mustCreateSession(t, store, "ses_1")

	if head, err := store.LogHead(ctx); err != nil || head != 0 {
		t.Fatalf("an empty log's head is %d (err %v), want 0", head, err)
	}

	appendEvent(t, store, session.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "a"})
	appendEvent(t, store, session.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "b"})

	head, err := store.LogHead(ctx)
	if err != nil {
		t.Fatalf("log head: %v", err)
	}
	if head != 2 {
		t.Errorf("the log head is %d, want 2", head)
	}
}

// Pruning leaves gaps, which is fine. What is not fine is a client resuming
// into one and being told nothing happened, so how far the log has been
// discarded has to be answerable — and it must only ever rise, since sessions
// are pruned in no particular order.
func testPruningRaisesTheWatermark(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	first := mustCreateSession(t, store, "ses_1")
	second := mustCreateSession(t, store, "ses_2")

	appendEvent(t, store, second.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "a"})
	appendEvent(t, store, first.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "b"})
	appendEvent(t, store, first.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "c"})
	appendEvent(t, store, second.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{Text: "d"})

	if through, err := store.LogPrunedThrough(ctx); err != nil || through != 0 {
		t.Fatalf("a log that has lost nothing says %d (err %v), want 0", through, err)
	}

	// The first session holds positions 2 and 3.
	if _, err := store.PruneEvents(ctx, first.ID, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if through, err := store.LogPrunedThrough(ctx); err != nil || through != 3 {
		t.Fatalf("the watermark is %d (err %v), want 3", through, err)
	}

	// The second session holds position 1, which is lower. The mark stays.
	if _, err := store.PruneEvents(ctx, second.ID, 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if through, err := store.LogPrunedThrough(ctx); err != nil || through != 3 {
		t.Errorf("the watermark moved to %d (err %v); it must stay at 3", through, err)
	}
}

// aSchedule is one standing instruction, with a session to belong to.
func aSchedule(t *testing.T, store storage.Store, id domain.ScheduleID) domain.Schedule {
	t.Helper()

	ctx := context.Background()
	// The session is a fixture rather than the thing under test, so a test
	// that asks for the same schedule twice — deleting and recreating one —
	// gets the session it already made.
	session := domain.Session{ID: domain.SessionID("ses_" + string(id)), Title: "scheduled"}
	if err := store.CreateSession(ctx, session); err != nil &&
		!errors.Is(err, storage.ErrDuplicateSession) {
		t.Fatalf("create session: %v", err)
	}

	return domain.Schedule{
		ID:         id,
		Revision:   1,
		Expression: "0 9 * * *",
		Zone:       "Asia/Taipei",
		Prompt:     "what changed yesterday",
		SessionID:  session.ID,
		CreatedBy:  domain.FromTheMachine("cli"),
		Deliver:    []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: "cli"}},
		CreatedAt:  time.Unix(1000, 0).UTC(),
	}
}

func testScheduleRoundTrip(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	want := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateSchedule(ctx, want); !errors.Is(err, storage.ErrDuplicateSchedule) {
		t.Errorf("creating it twice: %v", err)
	}

	got, err := store.Schedule(ctx, want.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if got.Expression != want.Expression || got.Zone != want.Zone || got.Prompt != want.Prompt {
		t.Errorf("what it does did not survive: %+v", got)
	}
	// Who set it up is attribution and has to survive, because it is the only
	// record of who authorized an automation that acts on its own.
	if got.CreatedBy.Kind != want.CreatedBy.Kind {
		t.Errorf("who created it did not survive: %+v", got.CreatedBy)
	}
	// Where the answer goes is stored, never inferred later from anything.
	if len(got.Deliver) != 1 || got.Deliver[0].Ref != "cli" {
		t.Errorf("where it delivers did not survive: %+v", got.Deliver)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created at %v, want %v", got.CreatedAt, want.CreatedAt)
	}

	if _, err := store.Schedule(ctx, "sch_missing"); !errors.Is(err, storage.ErrScheduleNotFound) {
		t.Errorf("a schedule that is not there: %v", err)
	}
}

func testScheduleListIncludesPaused(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	for _, id := range []domain.ScheduleID{"sch_1", "sch_2"} {
		if err := store.CreateSchedule(ctx, aSchedule(t, store, id)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if err := store.SetSchedulePaused(ctx, "sch_2", true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Paused ones included: the commonest reason to look at this list is to
	// find the one to turn back on.
	listed, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("want two schedules, got %d", len(listed))
	}

	var paused int
	for _, one := range listed {
		if one.Paused {
			paused++
		}
	}
	if paused != 1 {
		t.Errorf("want one paused, got %d", paused)
	}
}

// testEditingASchedulesCountsAsAChange is what stops an edit disturbing the
// occasions already accounted for.
func testEditingASchedulesCountsAsAChange(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	schedule := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}

	schedule.Expression = "0 18 * * *"
	if err := store.UpdateSchedule(ctx, schedule); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := store.Schedule(ctx, schedule.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Expression != "0 18 * * *" {
		t.Errorf("the change did not take: %q", got.Expression)
	}
	// A schedule that changed is a different instruction, and the firings
	// resolved under the old one belong to the old one.
	if got.Revision != 2 {
		t.Errorf("revision %d, want 2", got.Revision)
	}

	err = store.UpdateSchedule(ctx, domain.Schedule{ID: "sch_missing"})
	if !errors.Is(err, storage.ErrScheduleNotFound) {
		t.Errorf("updating one that is not there: %v", err)
	}
}

// testPausingIsNotAChange is the other half.
//
// Pausing does not alter what the schedule means. Counting it would orphan
// every occasion already resolved, and turning a schedule off and on again
// would make it owe its whole history.
func testPausingIsNotAChange(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	schedule := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetSchedulePaused(ctx, schedule.ID, true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := store.SetSchedulePaused(ctx, schedule.ID, false); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got, err := store.Schedule(ctx, schedule.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Revision != 1 {
		t.Errorf("pausing counted as %d changes", got.Revision-1)
	}
	if got.Paused {
		t.Error("it did not resume")
	}
}

// testOneOccasionResolvesOnce is what makes reconciling idempotent.
//
// A daemon restarting twice in a minute, or two of them starting a second
// apart, must produce one run for one three o'clock.
func testOneOccasionResolvesOnce(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	schedule := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}

	due := time.Unix(2000, 0).UTC()
	firing := domain.Firing{
		ScheduleID: schedule.ID, Revision: 1,
		For: due, Observed: time.Unix(2100, 0).UTC(), Missed: 3, RunID: "run_1",
	}

	if err := store.ResolveFiring(ctx, firing); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The second attempt is refused, and that refusal is the answer to the
	// question rather than a failure to report.
	second := firing
	second.Observed = time.Unix(2200, 0).UTC()
	second.RunID = "run_2"
	if err := store.ResolveFiring(ctx, second); !errors.Is(err, storage.ErrFiringAlreadyResolved) {
		t.Errorf("the same occasion resolved twice: %v", err)
	}

	last, err := store.LastFiring(ctx, schedule.ID, 1)
	if err != nil {
		t.Fatalf("last firing: %v", err)
	}
	if !last.Equal(due) {
		t.Errorf("last firing %v, want %v", last, due)
	}
}

// testLastFiringIsPerRevision keeps an edit from skipping the new
// instruction's first occasion.
func testLastFiringIsPerRevision(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	schedule := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Nothing has happened yet, which is the zero time rather than an error.
	last, err := store.LastFiring(ctx, schedule.ID, 1)
	if err != nil {
		t.Fatalf("last firing: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("a schedule that never fired reports %v", last)
	}

	if err := store.ResolveFiring(ctx, domain.Firing{
		ScheduleID: schedule.ID, Revision: 1,
		For: time.Unix(2000, 0).UTC(), Observed: time.Unix(2000, 0).UTC(),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The next revision has its own account. Asking what the old instruction
	// last did would let an edit skip the new one's first occasion.
	last, err = store.LastFiring(ctx, schedule.ID, 2)
	if err != nil {
		t.Fatalf("last firing at the new revision: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("the new revision inherited %v from the old one", last)
	}
}

func testDeletingAScheduleForgetsItsFirings(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	schedule := aSchedule(t, store, "sch_1")

	if err := store.CreateSchedule(ctx, schedule); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.ResolveFiring(ctx, domain.Firing{
		ScheduleID: schedule.ID, Revision: 1,
		For: time.Unix(2000, 0).UTC(), Observed: time.Unix(2000, 0).UTC(),
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if err := store.DeleteSchedule(ctx, schedule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Schedule(ctx, schedule.ID); !errors.Is(err, storage.ErrScheduleNotFound) {
		t.Errorf("it survived deletion: %v", err)
	}
	if err := store.DeleteSchedule(ctx, schedule.ID); !errors.Is(err, storage.ErrScheduleNotFound) {
		t.Errorf("deleting it twice: %v", err)
	}

	// And its account goes with it. One created again under the same name
	// would otherwise inherit occasions it never had.
	recreated := aSchedule(t, store, "sch_1")
	recreated.SessionID = schedule.SessionID
	if err := store.CreateSchedule(ctx, recreated); err != nil {
		t.Fatalf("create again: %v", err)
	}
	last, err := store.LastFiring(ctx, recreated.ID, 1)
	if err != nil {
		t.Fatalf("last firing: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("a schedule created again inherited a firing at %v", last)
	}
}

// sha256 of "hello" and of the second text below, worked out outside this
// codebase. The suite checks the value rather than calling a hashing
// function, because the stores must agree with config.Hash, which this
// package cannot import, and an implementation of both that was wrong the
// same way would pass a test written with the same function.
const (
	helloHash = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	// Multi-byte text, to show it is the UTF-8 bytes that are hashed.
	settingsText = "[server]\naddr = \"127.0.0.1:7777\"\n# 設定\n"
	settingsHash = "def21b8088ad57c3eef2dfd9256f50b3704c2378474f53fec00e4ccefd530576"
)

// sameDocument compares what a store promises about a document. The time by
// instant: one store hands back the time it was given and the other one it
// has read back as a number.
func sameDocument(got, want storage.Document) bool {
	return got.Name == want.Name && got.Text == want.Text && got.Hash == want.Hash &&
		got.Source == want.Source && got.Version == want.Version &&
		got.UpdatedAt.Equal(want.UpdatedAt)
}

func mustPutDocument(
	t *testing.T, store storage.Store, name, text, expect string,
) storage.Document {
	t.Helper()

	doc, err := store.PutDocument(context.Background(), name, text, "web", expect, fixedTime())
	if err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
	return doc
}

func testADocumentNeverWrittenIsTheZeroValue(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	for _, name := range []string{"config", "agents", "persona"} {
		got, err := store.Document(ctx, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Absent is a state, not a failure: everything that reads a document
		// treats one never written as empty.
		if !sameDocument(got, storage.Document{Name: name}) {
			t.Errorf("%s was never written but reads as %+v", name, got)
		}

		versions, err := store.DocumentVersions(ctx, name)
		if err != nil || len(versions) != 0 {
			t.Errorf("%s has no history yet but lists %d versions (err %v)", name, len(versions), err)
		}
	}
}

func testADocumentIsCreatedByExpectingNothing(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	created, err := store.PutDocument(ctx, "config", "hello", "env", "", fixedTime())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := storage.Document{
		Name: "config", Text: "hello", Hash: helloHash, Source: "env",
		Version: 1, UpdatedAt: fixedTime(),
	}
	if !sameDocument(created, want) {
		t.Errorf("created %+v, want %+v", created, want)
	}

	got, err := store.Document(ctx, "config")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !sameDocument(got, want) {
		t.Errorf("read back %+v, want %+v", got, want)
	}

	// The next edit starts from what it read, and counts as the next version
	// from whoever it came.
	later := fixedTime().Add(time.Hour)
	updated, err := store.PutDocument(ctx, "config", "hello again", "web", created.Hash, later)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || updated.Source != "web" || !updated.UpdatedAt.Equal(later) {
		t.Errorf("updated is %+v", updated)
	}

	// Documents are separate: writing one is not writing the others.
	if other, err := store.Document(ctx, "agents"); err != nil || other.Version != 0 {
		t.Errorf("writing config touched agents: %+v (err %v)", other, err)
	}
}

func testADocumentHashIsSha256HexAndEmptyForEmptyText(t *testing.T, newStore Factory) {
	store := newStore(t)

	if doc := mustPutDocument(t, store, "config", settingsText, ""); doc.Hash != settingsHash {
		t.Errorf("hash of multi-byte text is %q, want %q", doc.Hash, settingsHash)
	}

	// Empty text hashes to nothing, not to the hash of nothing, so a cleared
	// document and one never written are the same token to an editor.
	cleared := mustPutDocument(t, store, "config", "", settingsHash)
	if cleared.Hash != "" {
		t.Errorf("empty text hashes to %q, want the empty string", cleared.Hash)
	}
}

func testAStaleHashIsRefusedAndWritesNothing(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	// Against a document that does not exist: only "" is the hash of nothing.
	_, err := store.PutDocument(ctx, "config", "first", "web", "not-a-hash", fixedTime())
	if !errors.Is(err, storage.ErrDocumentChanged) {
		t.Errorf("writing over a document that is not there, from a hash: %v", err)
	}
	if got, err := store.Document(ctx, "config"); err != nil || got.Version != 0 {
		t.Errorf("the refused write created the document: %+v (err %v)", got, err)
	}

	current := mustPutDocument(t, store, "config", "mine", "")

	for name, expect := range map[string]string{
		"another document's hash":   helloHash,
		"nothing, after it was set": "",
	} {
		_, err := store.PutDocument(ctx, "config", "theirs", "web", expect, fixedTime())
		if !errors.Is(err, storage.ErrDocumentChanged) {
			t.Errorf("expecting %s: %v", name, err)
		}
	}

	got, err := store.Document(ctx, "config")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !sameDocument(got, current) {
		t.Errorf("a refused write changed the document: %+v, want %+v", got, current)
	}
	// Nor does it leave a version behind: the history is of what was saved.
	if versions, err := store.DocumentVersions(ctx, "config"); err != nil || len(versions) != 1 {
		t.Errorf("history has %d versions after a refused write (err %v), want 1", len(versions), err)
	}
}

// testAClearedDocumentIsAnEmptyRowThatCanBeWrittenAgain is the other half of
// "absent and cleared are the same token": one is a row and the other is not,
// and the row has to stay, or an env seed brings back what was removed.
func testAClearedDocumentIsAnEmptyRowThatCanBeWrittenAgain(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	written := mustPutDocument(t, store, "persona", "be brief", "")
	cleared := mustPutDocument(t, store, "persona", "", written.Hash)
	if cleared.Text != "" || cleared.Hash != "" || cleared.Version != 2 {
		t.Fatalf("cleared is %+v, want an empty second version", cleared)
	}

	got, err := store.Document(ctx, "persona")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// Version 2 and not 0: it is a row that says nothing, not a missing one.
	if got.Version != 2 || got.Text != "" || got.Hash != "" {
		t.Errorf("a cleared document reads as %+v", got)
	}

	// The hash it had before is stale now, and "" is the token for what it is.
	_, err = store.PutDocument(ctx, "persona", "stale", "web", written.Hash, fixedTime())
	if !errors.Is(err, storage.ErrDocumentChanged) {
		t.Errorf("writing from the hash before it was cleared: %v", err)
	}

	again := mustPutDocument(t, store, "persona", "be brief again", "")
	if again.Version != 3 {
		t.Errorf("written again as version %d, want 3: numbering carries on", again.Version)
	}
}

func testDocumentVersionsAreNewestFirstAndPrunedToTheLimit(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	total := storage.DocumentVersionsKept + 5
	var current storage.Document
	for i := 1; i <= total; i++ {
		current = mustPutDocument(t, store, "config", fmt.Sprintf("text %d", i), current.Hash)
	}
	// Another document's history is its own, and is not touched by this one
	// being pruned.
	mustPutDocument(t, store, "agents", "rules", "")

	versions, err := store.DocumentVersions(ctx, "config")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != storage.DocumentVersionsKept {
		t.Fatalf("kept %d versions, want %d", len(versions), storage.DocumentVersionsKept)
	}
	for i, one := range versions {
		wantVersion := int64(total - i)
		if one.Version != wantVersion || one.Text != fmt.Sprintf("text %d", wantVersion) {
			t.Errorf("entry %d is version %d %q, want version %d: newest first",
				i, one.Version, one.Text, wantVersion)
		}
	}

	// The current version is in the history, so restoring to what is live is
	// not a special case.
	if !sameDocument(versions[0], current) {
		t.Errorf("newest in history %+v, current %+v", versions[0], current)
	}

	oldest := int64(total - storage.DocumentVersionsKept + 1)
	got, err := store.DocumentVersion(ctx, "config", oldest)
	if err != nil || got.Text != fmt.Sprintf("text %d", oldest) {
		t.Errorf("the oldest kept version %d: %+v (err %v)", oldest, got, err)
	}
	if got.Hash != storage.DocumentHash(got.Text) {
		t.Errorf("a version read back has hash %q, which is not its text's", got.Hash)
	}

	if other, err := store.DocumentVersions(ctx, "agents"); err != nil || len(other) != 1 {
		t.Errorf("agents has %d versions (err %v), want 1", len(other), err)
	}
}

func testAVersionNeverWrittenOrAlreadyPrunedIsNotFound(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.DocumentVersion(ctx, "config", 1); !errors.Is(err, storage.ErrDocumentVersionNotFound) {
		t.Errorf("a version of a document never written: %v", err)
	}

	var current storage.Document
	for i := 1; i <= storage.DocumentVersionsKept+1; i++ {
		current = mustPutDocument(t, store, "config", fmt.Sprintf("text %d", i), current.Hash)
	}

	for name, version := range map[string]int64{
		"already pruned":  1,
		"zero":            0,
		"not yet written": current.Version + 1,
		"negative":        -1,
	} {
		if _, err := store.DocumentVersion(ctx, "config", version); !errors.Is(err, storage.ErrDocumentVersionNotFound) {
			t.Errorf("version %d (%s): %v", version, name, err)
		}
	}

	// Asking for another document's version is not finding it there.
	if _, err := store.DocumentVersion(ctx, "agents", current.Version); !errors.Is(err, storage.ErrDocumentVersionNotFound) {
		t.Errorf("a version number from another document: %v", err)
	}
}

// testOneHashHasOneWinnerAmongConcurrentWrites is what the hash is for: two
// editors who read the same version cannot both save over it.
func testOneHashHasOneWinnerAmongConcurrentWrites(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	start := mustPutDocument(t, store, "config", "start", "")

	const editors = 8
	results := make(chan error, editors)
	var wg sync.WaitGroup
	for i := range editors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.PutDocument(ctx, "config", fmt.Sprintf("editor %d", i), "web", start.Hash, fixedTime())
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var won, refused int
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, storage.ErrDocumentChanged):
			refused++
		default:
			t.Errorf("an editor failed for another reason: %v", err)
		}
	}
	if won != 1 || refused != editors-1 {
		t.Errorf("%d editors won and %d were refused, want 1 and %d", won, refused, editors-1)
	}

	// One write, one new version: the history has no trace of the others.
	versions, err := store.DocumentVersions(ctx, "config")
	if err != nil || len(versions) != 2 {
		t.Errorf("history has %d versions (err %v), want 2", len(versions), err)
	}
}

func testSeedingNeverOverwritesAndReturnsOnlyWhatItWrote(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	existing := mustPutDocument(t, store, "config", "operator's", "")

	written, err := store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: "config", Text: "from the environment", Source: "env"},
		{Name: "persona", Text: "be brief", Source: "env"},
		{Name: "agents", Text: "", Source: "file"},
	}, fixedTime().Add(time.Hour))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !reflect.DeepEqual(written, []string{"persona", "agents"}) {
		t.Errorf("wrote %v, want [persona agents]: only what was missing, in the order given", written)
	}

	got, err := store.Document(ctx, "config")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !sameDocument(got, existing) {
		t.Errorf("a seed changed a document that existed: %+v, want %+v", got, existing)
	}
	if versions, err := store.DocumentVersions(ctx, "config"); err != nil || len(versions) != 1 {
		t.Errorf("a seed that wrote nothing left %d versions of config (err %v)", len(versions), err)
	}

	persona, err := store.Document(ctx, "persona")
	if err != nil {
		t.Fatalf("read persona: %v", err)
	}
	wantPersona := storage.Document{
		Name: "persona", Text: "be brief", Hash: storage.DocumentHash("be brief"),
		Source: "env", Version: 1, UpdatedAt: fixedTime().Add(time.Hour),
	}
	if !sameDocument(persona, wantPersona) {
		t.Errorf("seeded persona is %+v, want %+v", persona, wantPersona)
	}
	if versions, err := store.DocumentVersions(ctx, "persona"); err != nil || len(versions) != 1 {
		t.Errorf("a seeded document has %d versions (err %v), want its first", len(versions), err)
	}

	// Seeding is idempotent: the second time everything is there.
	again, err := store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: "persona", Text: "different", Source: "env"},
	}, fixedTime())
	if err != nil || len(again) != 0 {
		t.Errorf("seeding what exists wrote %v (err %v)", again, err)
	}
}

// testSeedingDoesNotBringBackAClearedDocument is why a cleared document is a
// row: the environment still has the text the operator removed.
func testSeedingDoesNotBringBackAClearedDocument(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	written := mustPutDocument(t, store, "agents", "old rules", "")
	mustPutDocument(t, store, "agents", "", written.Hash)

	seeded, err := store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: "agents", Text: "old rules", Source: "env"},
	}, fixedTime())
	if err != nil || len(seeded) != 0 {
		t.Errorf("seeding over a cleared document wrote %v (err %v)", seeded, err)
	}
	if got, err := store.Document(ctx, "agents"); err != nil || got.Text != "" {
		t.Errorf("the cleared document came back as %q (err %v)", got.Text, err)
	}
}

func testSeedingIsAllOrNothing(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()

	// The one that cannot be written comes last, so a store that wrote as it
	// went would already have written the first.
	written, err := store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: "config", Text: "fine", Source: "env"},
		{Name: "not-a-document", Text: "refused", Source: "env"},
	}, fixedTime())
	if err == nil {
		t.Fatalf("seeding a document that cannot exist succeeded, wrote %v", written)
	}
	if len(written) != 0 {
		t.Errorf("a failed seed reports having written %v", written)
	}

	got, readErr := store.Document(ctx, "config")
	if readErr != nil || got.Version != 0 {
		t.Errorf("config was written although the seed failed: %+v (err %v)", got, readErr)
	}
	if versions, err := store.DocumentVersions(ctx, "config"); err != nil || len(versions) != 0 {
		t.Errorf("a failed seed left %d versions behind (err %v)", len(versions), err)
	}
}

func testANameThatIsNotADocumentIsRefusedEverywhere(t *testing.T, newStore Factory) {
	store := newStore(t)
	ctx := context.Background()
	const name = "secrets"

	if _, err := store.Document(ctx, name); err == nil {
		t.Error("read a document that cannot exist")
	}
	// Both ways of expecting: the refusal is about the name, not about a hash
	// that happens to be wrong.
	for _, expect := range []string{"", helloHash} {
		if _, err := store.PutDocument(ctx, name, "x", "web", expect, fixedTime()); err == nil {
			t.Errorf("wrote a document that cannot exist, expecting %q", expect)
		}
	}
	if _, err := store.DocumentVersions(ctx, name); err == nil {
		t.Error("listed the versions of a document that cannot exist")
	}
	if _, err := store.DocumentVersion(ctx, name, 1); err == nil {
		t.Error("read a version of a document that cannot exist")
	}
}

// The ids this suite makes up are its own to keep unique.
//
// Exported so the packages that run the suite check it: a duplicate here
// fails as a unique-constraint violation in the store, which reads as the
// store breaking a promise rather than as the suite handing it the same id
// twice.
func TestIDsAreUniqueWithoutTheClock(t *testing.T) {
	seen := map[domain.EventID]bool{}
	for range 1000 {
		id := nextEventID("ses_1")
		if seen[id] {
			t.Fatalf("%q was handed out twice", id)
		}
		seen[id] = true
	}
}
