package control

import (
	"testing"
	"time"

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
