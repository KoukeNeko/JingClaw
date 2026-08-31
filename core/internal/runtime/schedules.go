package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/schedule"
)

// NewSchedule stores a standing instruction, after checking it could run.
//
// Checked here rather than at three in the morning. An expression naming the
// thirtieth of February, or a zone this machine does not have, is a thing to
// be told about while somebody is still looking at what they typed.
func (r *Runtime) NewSchedule(
	ctx context.Context, wanted domain.Schedule,
) (domain.Schedule, error) {
	if strings.TrimSpace(wanted.Prompt) == "" {
		return domain.Schedule{}, fmt.Errorf("runtime: a schedule needs something to ask")
	}

	wanted.ID = domain.ScheduleID(r.opts.NewScheduleID())
	wanted.Revision = 1
	wanted.CreatedAt = r.opts.Now()

	if err := schedule.Validate(wanted); err != nil {
		return domain.Schedule{}, err
	}

	// A session of its own unless one was named. One per schedule rather than
	// one per firing, so a daily question can be answered with reference to
	// yesterday's; what stops it growing forever is the compaction every
	// other session gets.
	if wanted.SessionID == "" {
		session, err := r.CreateSession(ctx, scheduleTitle(wanted))
		if err != nil {
			return domain.Schedule{}, err
		}
		wanted.SessionID = session.ID
	} else if _, err := r.opts.Store.Session(ctx, wanted.SessionID); err != nil {
		return domain.Schedule{}, err
	}

	if err := r.opts.Store.CreateSchedule(ctx, wanted); err != nil {
		return domain.Schedule{}, err
	}
	return wanted, nil
}

// scheduleTitle is what the session is called in a listing.
func scheduleTitle(one domain.Schedule) string {
	const most = 40

	asked := strings.TrimSpace(one.Prompt)
	if len(asked) > most {
		asked = asked[:most] + "…"
	}
	return one.Expression + " — " + asked
}

// Schedules is every standing instruction, with when each next comes due.
func (r *Runtime) Schedules(ctx context.Context) ([]domain.Schedule, []time.Time, error) {
	schedules, err := r.opts.Store.ListSchedules(ctx)
	if err != nil {
		return nil, nil, err
	}

	next := make([]time.Time, len(schedules))
	for index, one := range schedules {
		if one.Paused {
			// A paused schedule has no next time. Reporting one would be
			// saying it will run then, which it will not.
			continue
		}
		if when, ok := schedule.NextAfter(one, r.opts.Now()); ok {
			next[index] = when
		}
	}
	return schedules, next, nil
}

// PauseSchedule stops or resumes one.
func (r *Runtime) PauseSchedule(
	ctx context.Context, id domain.ScheduleID, paused bool,
) (domain.Schedule, error) {
	if err := r.opts.Store.SetSchedulePaused(ctx, id, paused); err != nil {
		return domain.Schedule{}, err
	}
	return r.opts.Store.Schedule(ctx, id)
}

// ForgetSchedule removes one, and the account of what it has already done.
//
// The session it made is left behind. What it holds is a record of runs that
// really happened, and deleting a schedule is a statement about the future.
func (r *Runtime) ForgetSchedule(ctx context.Context, id domain.ScheduleID) error {
	return r.opts.Store.DeleteSchedule(ctx, id)
}
