package control

import (
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/domain/domaintest"
)

// An event the wire format cannot express reaches no client at all, and the
// gap is invisible until somebody notices a reply that never arrived. Every
// kind the log can hold has to translate.
func TestEveryEventKindConvertsToProto(t *testing.T) {
	samples := domaintest.Payloads()

	for _, kind := range domain.AllEventKinds() {
		payload, ok := samples[kind]
		if !ok {
			t.Errorf("no sample for %s; add one to domaintest.Payloads", kind)
			continue
		}

		t.Run(string(kind), func(t *testing.T) {
			converted, err := eventToProto(domain.Event{
				ID:         "evt_1",
				SessionID:  "ses_1",
				RunID:      "run_1",
				Seq:        7,
				OccurredAt: time.Unix(0, 0).UTC(),
				Kind:       kind,
				Payload:    payload,
			})
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if converted.GetPayload() == nil {
				t.Fatal("converted to an event with no payload, which no client could render")
			}
		})
	}
}

// One session whose title is not text must not take the listing with it: the
// board, and the CLI, ask for every session at once.
func TestASessionWithABrokenTitleStillMarshals(t *testing.T) {
	converted := sessionToProto(domain.Session{ID: "ses_1", Title: "0 9 * * * — 整理昨天\xef\xbc…"})

	if !utf8.ValidString(converted.GetTitle()) {
		t.Errorf("the title is still not text: %q", converted.GetTitle())
	}
	if _, err := proto.Marshal(converted); err != nil {
		t.Errorf("marshal: %v", err)
	}
}
