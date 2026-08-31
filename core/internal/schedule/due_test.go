package schedule

import (
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

func hourly(t *testing.T) domain.Schedule {
	t.Helper()

	return domain.Schedule{
		ID:         "sch_1",
		Revision:   1,
		Expression: "0 * * * *",
		Zone:       "UTC",
		CreatedAt:  at(t, "2026-08-31 00:00"),
	}
}

func TestNothingIsOwedBeforeTheFirstFiring(t *testing.T) {
	_, owed, err := Owed(hourly(t), time.Time{}, at(t, "2026-08-31 00:30"))
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if owed {
		t.Error("a firing was owed before one had come due")
	}
}

// TestANewScheduleDoesNotOweTheWholeOfHistory is what CreatedAt is for.
//
// A schedule made this morning saying "every hour" has not missed every hour
// since the epoch, and a first reconcile that thought so would start by
// running one.
func TestANewScheduleDoesNotOweTheWholeOfHistory(t *testing.T) {
	made := hourly(t)
	made.CreatedAt = at(t, "2026-08-31 10:05")

	firing, owed, err := Owed(made, time.Time{}, at(t, "2026-08-31 11:30"))
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if !owed {
		t.Fatal("nothing was owed an hour and a half later")
	}
	if firing.Missed != 0 {
		t.Errorf("it thinks it missed %d firings from before it existed", firing.Missed)
	}
	if want := at(t, "2026-08-31 11:00").UTC(); !firing.For.Equal(want) {
		t.Errorf("owed %v, want %v", firing.For, want)
	}
}

// TestFourMissedHoursAreOneAnswer is the coalescing launchd and systemd both
// do, and the reason this is the default.
//
// A laptop that slept from half past midnight until five owes one answer on
// waking, not four agents arriving at once to do the night's work.
func TestFourMissedHoursAreOneAnswer(t *testing.T) {
	firing, owed, err := Owed(hourly(t),
		at(t, "2026-08-31 00:00"), // last resolved: midnight
		at(t, "2026-08-31 05:30")) // woke at half past five
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if !owed {
		t.Fatal("nothing was owed after sleeping through four firings")
	}

	// The latest one, not the oldest: the answer is about now.
	if want := at(t, "2026-08-31 05:00").UTC(); !firing.For.Equal(want) {
		t.Errorf("owed %v, want the most recent %v", firing.For, want)
	}
	// And it can say it is late.
	if firing.Missed != 4 {
		t.Errorf("missed %d, want 4", firing.Missed)
	}
	// When it was due and when anybody noticed are different facts.
	if firing.Observed.Equal(firing.For) {
		t.Error("the time it was due and the time it was seen are the same value")
	}
}

// TestSkipRunsNothingButStillResolves keeps a skipped firing from being owed
// again a minute later.
func TestSkipRunsNothingButStillResolves(t *testing.T) {
	skipping := hourly(t)
	skipping.Missed = domain.MissedSkip

	firing, owed, err := Owed(skipping, at(t, "2026-08-31 00:00"), at(t, "2026-08-31 05:00"))
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if owed {
		t.Error("a skipping schedule ran work from five hours ago")
	}
	// Still named, so the caller can mark it resolved. Without that the next
	// reconcile finds the same four firings and skips them again forever.
	if firing.For.IsZero() {
		t.Error("nothing was named to resolve, so this will be owed again")
	}
}

// TestAFiringThatIsOnTimeIsNotSkipped is the precondition the test above
// needs: skip has to mean late, not always.
func TestAFiringThatIsOnTimeIsNotSkipped(t *testing.T) {
	skipping := hourly(t)
	skipping.Missed = domain.MissedSkip

	_, owed, err := Owed(skipping, at(t, "2026-08-31 04:00"), at(t, "2026-08-31 05:00"))
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if !owed {
		t.Error("a firing that had just come due was treated as missed")
	}
}

func TestAPausedScheduleOwesNothing(t *testing.T) {
	paused := hourly(t)
	paused.Paused = true

	_, owed, err := Owed(paused, at(t, "2026-08-31 00:00"), at(t, "2026-08-31 09:00"))
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	if owed {
		t.Error("a paused schedule fired")
	}
}

// TestTheSameFiringIsNotOwedTwice is what makes reconciling idempotent.
//
// A daemon that restarts, or wakes, asks this again. Answering the same
// firing twice is a second run for one occasion.
func TestTheSameFiringIsNotOwedTwice(t *testing.T) {
	schedule := hourly(t)
	now := at(t, "2026-08-31 05:30")

	first, owed, err := Owed(schedule, at(t, "2026-08-31 04:00"), now)
	if err != nil || !owed {
		t.Fatalf("first: %v %v", owed, err)
	}

	// Asked again, with the first now resolved.
	_, owed, err = Owed(schedule, first.For, now)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if owed {
		t.Error("the same firing was owed again after being resolved")
	}
}

// TestAZoneThisMachineLacksIsSaidNotGuessed keeps a schedule from silently
// running at the wrong hour.
func TestAZoneThisMachineLacksIsSaidNotGuessed(t *testing.T) {
	nowhere := hourly(t)
	nowhere.Zone = "Mars/Olympus_Mons"

	if _, _, err := Owed(nowhere, time.Time{}, at(t, "2026-08-31 09:00")); err == nil {
		t.Fatal("a schedule in a zone that does not exist was accepted")
	}
}

// TestValidateRefusesWhatWillNeverRun catches it where somebody can read it.
func TestValidateRefusesWhatWillNeverRun(t *testing.T) {
	never := hourly(t)
	never.Expression = "0 9 30 2 *" // the thirtieth of February

	if err := Validate(never); err == nil {
		t.Error("a schedule that can never run was accepted")
	}

	if err := Validate(hourly(t)); err != nil {
		t.Errorf("an ordinary schedule was refused: %v", err)
	}
}
