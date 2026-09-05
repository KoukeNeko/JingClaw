package control

import (
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// The daemon's half of the same guard: every kind it can queue has to encode
// as something other than UNSPECIFIED, which the gateway reads as nothing.
func TestEveryDispatchKindEncodes(t *testing.T) {
	for _, kind := range gateway.AllDispatchKinds() {
		if dispatchKindToProto(kind) == controlv1.DispatchKind_DISPATCH_KIND_UNSPECIFIED {
			t.Errorf("%q encodes as UNSPECIFIED; the gateway would receive it as an empty kind", kind)
		}
	}
}
