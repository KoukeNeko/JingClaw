package gateway

import (
	"testing"
	"time"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Every kind the daemon can put in the outbox has to come out of the wire as
// itself. Two did not, for weeks: log and question both decoded as an empty
// kind, could not be rendered, and were offered again on every reconnect.
func TestEveryDispatchKindSurvivesTheWire(t *testing.T) {
	seen := map[gateway.DispatchKind]bool{}
	for value, name := range controlv1.DispatchKind_name {
		kind := dispatchKindFromProto(controlv1.DispatchKind(value))
		if controlv1.DispatchKind(value) == controlv1.DispatchKind_DISPATCH_KIND_UNSPECIFIED {
			if kind != "" {
				t.Errorf("UNSPECIFIED decoded as %q rather than nothing", kind)
			}
			continue
		}
		if kind == "" {
			t.Errorf("the wire value %s decodes to no kind at all", name)
		}
		seen[kind] = true
	}
	for _, kind := range gateway.AllDispatchKinds() {
		if !seen[kind] {
			t.Errorf("%q has no wire value; a dispatch of that kind would arrive empty and never render", kind)
		}
	}
}

// A log line from long ago is not context for anything, and an answer is owed
// however late it is.
func TestOnlyOldLogLinesAreStale(t *testing.T) {
	now := time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)

	if !staleLog(gateway.Dispatch{Kind: gateway.DispatchLog, CreatedAt: old}, now) {
		t.Error("an hour-old log line was posted")
	}
	if staleLog(gateway.Dispatch{Kind: gateway.DispatchLog, CreatedAt: fresh}, now) {
		t.Error("a fresh log line was dropped")
	}
	for _, kind := range gateway.AllDispatchKinds() {
		if kind == gateway.DispatchLog {
			continue
		}
		if staleLog(gateway.Dispatch{Kind: kind, CreatedAt: old}, now) {
			t.Errorf("an old %s was dropped, and those are owed however late", kind)
		}
	}
	if staleLog(gateway.Dispatch{Kind: gateway.DispatchLog}, now) {
		t.Error("a log line with no timestamp was treated as old")
	}
}
