package console

import (
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/domain/domaintest"
)

// silent is every kind the console deliberately does not print, and why.
//
// Named rather than left to fall through the default case. A console that
// quietly drops what it does not recognise is one where adding an event kind
// makes it invisible, and nothing says so — the log simply never mentions a
// thing that happened.
var silent = map[domain.EventKind]string{
	domain.EventAssistantTextDelta:      "a token at a time; the completed message says it once",
	domain.EventAssistantReasoningDelta: "the same, and it is not the answer",
	domain.EventUsageChanged:            "a counter, not something that happened",
	domain.EventRunDirections:           "carried on the run, and shown when the run changes state",
}

// Every kind is either printed or listed above. This is the test that fails
// when somebody adds an event and the console says nothing about it.
func TestEveryEventKindIsAccountedFor(t *testing.T) {
	for kind, payload := range domaintest.Payloads() {
		_, shown := Describe(domain.Event{
			SessionID: "s_1",
			Kind:      kind,
			Payload:   payload,
		})

		reason, deliberate := silent[kind]
		switch {
		case shown && deliberate:
			t.Errorf("%s is printed and also listed as silent (%q); one of the two is wrong",
				kind, reason)
		case !shown && !deliberate:
			t.Errorf("%s produces no line and is not listed as deliberately silent. "+
				"Either give it one in Describe, or add it to silent with the reason.", kind)
		}
	}
}

// A line with no kind is a line that says nothing, which is worse than no line.
func TestEveryPrintedLineSaysWhatHappened(t *testing.T) {
	for kind, payload := range domaintest.Payloads() {
		line, shown := Describe(domain.Event{
			SessionID: "s_1",
			Kind:      kind,
			Payload:   payload,
		})
		if !shown {
			continue
		}
		if line.Kind == "" {
			t.Errorf("%s produced a line with no kind: %q", kind, line.String())
		}
	}
}
