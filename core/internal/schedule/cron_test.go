package schedule

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, text string) Expression {
	t.Helper()

	parsed, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return parsed
}

func at(t *testing.T, stamp string) time.Time {
	t.Helper()

	when, err := time.Parse("2006-01-02 15:04", stamp)
	if err != nil {
		t.Fatalf("bad time in the test itself: %v", err)
	}
	return when
}

func TestWhatAnExpressionNames(t *testing.T) {
	for _, one := range []struct {
		expression string
		moment     string
		matches    bool
	}{
		{"0 9 * * *", "2026-08-31 09:00", true},
		{"0 9 * * *", "2026-08-31 09:01", false},
		{"0 9 * * *", "2026-08-31 21:00", false},
		{"*/15 * * * *", "2026-08-31 10:30", true},
		{"*/15 * * * *", "2026-08-31 10:31", false},
		{"30 8-10 * * *", "2026-08-31 09:30", true},
		{"30 8-10 * * *", "2026-08-31 11:30", false},
		{"0 0 1 * *", "2026-09-01 00:00", true},
		{"0 0 1 * *", "2026-09-02 00:00", false},

		// Names, because "0 9 * * mon" is what somebody writes.
		{"0 9 * * mon", "2026-08-31 09:00", true}, // a Monday
		{"0 9 * * MON", "2026-08-31 09:00", true},
		{"0 9 * jan *", "2026-08-31 09:00", false},

		// Sunday is 0 and 7 in every cron there has ever been.
		{"0 9 * * 0", "2026-08-30 09:00", true},
		{"0 9 * * 7", "2026-08-30 09:00", true},

		// A single value with a step runs to the end of the field.
		{"5/10 * * * *", "2026-08-31 10:25", true},
		{"5/10 * * * *", "2026-08-31 10:20", false},
	} {
		got := mustParse(t, one.expression).Matches(at(t, one.moment))
		if got != one.matches {
			t.Errorf("%q at %s: got %v", one.expression, one.moment, got)
		}
	}
}

// TestBothDayFieldsMeanEither is cron's oldest oddity, and the one anybody
// reading the rest of the syntax would guess wrong.
func TestBothDayFieldsMeanEither(t *testing.T) {
	// The first of the month, or any Monday.
	both := mustParse(t, "0 9 1 * mon")

	for _, one := range []struct {
		moment  string
		matches bool
		why     string
	}{
		{"2026-09-01 09:00", true, "the first, which is a Tuesday"},
		{"2026-08-31 09:00", true, "a Monday, which is not the first"},
		{"2026-09-02 09:00", false, "neither"},
	} {
		if got := both.Matches(at(t, one.moment)); got != one.matches {
			t.Errorf("%s (%s): got %v", one.moment, one.why, got)
		}
	}

	// And with only one of them given, it is the ordinary conjunction.
	onlyWeekday := mustParse(t, "0 9 * * mon")
	if onlyWeekday.Matches(at(t, "2026-09-01 09:00")) {
		t.Error("a Tuesday matched an expression naming only Monday")
	}
}

// TestWhatIsWrongIsSaidByField is why this is written out rather than taken
// from a library.
//
// A cron expression is five columns, four of which look plausible when wrong.
// "invalid expression" leaves somebody counting asterisks.
func TestWhatIsWrongIsSaidByField(t *testing.T) {
	for _, one := range []struct {
		expression string
		says       string
	}{
		{"0 25 * * *", "hour"},
		{"0 9 32 * *", "day of month"},
		{"0 9 * 13 *", "month"},
		{"0 9 * * 9", "day of week"},
		{"99 9 * * *", "minute"},
		{"0 9 * * mo", "day of week"},
		{"*/0 * * * *", "step"},
		{"10-5 * * * *", "backwards"},
		{"0 9 * *", "5"},
		{"", "expression is needed"},
	} {
		_, err := Parse(one.expression)
		if err == nil {
			t.Errorf("Parse(%q) was accepted", one.expression)
			continue
		}
		if !strings.Contains(err.Error(), one.says) {
			t.Errorf("Parse(%q) does not mention %q: %v", one.expression, one.says, err)
		}
	}
}

func TestTheShorthandsPeopleExpect(t *testing.T) {
	for _, one := range []struct{ shorthand, same string }{
		{"@hourly", "0 * * * *"},
		{"@daily", "0 0 * * *"},
		{"@midnight", "0 0 * * *"},
		{"@weekly", "0 0 * * 0"},
		{"@monthly", "0 0 1 * *"},
		{"@yearly", "0 0 1 1 *"},
	} {
		short := mustParse(t, one.shorthand)
		long := mustParse(t, one.same)

		// Compared by behaviour over a year rather than by struct equality,
		// which would pass on two expressions that were both wrong.
		when := at(t, "2026-01-01 00:00")
		for range 366 * 24 {
			if short.Matches(when) != long.Matches(when) {
				t.Fatalf("%s and %q disagree at %s", one.shorthand, one.same, when)
			}
			when = when.Add(time.Hour)
		}
	}
}

func TestNextIsTheFirstMomentAfter(t *testing.T) {
	daily := mustParse(t, "0 9 * * *")
	utc := time.UTC

	// From before it, the same day.
	got, ok := daily.Next(at(t, "2026-08-31 08:00"), utc)
	if !ok || !got.Equal(at(t, "2026-08-31 09:00").In(utc)) {
		t.Errorf("got %v (%v)", got, ok)
	}

	// From exactly it, the next one — strictly after, or a firing that has
	// just been resolved would be found again immediately.
	got, ok = daily.Next(at(t, "2026-08-31 09:00"), utc)
	if !ok || !got.Equal(at(t, "2026-09-01 09:00").In(utc)) {
		t.Errorf("from the moment itself: got %v (%v)", got, ok)
	}
}

// TestAnExpressionThatNamesNothingSaysSo keeps the search bounded.
//
// The thirtieth of February is a legal expression that matches no time there
// will ever be, and looking for the next one is a loop that does not end.
func TestAnExpressionThatNamesNothingSaysSo(t *testing.T) {
	never := mustParse(t, "0 9 30 2 *")

	if _, ok := never.Next(at(t, "2026-08-31 09:00"), time.UTC); ok {
		t.Error("a moment was found for the thirtieth of February")
	}
}

// TestNineMeansNineWhereSomebodyIs is why a zone is stored by name.
func TestNineMeansNineWhereSomebodyIs(t *testing.T) {
	taipei, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Skipf("no zone database here: %v", err)
	}

	daily := mustParse(t, "0 9 * * *")

	got, ok := daily.Next(at(t, "2026-08-31 00:30").UTC(), taipei)
	if !ok {
		t.Fatal("nothing found")
	}
	if hour := got.Hour(); hour != 9 {
		t.Errorf("nine in Taipei came out as %d", hour)
	}
	// And it is a real instant, not a wall clock: 09:00 in Taipei is 01:00 UTC.
	if utcHour := got.UTC().Hour(); utcHour != 1 {
		t.Errorf("09:00 in Taipei is 01:00 UTC, got %d", utcHour)
	}
}
