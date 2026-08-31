package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

// reconcileEvery is how often the daemon asks what is owed.
//
// A minute, because that is the finest a cron expression can name. It is not
// what makes a schedule fire on time — the answer is derived from the log
// either way, so a tick that is late merely asks the question late, and one
// that never happens at all is caught by the next start.
const reconcileEvery = time.Minute

// watchSchedules asks what is owed, now and then repeatedly.
//
// Now, because a machine that was asleep runs no timers: the first thing a
// waking daemon has to do is work out what came due while it was gone, and
// waiting a minute to ask would delay every schedule by a minute after every
// restart.
//
// It returns when the context ends, which is the daemon shutting down.
func watchSchedules(ctx context.Context, rt *runtime.Runtime, logger *slog.Logger) {
	reconcile := func() {
		started, err := rt.Reconcile(ctx)
		if err != nil {
			// Not fatal. A schedule that cannot be read is a schedule that
			// does not run; the daemon still serves everything else, and the
			// alternative is a stopped agent because of a stored row.
			logger.Warn("schedules could not be reconciled", "error", err)
			return
		}
		if started > 0 {
			logger.Info("schedules came due", "started", started)
		}
	}

	reconcile()

	// Aligned to the minute, because that is what the expressions name. A
	// ticker started at whatever second the daemon happened to boot would ask
	// at seventeen seconds past every minute, so a schedule due at nine
	// o'clock would run at 09:00:17 — or, with a boot a second later, at
	// 09:00:59. The alignment costs one sleep and removes a minute of
	// arbitrary lateness from every schedule.
	select {
	case <-ctx.Done():
		return
	case <-time.After(untilNextMinute(time.Now())):
	}
	reconcile()

	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

// untilNextMinute is how long until the clock reaches the next whole minute.
//
// A hair past it, so that a firing due at nine o'clock is already in the past
// when the question is asked. Landing exactly on the boundary would make
// whether it fires now or in a minute depend on which side of the second the
// clock rounds to.
func untilNextMinute(now time.Time) time.Duration {
	const justAfter = 100 * time.Millisecond
	return now.Truncate(time.Minute).Add(time.Minute + justAfter).Sub(now)
}
