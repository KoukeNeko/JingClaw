package ollama

import (
	"net/http"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// A request refused for its size is the conversation being too long.
//
// Ollama answers 413 with Go's own "http: request body too large", which is
// not the 400 the context check looks at, so it landed in the bucket for
// requests the server could not parse. What reached the room was "something
// went wrong at the model" — a shrug — when the true thing is both sayable
// and actionable: the conversation outgrew the model, and compaction is the
// thing that fixes it.
func TestARequestRefusedForItsSizeIsContextOverflow(t *testing.T) {
	for _, refusal := range []struct {
		status  int
		message string
	}{
		{http.StatusRequestEntityTooLarge, "http: request body too large"},
		{http.StatusRequestEntityTooLarge, "payload too large"},

		// The same thing under the status the check already handled, which
		// has to keep working.
		{http.StatusBadRequest, "input length exceeds context length"},
	} {
		if got := kindFor(refusal.status, refusal.message); got != provider.KindContextOverflow {
			t.Errorf("%d %q classified as %q, want context overflow",
				refusal.status, refusal.message, got)
		}
	}
}

// And a size refusal is not a licence to call everything overflow.
//
// A malformed request still says so, or the shortening that follows an
// overflow is done against something shortening cannot fix — and it is done
// again on the retry, and again.
func TestAMalformedRequestIsStillMalformed(t *testing.T) {
	if got := kindFor(http.StatusBadRequest, "invalid json"); got != provider.KindInvalidRequest {
		t.Errorf("a malformed request classified as %q", got)
	}
}
