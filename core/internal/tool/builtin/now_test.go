package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
)

// The time it gives back is the time it was asked.
func TestItAnswersWithTheTimeOnTheClock(t *testing.T) {
	said := ask(t, at("2026-09-01T18:42:07+08:00"), "Asia/Taipei")

	if !strings.Contains(said, "2026-09-01T18:42:07+08:00") {
		t.Errorf("the clock said:\n%s", said)
	}
}

// It says which zone that offset is, by name.
//
// The offset alone is unambiguous and unreadable; the abbreviation alone is
// readable and a lie. "CST" is both China Standard Time at +08:00 and Central
// Standard Time at -06:00, and a model told only "CST" will convert to
// whichever it guesses.
func TestItNamesTheZoneRatherThanAbbreviatingIt(t *testing.T) {
	said := ask(t, at("2026-09-01T18:42:07+08:00"), "Asia/Taipei")

	if !strings.Contains(said, "Asia/Taipei") {
		t.Errorf("the zone is not named:\n%s", said)
	}
	if strings.Contains(said, "CST") {
		t.Errorf("an ambiguous abbreviation is offered as the zone:\n%s", said)
	}
}

// With no name to be had, the offset stands on its own.
//
// Windows has no zoneinfo name to read, and a machine can be configured
// without one. Inventing a plausible name would be worse than saying less:
// the offset is true and a wrong name is not.
func TestWithNoZoneNameItSaysTheOffsetAndStops(t *testing.T) {
	said := ask(t, at("2026-09-01T18:42:07+08:00"), "")

	if !strings.Contains(said, "+08:00") {
		t.Errorf("the offset is missing:\n%s", said)
	}
	for _, invented := range []string{"Asia/", "America/", "Europe/"} {
		if strings.Contains(said, invented) {
			t.Errorf("a zone name was invented where there is none:\n%s", said)
		}
	}

	// And no empty line offering one. "zone:" with nothing after it reads as
	// a zone whose name is blank rather than as a machine that has none.
	if strings.Contains(said, "zone:") {
		t.Errorf("a zone is offered where there is no name for one:\n%s", said)
	}
}

// UTC is there too, because it is the one everything else converts through.
func TestItAlsoGivesTheTimeInUTC(t *testing.T) {
	said := ask(t, at("2026-09-01T18:42:07+08:00"), "Asia/Taipei")

	if !strings.Contains(said, "2026-09-01T10:42:07Z") {
		t.Errorf("the same instant in UTC is missing:\n%s", said)
	}
}

// And the day of the week, which no amount of arithmetic on a date is worth
// asking a model to do.
func TestItSaysWhichDayOfTheWeekItIs(t *testing.T) {
	said := ask(t, at("2026-09-01T18:42:07+08:00"), "Asia/Taipei")

	if !strings.Contains(said, "Tuesday") {
		t.Errorf("the weekday is missing:\n%s", said)
	}
}

// Reading a clock touches nothing, so nothing stops to approve it.
//
// An approval for this is a prompt somebody answers without reading, which
// is how they learn to answer the next one that way too.
func TestReadingTheClockNeedsNobodysPermission(t *testing.T) {
	spec := (&builtin.Now{}).Spec()

	if spec.Level != tool.LevelInternal {
		t.Errorf("reading a clock is gated at %v", spec.Level)
	}
	if !spec.Capabilities.Idempotent {
		t.Error("asking twice is marked as changing something")
	}
	if spec.Capabilities.ForeignContent {
		t.Error("a clock is marked as returning somebody else's words")
	}
}

// It takes no arguments, and says so in a way a model can check against.
func TestItTakesNoArguments(t *testing.T) {
	spec := (&builtin.Now{}).Spec()

	var schema struct {
		Type       string          `json:"type"`
		Properties map[string]any  `json:"properties"`
		Additional json.RawMessage `json:"additionalProperties"`
	}
	if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
		t.Fatalf("the schema will not parse: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("the schema is a %q", schema.Type)
	}
	if len(schema.Properties) != 0 {
		t.Errorf("a tool that takes nothing declares %d properties", len(schema.Properties))
	}
	if string(schema.Additional) != "false" {
		t.Error("the schema accepts properties it does not describe")
	}
}

// The summary is what a client draws in a list, so it carries the answer.
func TestTheSummaryCarriesTheAnswer(t *testing.T) {
	clock := &builtin.Now{
		Reading:  func() time.Time { return at("2026-09-01T18:42:07+08:00") },
		ZoneName: func() string { return "Asia/Taipei" },
	}

	result, err := clock.Execute(context.Background(), tool.Call{Name: "current_time"})
	if err != nil {
		t.Fatalf("reading the clock: %v", err)
	}
	if !strings.Contains(result.Summary, "2026-09-01") {
		t.Errorf("the line a client draws does not say the date: %q", result.Summary)
	}
	if strings.Contains(result.Summary, "\n") {
		t.Errorf("the summary is more than one line: %q", result.Summary)
	}
}

// helpers

func ask(t *testing.T, reading time.Time, zone string) string {
	t.Helper()

	clock := &builtin.Now{
		Reading:  func() time.Time { return reading },
		ZoneName: func() string { return zone },
	}
	result, err := clock.Execute(context.Background(), tool.Call{Name: "current_time"})
	if err != nil {
		t.Fatalf("reading the clock: %v", err)
	}
	if result.IsError {
		t.Fatalf("reading the clock failed: %s", result.Content)
	}
	return result.Content
}

func at(stamp string) time.Time {
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		panic(err)
	}
	return when
}

// The machine's own zone is read as a name, from wherever it keeps one.
//
// Checked as a function of a path rather than against this machine, so the
// check means the same thing on a machine whose zoneinfo lives somewhere else
// — which is the whole reason it looks for the directory instead of counting
// segments.
func TestAZoneNameIsTakenFromWhereverZoneinfoIs(t *testing.T) {
	for _, reading := range []struct {
		path   string
		wanted string
	}{
		{"/usr/share/zoneinfo/Asia/Taipei", "Asia/Taipei"},
		{"/var/db/timezone/zoneinfo/Asia/Taipei", "Asia/Taipei"},
		{"../usr/share/zoneinfo/Europe/London", "Europe/London"},
		{"/usr/share/zoneinfo/UTC", "UTC"},

		// Nothing to read a name out of, which must give nothing rather than
		// the last thing on the path.
		{"/etc/localtime", ""},
		{"", ""},
		{"/usr/share/zoneinfo", ""},
	} {
		if got := builtin.ZoneNameIn(reading.path); got != reading.wanted {
			t.Errorf("%q gave %q, wanted %q", reading.path, got, reading.wanted)
		}
	}
}
