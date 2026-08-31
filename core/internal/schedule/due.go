package schedule

import (
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Owed is what a schedule still has to do, worked out from what it last did.
//
// Derived rather than remembered. A timer inside a process knows only that
// the next tick is in sixty seconds, which is exactly the wrong thing to know
// on a laptop that has been shut since midnight: the question on waking is
// not "when is the next one" but "what was owed while I was gone", and only
// the last resolved firing plus the expression can answer it.
//
// lastResolved is the time of the most recent firing already turned into a
// run, or the zero time for a schedule that has never fired. now is when the
// question is being asked.
//
// Returns at most one firing. Every policy here coalesces — what differs is
// whether a missed firing is worth doing late at all.
func Owed(
	schedule domain.Schedule, lastResolved, now time.Time,
) (domain.Firing, bool, error) {
	if schedule.Paused {
		return domain.Firing{}, false, nil
	}

	expression, err := Parse(schedule.Expression)
	if err != nil {
		return domain.Firing{}, false, err
	}

	zone, err := zoneOf(schedule)
	if err != nil {
		return domain.Firing{}, false, err
	}

	// A schedule that has never fired starts from when it was made, so
	// creating one at nine does not immediately owe every nine o'clock since
	// the epoch.
	since := lastResolved
	if since.IsZero() {
		since = schedule.CreatedAt
	}

	// Every firing between then and now. Counted rather than collected: what
	// comes back is one firing, and the count is what lets a late answer say
	// it is late.
	var (
		due    time.Time
		missed int
	)
	for at, ok := expression.Next(since, zone); ok && !at.After(now); at, ok = expression.Next(at, zone) {
		if !due.IsZero() {
			missed++
		}
		due = at

		// A schedule left unfired for years — a laptop in a drawer — should
		// not be walked minute by minute through all of it. The count stops
		// being interesting long before this.
		const mostCounted = 10000
		if missed > mostCounted {
			break
		}
	}

	if due.IsZero() {
		return domain.Firing{}, false, nil
	}

	// Nothing was missed if this is the firing that just came due.
	if schedule.Missed == domain.MissedSkip && missed > 0 {
		// Skip means the answer would be about a moment that has passed, so
		// there is nothing worth running — but the firings still have to be
		// marked resolved, or the next reconcile owes them again.
		return domain.Firing{
			ScheduleID: schedule.ID,
			Revision:   schedule.Revision,
			For:        due,
			Observed:   now,
			Missed:     missed,
		}, false, nil
	}

	return domain.Firing{
		ScheduleID: schedule.ID,
		Revision:   schedule.Revision,
		For:        due,
		Observed:   now,
		Missed:     missed,
	}, true, nil
}

// zoneOf is where a schedule's hours are.
//
// By name, so that nine o'clock is nine o'clock on both sides of a daylight
// saving change. An offset stored instead would be wrong twice a year, in
// opposite directions, and only for the people it matters to.
func zoneOf(schedule domain.Schedule) (*time.Location, error) {
	if schedule.Zone == "" {
		return time.Local, nil
	}

	zone, err := time.LoadLocation(schedule.Zone)
	if err != nil {
		return nil, fmt.Errorf("schedule: %s names the zone %q, which this machine does not have: %w",
			schedule.ID, schedule.Zone, err)
	}
	return zone, nil
}

// Validate says whether a schedule could ever run, before it is stored.
//
// Checked at the point somebody types it rather than at three in the morning.
// A zone this machine does not have, or an expression naming the thirtieth of
// February, is a thing to be told about now.
func Validate(schedule domain.Schedule) error {
	expression, err := Parse(schedule.Expression)
	if err != nil {
		return err
	}
	zone, err := zoneOf(schedule)
	if err != nil {
		return err
	}

	from := schedule.CreatedAt
	if from.IsZero() {
		from = time.Now()
	}
	if _, ok := expression.Next(from, zone); !ok {
		return fmt.Errorf("schedule: %q names no time that will ever come", schedule.Expression)
	}
	return nil
}

// NextAfter is when a schedule next comes due, for showing in a listing.
//
// Distinct from Owed, which answers what is outstanding. This answers what is
// coming, and a schedule with an unusable expression or an unknown zone
// simply has no answer rather than an error: a listing that failed because
// one row was wrong would hide the others.
func NextAfter(one domain.Schedule, from time.Time) (time.Time, bool) {
	expression, err := Parse(one.Expression)
	if err != nil {
		return time.Time{}, false
	}
	zone, err := zoneOf(one)
	if err != nil {
		return time.Time{}, false
	}
	return expression.Next(from, zone)
}
