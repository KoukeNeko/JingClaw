package ollama

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

const providerName = "ollama"

// classify turns a failed request into something the runtime can act on.
//
// A machine serving models locally fails in ways a hosted API does not, and
// the HTTP status alone does not separate them. Ollama answers 503 both while
// a model is still loading — which resolves by waiting — and when its request
// queue is full, which also resolves by waiting but means something different
// to whoever is watching. It answers 500 for a model that will not fit in
// memory, which no amount of waiting fixes.
//
// So the body decides, and the status is only the first hint.
func classify(status int, body []byte, model string) error {
	message := strings.TrimSpace(readErrorMessage(body))

	return &apiError{
		kind:       kindFor(status, message),
		model:      model,
		statusCode: status,
		message:    message,
	}
}

func kindFor(status int, message string) provider.ErrorKind {
	lowered := strings.ToLower(message)

	// Memory is checked before the status, because it arrives under several
	// and means the same thing under all of them: this machine cannot hold
	// this request, and sending it again unchanged will not change that.
	if mentionsMemory(lowered) {
		return provider.KindResourceExhausted
	}

	switch status {
	case http.StatusTooManyRequests:
		// A local daemon has no billing; anything here is a front door.
		return provider.KindRateLimited

	case http.StatusServiceUnavailable:
		// Queue full, or a model still loading. Both clear on their own.
		return provider.KindOverloaded

	case http.StatusUnauthorized, http.StatusForbidden:
		// Only Ollama Cloud can produce these; a local daemon has no
		// credentials to reject.
		return provider.KindAuth

	case http.StatusNotFound:
		// Reliably "no such model" rather than "no such endpoint", and worth
		// distinguishing: the fix is to pull it.
		return provider.KindNotFound

	case http.StatusBadRequest:
		if mentionsContext(lowered) {
			return provider.KindContextOverflow
		}
		return provider.KindInvalidRequest
	}

	if status >= 500 {
		return provider.KindTransient
	}
	if status >= 400 {
		return provider.KindInvalidRequest
	}
	return provider.KindUnknown
}

// mentionsMemory reports whether a failure is about this machine's capacity.
//
// Matched on single words rather than whole phrases. The wording varies —
// "out of memory", "requires more system memory than is available",
// "cannot allocate" — and a list of exact sentences is a list that is already
// out of date. Any mention of memory or VRAM in an inference server's error is
// about not having enough of it.
func mentionsMemory(lowered string) bool {
	for _, word := range []string{
		"memory", "vram", "oom", "cannot allocate", "no available slot",
	} {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}

func mentionsContext(lowered string) bool {
	return strings.Contains(lowered, "context") &&
		(strings.Contains(lowered, "exceed") || strings.Contains(lowered, "too long") ||
			strings.Contains(lowered, "too large"))
}

// readErrorMessage pulls the message out of a body that may or may not be JSON.
//
// A daemon behind a proxy answers with the proxy's HTML, and a runtime that
// insists on JSON reports a parse failure instead of the 502 that actually
// happened.
func readErrorMessage(body []byte) string {
	var decoded errorBody
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error != "" {
		return decoded.Error
	}

	text := strings.TrimSpace(string(body))
	const maxLength = 300
	if len(text) > maxLength {
		text = text[:maxLength] + "…"
	}
	return text
}

// apiError adapts to provider.Error without exporting a second error type.
type apiError struct {
	kind       provider.ErrorKind
	model      string
	statusCode int
	message    string
}

func (e *apiError) Error() string {
	base := providerName
	if e.model != "" {
		base += " (" + e.model + ")"
	}
	base += ": " + string(e.kind)
	if e.message != "" {
		base += ": " + e.message
	}
	return base
}

func (e *apiError) As(target any) bool {
	perr, ok := target.(**provider.Error)
	if !ok {
		return false
	}
	*perr = &provider.Error{
		Kind:       e.kind,
		Provider:   providerName,
		Model:      e.model,
		StatusCode: e.statusCode,
		Message:    e.message,
	}
	return true
}
