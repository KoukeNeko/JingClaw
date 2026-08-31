package daemon

import (
	"testing"
	"time"
)

// TestTheQuestionIsAskedJustAfterTheMinute is why the first tick is aligned.
//
// A ticker started at whatever second the daemon booted asks at that second
// of every minute, so a schedule due at nine o'clock runs at 09:00:17 — or,
// with a boot one second later, at 09:00:59. The lateness is arbitrary and
// invisible, which is the worst combination.
func TestTheQuestionIsAskedJustAfterTheMinute(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 9, 0, 17, 0, time.UTC),
		time.Date(2026, 8, 31, 9, 0, 59, 900_000_000, time.UTC),
	} {
		wait := untilNextMinute(now)
		if wait <= 0 {
			t.Errorf("from %s the wait is %v", now.Format(time.TimeOnly), wait)
			continue
		}

		landed := now.Add(wait)

		// Just after the boundary, never before it: a firing due at nine has
		// to already be in the past when the question is asked, or whether it
		// runs now or in a minute depends on how the clock rounds.
		if !landed.After(landed.Truncate(time.Minute)) {
			t.Errorf("from %s it lands exactly on %s", now.Format(time.TimeOnly), landed)
		}
		if late := landed.Sub(landed.Truncate(time.Minute)); late > time.Second {
			t.Errorf("from %s it lands %v after the minute", now.Format(time.TimeOnly), late)
		}
		if landed.Sub(now) > time.Minute+time.Second {
			t.Errorf("from %s it waits %v, more than a minute", now.Format(time.TimeOnly), wait)
		}
	}
}
