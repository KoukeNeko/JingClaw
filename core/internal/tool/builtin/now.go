package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Now tells the model what time it is.
//
// A tool rather than a line in the prompt, and the reason is caching. The
// prompt is a stable prefix that providers are paid to remember and replay;
// a clock written into it is correct once and then replayed as fact for as
// long as the prefix survives. Putting the time somewhere that is cached is
// not a small inaccuracy, it is a stale answer with nothing to mark it stale.
//
// It exists because the alternative is what a model does without it: answer
// from its training cutoff, confidently, with no sign that it guessed. A
// wrong date is not obviously wrong the way a wrong file path is — nothing
// downstream fails, and it comes back as a scheduled run for a day that has
// already been and gone.
type Now struct {
	// Reading is the clock, so a check can hold it still. Nil is the real one.
	Reading func() time.Time

	// ZoneName is where that reading is from, as a name a reader can convert
	// with. Nil asks the machine.
	ZoneName func() string
}

func (t *Now) Spec() tool.Spec {
	return tool.Spec{
		Name: "current_time",
		Description: "What the date and time are right now, on the machine you are running on. " +
			"Call it before saying or assuming anything about the current date, the day of the " +
			"week, how long ago something happened, or what 'today' and 'tomorrow' mean — you " +
			"cannot work any of those out from what you were trained on, and being wrong about " +
			"them is not obviously wrong to anybody reading the answer. " +
			"It takes no arguments and changes nothing.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`),
		// Reading a clock touches nothing. An approval here is a prompt
		// somebody answers without reading, which is how they learn to answer
		// the next one that way too.
		Level:        tool.LevelInternal,
		Capabilities: tool.Capabilities{Idempotent: true},
	}
}

func (t *Now) Execute(context.Context, tool.Call) (tool.Result, error) {
	reading := time.Now
	if t.Reading != nil {
		reading = t.Reading
	}
	naming := zoneName
	if t.ZoneName != nil {
		naming = t.ZoneName
	}

	when := reading()
	said := &strings.Builder{}

	fmt.Fprintf(said, "%s\n", when.Format(time.RFC3339))
	fmt.Fprintf(said, "%s\n", when.Format("Monday, 2 January 2006"))

	// The name, when there is one to have. Not the abbreviation: "CST" is
	// China Standard Time at +08:00 and Central Standard Time at -06:00, so a
	// reader given only that converts to whichever they guess. The offset on
	// the line above is what makes the instant unambiguous either way.
	if zone := naming(); zone != "" {
		fmt.Fprintf(said, "zone: %s\n", zone)
	}
	fmt.Fprintf(said, "utc:  %s\n", when.UTC().Format(time.RFC3339))

	return tool.Result{
		Content: said.String(),
		Summary: when.Format("2006-01-02 15:04:05 -07:00"),
	}, nil
}

// zoneName is the machine's own zone, as a name rather than an abbreviation.
//
// Read from TZ, or from what /etc/localtime points at, because Go's own
// time.Local reports itself as "Local" and its abbreviation is not unique.
// Empty when there is nothing truthful to say — Windows has no zoneinfo name
// to read, and a machine can be configured without one. Inventing a plausible
// name would be worse than saying less: the offset is true, and a wrong name
// is not.
func zoneName() string {
	if named := strings.TrimSpace(os.Getenv("TZ")); named != "" {
		return named
	}
	if runtime.GOOS == "windows" {
		return ""
	}

	pointed, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	return ZoneNameIn(pointed)
}

// ZoneNameIn is the part of a zoneinfo path that is the zone's name.
//
// Exported so it can be checked against the paths other machines use rather
// than only against this one's.
//
// Taken by looking for the directory rather than by counting path segments,
// because where that directory sits differs by system: /usr/share/zoneinfo on
// most, /var/db/timezone/zoneinfo on macOS.
func ZoneNameIn(path string) string {
	const marker = "zoneinfo"

	cleaned := filepath.ToSlash(filepath.Clean(path))
	segments := strings.Split(cleaned, "/")
	for index, segment := range segments {
		if segment == marker && index+1 < len(segments) {
			return strings.Join(segments[index+1:], "/")
		}
	}
	return ""
}
