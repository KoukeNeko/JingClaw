package provider

import (
	"errors"
	"fmt"
	"time"
)

// ErrorKind classifies provider failures by what the runtime should do about
// them. Adapters translate their SDK's errors into these; nothing above this
// package should be inspecting HTTP status codes.
type ErrorKind string

const (
	// KindRateLimited: the caller is over quota. Honour RetryAfter when the
	// server supplied one rather than guessing.
	KindRateLimited ErrorKind = "rate_limited"

	// KindOverloaded: the provider is temporarily out of capacity. Retryable,
	// but not the caller's fault and not fixed by sending less.
	KindOverloaded ErrorKind = "overloaded"

	// KindTransient: network blips and 5xx.
	KindTransient ErrorKind = "transient"

	// KindInvalidRequest: the request is wrong and will stay wrong. Retrying
	// only wastes quota.
	KindInvalidRequest ErrorKind = "invalid_request"

	// KindAuth: missing, wrong or unauthorized credentials.
	KindAuth ErrorKind = "auth"

	// KindNotFound: unknown model or endpoint.
	KindNotFound ErrorKind = "not_found"

	// KindContextOverflow: the request exceeded the model's window. The fix is
	// compaction, not repetition.
	KindContextOverflow ErrorKind = "context_overflow"

	// KindContentFiltered: the provider refused on safety grounds. Deterministic
	// for the same input, so not retryable.
	KindContentFiltered ErrorKind = "content_filtered"

	KindUnknown ErrorKind = "unknown"
)

// Retryable reports whether resending an identical request could plausibly
// succeed.
func (k ErrorKind) Retryable() bool {
	switch k {
	case KindRateLimited, KindOverloaded, KindTransient:
		return true
	default:
		return false
	}
}

// Error is a provider failure with enough structure for the runtime to decide
// what to do without knowing which vendor produced it.
type Error struct {
	Kind ErrorKind

	// Provider and Model give a log line enough context to be actionable.
	Provider string
	Model    string

	StatusCode int

	// RetryAfter is set only when the server said so. An absent value means
	// "back off on your own schedule", not "retry immediately".
	RetryAfter *time.Duration

	RequestID string
	Message   string

	cause error
}

func (e *Error) Error() string {
	base := fmt.Sprintf("provider %s: %s", e.Provider, e.Kind)
	if e.Model != "" {
		base = fmt.Sprintf("provider %s (%s): %s", e.Provider, e.Model, e.Kind)
	}
	if e.Message != "" {
		base += ": " + e.Message
	}
	if e.StatusCode != 0 {
		base += fmt.Sprintf(" [status %d]", e.StatusCode)
	}
	return base
}

func (e *Error) Unwrap() error { return e.cause }

// Retryable reports whether this specific failure is worth resending.
func (e *Error) Retryable() bool { return e.Kind.Retryable() }

// KindOf extracts the classification from an error, defaulting to unknown.
func KindOf(err error) ErrorKind {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Kind
	}
	return KindUnknown
}

// IsRetryable reports whether err is a provider error worth resending.
func IsRetryable(err error) bool {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Retryable()
	}
	return false
}

// RetryAfter returns the server-specified delay, if there was one.
func RetryAfter(err error) (time.Duration, bool) {
	var perr *Error
	if errors.As(err, &perr) && perr.RetryAfter != nil {
		return *perr.RetryAfter, true
	}
	return 0, false
}

// NewError builds a classified provider error.
func NewError(kind ErrorKind, providerName, model, message string, cause error) *Error {
	return &Error{
		Kind:     kind,
		Provider: providerName,
		Model:    model,
		Message:  message,
		cause:    cause,
	}
}
