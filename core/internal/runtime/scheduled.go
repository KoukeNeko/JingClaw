package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/schedule"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// unattendedProfile is what a scheduled session runs under.
//
// Named here rather than taken from configuration. Which profile an unwatched
// run gets is not a preference — it is the answer to "what may happen while
// nobody is looking" — and a setting for it would be a setting for making
// that answer worse.
const unattendedProfile = "unattended"

// Reconcile works out what every schedule owes and starts it.
//
// Called on start and then periodically. Both, and for the same reason: the
// periodic call is not what makes this correct. A laptop shut at midnight
// runs no timers at all, so the question on waking is not "has a minute
// passed" but "what came due while I was gone" — and that is answered from
// the log, by comparing the last occasion accounted for against the
// expression. The ticker only decides how often the question is asked.
//
// Returns how many runs it started. Errors from one schedule do not stop the
// others: a schedule naming a timezone this machine lacks is a thing to
// report, not a reason for every other schedule to stop.
func (r *Runtime) Reconcile(ctx context.Context) (int, error) {
	schedules, err := r.opts.Store.ListSchedules(ctx)
	if err != nil {
		return 0, err
	}

	var started int
	for _, one := range schedules {
		ran, err := r.reconcileOne(ctx, one, r.opts.Now())
		if err != nil {
			r.opts.Logger.Warn("a schedule could not be reconciled",
				"schedule", string(one.ID), "error", err)
			continue
		}
		if ran {
			started++
		}
	}
	return started, nil
}

// reconcileOne settles a single schedule, and says whether it started a run.
func (r *Runtime) reconcileOne(
	ctx context.Context, one domain.Schedule, now time.Time,
) (bool, error) {
	lastResolved, err := r.opts.Store.LastFiring(ctx, one.ID, one.Revision)
	if err != nil {
		return false, err
	}

	firing, owed, err := schedule.Owed(one, lastResolved, now)
	if err != nil {
		return false, err
	}
	if firing.For.IsZero() {
		return false, nil
	}

	// Already running, so this one waits. Overlap is not a thing to allow by
	// default: a schedule whose answer takes longer than its interval would
	// otherwise accumulate agents until something gave way, and the failure
	// would arrive as a machine grinding rather than as a schedule too eager.
	if owed {
		busy, err := r.scheduleIsBusy(ctx, one)
		if err != nil {
			return false, err
		}
		if busy {
			r.opts.Logger.Info("a schedule came due while its last run was still going",
				"schedule", string(one.ID), "due", firing.For)
			return false, nil
		}
	}

	if !owed {
		// Nothing to run, but the occasion still has to be accounted for or
		// the next reconcile will find it again and skip it again forever.
		return false, r.markResolved(ctx, firing)
	}

	// Claimed before anything is started. Two daemons, or one restarting
	// twice in a minute, both reach this line for the same three o'clock; the
	// one that loses the insert stops here rather than starting a second run.
	if err := r.markResolved(ctx, firing); err != nil {
		return false, err
	}

	runID, err := r.startScheduled(ctx, one, firing)
	if err != nil {
		// The occasion stays resolved. Retrying it would mean asking again
		// at some arbitrary later minute, which is not what the schedule
		// said, and the failure is in the log for somebody to read.
		return false, err
	}

	firing.RunID = runID
	if err := r.opts.Store.RecordFiringRun(ctx, firing); err != nil {
		r.opts.Logger.Warn("a scheduled run could not be linked to its firing",
			"schedule", string(one.ID), "run_id", string(runID), "error", err)
	}
	return true, nil
}

// markResolved accounts for an occasion, treating a second attempt as done.
func (r *Runtime) markResolved(ctx context.Context, firing domain.Firing) error {
	err := r.opts.Store.ResolveFiring(ctx, firing)
	if errors.Is(err, storage.ErrFiringAlreadyResolved) {
		// Not a failure. It is the answer: somebody already accounted for
		// this occasion, so there is nothing left to do about it.
		return nil
	}
	return err
}

// scheduleIsBusy reports whether this schedule's last run is still going.
func (r *Runtime) scheduleIsBusy(ctx context.Context, one domain.Schedule) (bool, error) {
	runs, err := r.opts.Store.ListRuns(ctx, one.SessionID)
	if err != nil {
		return false, err
	}

	for _, run := range runs {
		if run.Origin.Kind != domain.OriginSchedule || run.Origin.ClientID != string(one.ID) {
			continue
		}
		if !run.Status.IsTerminal() {
			return true, nil
		}
	}
	return false, nil
}

// startScheduled turns an occasion into a run.
func (r *Runtime) startScheduled(
	ctx context.Context, one domain.Schedule, firing domain.Firing,
) (domain.RunID, error) {
	// The profile is set every time rather than once at creation, because the
	// engine keeps this in memory and a restarted daemon would otherwise run
	// the next firing under whatever the fallback profile is — which is the
	// operator's own.
	if r.opts.Permissions != nil {
		if err := r.opts.Permissions.UseProfile(one.SessionID, unattendedProfile); err != nil {
			return "", fmt.Errorf("runtime: a scheduled session could not be confined: %w", err)
		}
	}

	runID, _, err := r.SendTurnTo(ctx, one.SessionID, domain.Turn{
		Text: promptFor(one, firing),

		// The schedule is who acts, not whoever set it up. Creating a
		// schedule is delegation; running one is not that person still
		// acting, hours later, while they are asleep.
		Origin: domain.FromASchedule(one.ID),

		// Where it was told to deliver, which is not the same as where it was
		// created. Empty delivers nowhere, and the answer is still in the log.
		Targets: one.Deliver,
	})
	return runID, err
}

// promptFor is what the agent is asked, with the lateness said out loud.
//
// Said because it changes the answer. A digest arriving five hours after it
// was due is about a different window than the one the question implies, and
// a model that does not know it is late will write as though it is not.
func promptFor(one domain.Schedule, firing domain.Firing) string {
	if firing.Missed == 0 {
		return one.Prompt
	}
	return fmt.Sprintf(
		"%s\n\n(This was due at %s and is running now instead. %d earlier turns "+
			"of this schedule were missed while nothing was running; answer for "+
			"the whole period rather than only the last interval.)",
		one.Prompt, firing.For.Format(time.RFC3339), firing.Missed)
}
