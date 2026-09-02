package storage

import (
	"encoding/json"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// The log is append-only, so the events already in it are the specification.
// This is a payload written before "who decided this" carried an origin,
// copied from a real deployment's database.
const asItWasWritten = `{"approval_id":"apr_01M174AGK7EPC6JDM0S5APHSEM",` +
	`"call_id":"call_yeiyczst","tool_name":"exec_command","status":"allowed",` +
	`"scope":"once","decided_by":"discord:900000000000000042"}`

func TestAnApprovalWrittenBeforeTheSplitStillReads(t *testing.T) {
	var payload approvalResolvedJSON
	if err := json.Unmarshal([]byte(asItWasWritten), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	decided := decodeOrigin(payload.DecidedBy.runOriginJSON)
	if decided.Principal == nil {
		t.Fatalf("the person who decided it was lost: %+v", decided)
	}
	if decided.Principal.Platform != "discord" {
		t.Errorf("platform is %q, want discord", decided.Principal.Platform)
	}
	if decided.Principal.PrincipalID != "900000000000000042" {
		t.Errorf("principal is %q, want 900000000000000042", decided.Principal.PrincipalID)
	}
	if decided.Kind != domain.OriginGateway {
		t.Errorf("kind is %q, want %q", decided.Kind, domain.OriginGateway)
	}
}

// The old field also held values that named no person at all. Those must not
// become one: promoting a client name into a principal is exactly the mistake
// the split exists to undo.
func TestAnOldValueThatNamedNobodyDoesNotBecomeSomebody(t *testing.T) {
	for _, old := range []string{"jingclaw-cli", "discord", "unknown", ""} {
		var payload approvalResolvedJSON
		raw, err := json.Marshal(map[string]string{"decided_by": old})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %q: %v", old, err)
		}

		if decided := decodeOrigin(payload.DecidedBy.runOriginJSON); decided.Principal != nil {
			t.Errorf("%q became a person: %+v", old, decided.Principal)
		}
	}
}

// What is written now is read back as what was written.
func TestAnOriginSurvivesBeingWrittenAndRead(t *testing.T) {
	for name, origin := range map[string]domain.RunOrigin{
		"the machine":       domain.FromTheMachine("jingclaw-cli"),
		"a platform accont": domain.FromAPlatformAccount("discord", "77", "Alice"),
		"a channel":         domain.FromAChannel("discord", "900000000000000041"),
	} {
		written, err := json.Marshal(approvalResolvedJSON{
			DecidedBy: decidedByJSON{encodeOrigin(origin)},
		})
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}

		var back approvalResolvedJSON
		if err := json.Unmarshal(written, &back); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}

		got := decodeOrigin(back.DecidedBy.runOriginJSON)
		if got.Kind != origin.Kind || got.ClientID != origin.ClientID {
			t.Errorf("%s: got %+v, want %+v", name, got, origin)
		}
		switch {
		case origin.Principal == nil && got.Principal != nil:
			t.Errorf("%s: gained a principal: %+v", name, got.Principal)
		case origin.Principal != nil && got.Principal == nil:
			t.Errorf("%s: lost its principal", name)
		case origin.Principal != nil && *got.Principal != *origin.Principal:
			t.Errorf("%s: principal is %+v, want %+v", name, got.Principal, origin.Principal)
		}
	}
}
