package openaicompat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// classify turns a failed request into something the runtime can act on.
//
// The profile is asked first. Status codes are where compatibility breaks
// worst — the same 403 means "you may not" on one server and "your prompt is
// too long" on another — so a server that is known to differ gets to say so
// before the general reading applies.
func classify(profile Profile, status int, header http.Header, body []byte, model string) error {
	envelope := readError(body)

	kind := provider.KindUnknown
	if profile.ClassifyStatus != nil {
		kind = profile.ClassifyStatus(status, envelope)
	}
	if kind == provider.KindUnknown {
		kind = kindForStatus(status, envelope)
	}

	failure := &apiError{
		kind:       kind,
		provider:   profile.Name,
		model:      model,
		statusCode: status,
		message:    messageOf(envelope, body),
	}
	if after, ok := retryAfter(header, time.Now()); ok {
		failure.retryAfter = &after
	}
	return failure
}

func kindForStatus(status int, body *wireError) provider.ErrorKind {
	if body != nil {
		// A machine-readable code beats the status it arrived with.
		switch strings.ToLower(codeOf(body)) {
		case "context_length_exceeded":
			return provider.KindContextOverflow
		case "insufficient_quota", "credit_balance_exhausted",
			"organization_spend_limit_exceeded", "project_spend_limit_exceeded",
			"organization_usage_limit_exceeded", "billing_hard_limit_reached":
			// Over the same status as an ordinary rate limit, and the
			// opposite situation: no amount of waiting reaches the far side.
			return provider.KindQuotaExhausted
		case "model_not_found":
			return provider.KindNotFound
		case "content_filter", "content_policy_violation":
			return provider.KindContentFiltered
		}

		lowered := strings.ToLower(body.Message)
		if strings.Contains(lowered, "context length") || strings.Contains(lowered, "context window") ||
			strings.Contains(lowered, "too many tokens") {
			return provider.KindContextOverflow
		}
		if strings.Contains(lowered, "out of memory") || strings.Contains(lowered, "vram") ||
			strings.Contains(lowered, "cannot allocate") {
			// Memory only. A busy slot is a queue that drains, which is a
			// different thing to tell somebody and a different thing to do
			// about it.
			return provider.KindResourceExhausted
		}
	}

	switch status {
	case http.StatusTooManyRequests:
		return provider.KindRateLimited
	case http.StatusPaymentRequired:
		return provider.KindQuotaExhausted
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.KindAuth
	case http.StatusNotFound:
		return provider.KindNotFound
	case http.StatusRequestEntityTooLarge:
		return provider.KindContextOverflow
	case http.StatusRequestTimeout, http.StatusConflict:
		return provider.KindTransient
	case http.StatusServiceUnavailable, 529:
		// 529 is not a standard status. One provider uses it for being
		// overloaded, and nobody else uses it at all.
		return provider.KindOverloaded
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
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

// retryAfter reads the header in both forms the specification allows.
//
// Seconds is the common one. An HTTP-date is legal and arrives from servers
// behind certain proxies, and reading it against the local clock rather than
// the response's own Date is where clock skew turns a twenty second wait into
// a negative one.
func retryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}

	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}

	at, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}

	base := now
	if served, err := http.ParseTime(header.Get("Date")); err == nil {
		// The server's own clock, so a difference between the two machines
		// does not become part of the wait.
		base = served
	}

	wait := at.Sub(base)
	if wait < 0 {
		return 0, true
	}
	return wait, true
}

func readError(body []byte) *wireError {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if envelope.Message != "" {
		return &wireError{Message: envelope.Message}
	}
	return nil
}

func codeOf(body *wireError) string {
	switch code := body.Code.(type) {
	case string:
		return code
	case float64:
		return strconv.FormatInt(int64(code), 10)
	}
	if body.Type != "" {
		return body.Type
	}
	return ""
}

// messageOf produces something worth logging, whatever shape the body was.
func messageOf(envelope *wireError, body []byte) string {
	if envelope != nil && envelope.Message != "" {
		return envelope.Message
	}

	// Not JSON, or not a shape this knows: a proxy's HTML, a plain string.
	// Reporting a decode failure instead of what the server said would hide
	// the outage behind a parser error.
	text := strings.TrimSpace(string(body))
	const maxLength = 300
	if len(text) > maxLength {
		text = text[:maxLength] + "…"
	}
	return text
}

type apiError struct {
	kind       provider.ErrorKind
	provider   string
	model      string
	statusCode int
	message    string
	retryAfter *time.Duration
}

func (e *apiError) Error() string {
	base := fmt.Sprintf("openai-compatible (%s", e.provider)
	if e.model != "" {
		base += ", " + e.model
	}
	base += "): " + string(e.kind)
	if e.message != "" {
		base += ": " + e.message
	}
	if e.statusCode != 0 {
		base += fmt.Sprintf(" [status %d]", e.statusCode)
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
		Provider:   "openai_compat/" + e.provider,
		Model:      e.model,
		StatusCode: e.statusCode,
		Message:    e.message,
		RetryAfter: e.retryAfter,
	}
	return true
}
