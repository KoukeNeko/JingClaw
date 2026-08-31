// Package schedule works out when a standing instruction is due.
//
// Written here rather than taken from a library, and the reason is the error
// message. A cron expression is five fields somebody typed, four of which
// look plausible when wrong, and the useful answer to a bad one names the
// field and says what was expected. A parser that returns "invalid
// expression" leaves somebody counting asterisks.
//
// It is also small. Five fields of numbers, ranges, steps and lists is a
// afternoon; the parts of cron that are genuinely hard — @reboot, weekday
// and monthday interacting, seconds, timezone transitions — are either not
// offered or handled by the standard library.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Expression is a parsed five-field cron: minute, hour, day of month, month,
// day of week.
type Expression struct {
	minute  set
	hour    set
	day     set
	month   set
	weekday set

	// dayRestricted and weekdayRestricted record whether each was given.
	//
	// Cron's oldest oddity: with both day-of-month and day-of-week named, a
	// day matching either one matches. It is not what the rest of the syntax
	// leads anybody to expect, and it is what every implementation does, so
	// diverging would be its own surprise.
	dayRestricted     bool
	weekdayRestricted bool
}

// set is which values of one field match.
type set [60]bool

// field describes one column, for parsing and for saying what was wrong.
type field struct {
	name     string
	min, max int

	// names are the words this field accepts instead of numbers.
	names map[string]int
}

var fields = []field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}},
	{name: "day of week", min: 0, max: 6, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}},
}

// shorthands are the spellings people expect to work.
var shorthands = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// Parse reads an expression, or says which field it could not.
func Parse(text string) (Expression, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Expression{}, fmt.Errorf("schedule: an expression is needed, such as %q (every day at nine)", "0 9 * * *")
	}
	if expanded, ok := shorthands[strings.ToLower(trimmed)]; ok {
		trimmed = expanded
	}

	columns := strings.Fields(trimmed)
	if len(columns) != len(fields) {
		return Expression{}, fmt.Errorf(
			"schedule: %q has %d fields; a cron expression has %d — minute, hour, day of month, month, day of week",
			text, len(columns), len(fields))
	}

	var parsed Expression
	into := []*set{&parsed.minute, &parsed.hour, &parsed.day, &parsed.month, &parsed.weekday}

	for index, column := range columns {
		values, err := parseField(fields[index], column)
		if err != nil {
			return Expression{}, err
		}
		*into[index] = values
	}

	parsed.dayRestricted = columns[2] != "*"
	parsed.weekdayRestricted = columns[4] != "*"

	return parsed, nil
}

// parseField reads one column: a list of ranges, each optionally stepped.
func parseField(spec field, column string) (set, error) {
	var values set

	for _, part := range strings.Split(column, ",") {
		low, high, step, err := parseRange(spec, part)
		if err != nil {
			return set{}, err
		}
		for value := low; value <= high; value += step {
			values[value] = true
		}
	}
	return values, nil
}

// parseRange reads one comma-separated piece: "*", "5", "1-3", or any of them
// with "/n" after.
func parseRange(spec field, part string) (low, high, step int, err error) {
	step = 1

	if slash := strings.Index(part, "/"); slash >= 0 {
		stepped := part[slash+1:]
		part = part[:slash]

		step, err = strconv.Atoi(stepped)
		if err != nil || step <= 0 {
			return 0, 0, 0, fmt.Errorf(
				"schedule: %q is not a step in the %s field; a step is a number above zero, as in */15",
				stepped, spec.name)
		}
	}

	switch {
	case part == "*":
		return spec.min, spec.max, step, nil

	case strings.Contains(part, "-"):
		ends := strings.SplitN(part, "-", 2)
		low, err = parseValue(spec, ends[0])
		if err != nil {
			return 0, 0, 0, err
		}
		high, err = parseValue(spec, ends[1])
		if err != nil {
			return 0, 0, 0, err
		}
		if low > high {
			return 0, 0, 0, fmt.Errorf(
				"schedule: %q runs backwards in the %s field", part, spec.name)
		}
		return low, high, step, nil

	default:
		low, err = parseValue(spec, part)
		if err != nil {
			return 0, 0, 0, err
		}
		// A single value with a step means from there to the end of the
		// field, which is what "5/10" means everywhere else.
		if step > 1 {
			return low, spec.max, step, nil
		}
		return low, low, step, nil
	}
}

// parseValue reads one number or name, and says what the field allows.
func parseValue(spec field, text string) (int, error) {
	text = strings.TrimSpace(text)

	if named, ok := spec.names[strings.ToLower(text)]; ok {
		return named, nil
	}

	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("schedule: %q is not a value for the %s field, which takes %d to %d",
			text, spec.name, spec.min, spec.max)
	}

	// Sunday is both 0 and 7 in every cron there has ever been.
	if spec.name == "day of week" && value == 7 {
		value = 0
	}

	if value < spec.min || value > spec.max {
		return 0, fmt.Errorf("schedule: %d is outside the %s field, which takes %d to %d",
			value, spec.name, spec.min, spec.max)
	}
	return value, nil
}

// Matches says whether a moment is one this expression names.
func (e Expression) Matches(at time.Time) bool {
	if !e.minute[at.Minute()] || !e.hour[at.Hour()] || !e.month[int(at.Month())] {
		return false
	}

	day, weekday := e.day[at.Day()], e.weekday[int(at.Weekday())]

	// Both given means either matching is enough. See dayRestricted.
	if e.dayRestricted && e.weekdayRestricted {
		return day || weekday
	}
	return day && weekday
}

// Next is the first moment strictly after from that this names, in zone.
//
// Bounded rather than open-ended: an expression naming the 30th of February
// matches nothing, and a search for it would otherwise not stop. Four years
// covers every real expression, leap days included.
func (e Expression) Next(from time.Time, zone *time.Location) (time.Time, bool) {
	// Minute resolution, so the search starts at the next whole minute after
	// the one given. Starting inside the current minute would return a time
	// that has already passed.
	at := from.In(zone).Truncate(time.Minute).Add(time.Minute)

	const mostMinutes = 4 * 366 * 24 * 60
	for range mostMinutes {
		if e.Matches(at) {
			return at, true
		}
		at = at.Add(time.Minute)
	}
	return time.Time{}, false
}
