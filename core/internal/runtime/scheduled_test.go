package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// scheduled stores one schedule and returns the harness around it.
func scheduled(
	t *testing.T, turns [][]provider.Event, expression string,
) (*runtime.Runtime, *memory.Store, domain.Schedule) {
	t.Helper()

	rt, store, _, _ := newToolHarness(t, turns)

	// A permission engine, because the profile is the thing under test. The
	// bare harness has none, which allows everything — and a check for "a
	// scheduled run cannot write" against a runtime that permits every write
	// is a check that cannot fail.
	rt.SetPermissions(permission.New(permission.LocalProfile()))

	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "scheduled")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Before the clock the harness reports, so a firing is owed rather than
	// the schedule looking as though it were made in the future.
	one := domain.Schedule{
		ID:         "sch_1",
		Revision:   1,
		Expression: expression,
		Zone:       "UTC",
		Prompt:     "what changed",
		SessionID:  session.ID,
		CreatedBy:  domain.FromTheMachine("cli"),
		CreatedAt:  time.Unix(0, 0).UTC().Add(-24 * time.Hour),
	}
	if err := store.CreateSchedule(ctx, one); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return rt, store, one
}

func scheduledRuns(t *testing.T, store *memory.Store, session domain.SessionID) []domain.Run {
	t.Helper()

	runs, err := store.ListRuns(context.Background(), session)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	var scheduled []domain.Run
	for _, run := range runs {
		if run.Origin.Kind == domain.OriginSchedule {
			scheduled = append(scheduled, run)
		}
	}
	return scheduled
}

func TestADueScheduleStartsARunThatSaysWhereItCameFrom(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{provider.TextDelta{Text: "Nothing changed."}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "* * * * *")
	ctx := context.Background()

	started, err := rt.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if started != 1 {
		t.Fatalf("started %d runs, want 1", started)
	}

	runs := scheduledRuns(t, store, one.SessionID)
	if len(runs) != 1 {
		t.Fatalf("want one scheduled run, got %d", len(runs))
	}

	// The schedule is who acts. Creating one is delegation; running one is
	// not the person who set it up still acting while they are asleep.
	if runs[0].Origin.ClientID != string(one.ID) {
		t.Errorf("the run does not name the schedule: %+v", runs[0].Origin)
	}
}

// TestOneOccasionBecomesOneRunHoweverOftenWeAsk is what makes a daemon safe
// to restart.
func TestOneOccasionBecomesOneRunHoweverOftenWeAsk(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{provider.TextDelta{Text: "Nothing."}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "* * * * *")
	ctx := context.Background()

	first, err := rt.Reconcile(ctx)
	if err != nil || first != 1 {
		t.Fatalf("first reconcile: %d %v", first, err)
	}
	waitForRun(t, rt, scheduledRuns(t, store, one.SessionID)[0].ID)

	// Asked again, as a restart would.
	second, err := rt.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if second != 0 {
		t.Errorf("the same occasion started %d more runs", second)
	}
	if got := len(scheduledRuns(t, store, one.SessionID)); got != 1 {
		t.Errorf("one occasion became %d runs", got)
	}
}

// TestAScheduleStillRunningIsNotStartedAgain keeps a slow answer from
// accumulating agents.
//
// A schedule whose answer takes longer than its interval would otherwise pile
// up runs until something gave way, and the failure would arrive as a machine
// grinding rather than as a schedule that was too eager.
func TestAScheduleStillRunningIsNotStartedAgain(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{provider.TextDelta{Text: "slow"}, provider.Completed{StopReason: domain.StopEndTurn}},
		{provider.TextDelta{Text: "second"}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "* * * * *")
	ctx := context.Background()

	// A run of this schedule's that is already going, and nothing resolved
	// against it — so the occasion below is genuinely owed. Without both, a
	// reconcile that started nothing would prove only that nothing was due.
	earlier, _, err := rt.SendTurnTo(ctx, one.SessionID, domain.Turn{
		Text:   "an earlier firing",
		Origin: domain.FromASchedule(one.ID),
	})
	if err != nil {
		t.Fatalf("start the earlier run: %v", err)
	}
	waitForRun(t, rt, earlier)

	running, err := store.Run(ctx, earlier)
	if err != nil {
		t.Fatalf("read the earlier run: %v", err)
	}
	running.Status = domain.RunRunning
	running.FinishedAt = nil
	if err := store.UpdateRun(ctx, running); err != nil {
		t.Fatalf("make it look unfinished: %v", err)
	}

	// The precondition: something is owed. A reconcile with a finished run
	// would start it, which is what says the refusal below is the overlap
	// check and not an empty schedule.
	last, err := store.LastFiring(ctx, one.ID, one.Revision)
	if err != nil {
		t.Fatalf("last firing: %v", err)
	}
	if !last.IsZero() {
		t.Fatalf("something was already resolved: %v", last)
	}

	started, err := rt.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if started != 0 {
		t.Errorf("a schedule started %d runs while its last one was still going", started)
	}
	if got := len(scheduledRuns(t, store, one.SessionID)); got != 1 {
		t.Errorf("want the one run that was already going, got %d", got)
	}

	// And the occasion was not consumed: once the run finishes, it is still
	// owed. Claiming it while declining to run it would lose the firing.
	if last, err = store.LastFiring(ctx, one.ID, one.Revision); err != nil {
		t.Fatalf("last firing: %v", err)
	}
	if !last.IsZero() {
		t.Error("the occasion was marked done by a reconcile that ran nothing")
	}
}

// TestAPausedScheduleStartsNothing.
func TestAPausedScheduleStartsNothing(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{provider.TextDelta{Text: "x"}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "* * * * *")
	ctx := context.Background()

	if err := store.SetSchedulePaused(ctx, one.ID, true); err != nil {
		t.Fatalf("pause: %v", err)
	}

	started, err := rt.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if started != 0 {
		t.Errorf("a paused schedule started %d runs", started)
	}
}

// TestALateRunIsToldItIsLate, because it changes the answer.
//
// A digest arriving five hours after it was due is about a different window
// than the question implies, and a model that does not know will write as
// though it is on time.
func TestALateRunIsToldItIsLate(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{provider.TextDelta{Text: "x"}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "0 * * * *")
	ctx := context.Background()

	if _, err := rt.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	events, err := store.ListAfter(ctx, one.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var asked string
	for _, event := range events {
		if said, ok := event.Payload.(domain.UserMessageAdded); ok {
			asked = said.Text
		}
	}
	if asked == "" {
		t.Fatal("nothing was asked")
	}
	if !strings.Contains(asked, one.Prompt) {
		t.Errorf("the question was lost: %q", asked)
	}
	// Twenty-four hours of hourly firings were missed, so it has to say so.
	if !strings.Contains(asked, "missed") {
		t.Errorf("a run hours late was not told it was late: %q", asked)
	}
}

// TestAScheduledRunCannotWriteOrRunAnything is the invariant the whole design
// rests on: nobody is watching, so nothing that would ask a person happens.
func TestAScheduledRunCannotWriteOrRunAnything(t *testing.T) {
	rt, store, one := scheduled(t, [][]provider.Event{
		{toolCall("call_1", "write_file", map[string]any{"path": "taken.md", "content": "no"})},
		{provider.TextDelta{Text: "I could not write."}, provider.Completed{StopReason: domain.StopEndTurn}},
	}, "* * * * *")
	ctx := context.Background()

	if _, err := rt.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	runs := scheduledRuns(t, store, one.SessionID)
	if len(runs) != 1 {
		t.Fatalf("want one run, got %d", len(runs))
	}
	waitForRun(t, rt, runs[0].ID)

	events, err := store.ListAfter(ctx, one.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var refused bool
	for _, event := range events {
		if done, ok := event.Payload.(domain.ToolCallCompleted); ok && done.Name == "write_file" {
			if !done.IsError {
				t.Fatal("a scheduled run wrote a file")
			}
			refused = true
		}
	}
	if !refused {
		t.Fatal("the write never reached the runtime, so this proves nothing")
	}

	// Refused, never parked. A run stopped on an approval nobody sees is not
	// waiting: it is stuck, at three in the morning, under a schedule that
	// will come due again before anybody looks.
	waiting, err := store.PendingApprovals(ctx, one.SessionID)
	if err != nil {
		t.Fatalf("pending approvals: %v", err)
	}
	if len(waiting) != 0 {
		t.Fatalf("a scheduled run asked somebody for permission: %+v", waiting)
	}
	if got := runs[0].Status; got == domain.RunAwaitingApproval {
		t.Error("a scheduled run parked waiting for a person")
	}
}
