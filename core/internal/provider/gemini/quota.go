package gemini

import (
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Google returns the machine-readable part of an error in details, alongside
// the English sentence. Only the sentence used to be kept, which is how a run
// came to be abandoned for twenty seconds it was told to wait, and how the
// whole of Google's prose ended up in a chat channel.
//
// https://ai.google.dev/gemini-api/docs/troubleshooting
const (
	retryInfoType    = "type.googleapis.com/google.rpc.RetryInfo"
	quotaFailureType = "type.googleapis.com/google.rpc.QuotaFailure"
)

// quotaDetail is what the structured part of a 429 says.
type quotaDetail struct {
	// RetryDelay is how long the server says to wait. Its presence is itself
	// informative: Google supplies it when waiting is going to help.
	RetryDelay *time.Duration

	// QuotaID names the window that was exhausted, e.g.
	// GenerateContentInputTokensPerModelPerMinute-FreeTier. It is the only
	// reliable way to tell a limit that clears in a minute from an allowance
	// that lasts until tomorrow.
	QuotaID string

	QuotaMetric string
}

// readQuotaDetail extracts what the error details carry.
func readQuotaDetail(apiErr genai.APIError) quotaDetail {
	var detail quotaDetail

	for _, item := range apiErr.Details {
		switch item["@type"] {
		case retryInfoType:
			// A protobuf Duration in its JSON form — "20.090700289s" — which
			// is not the integer seconds an HTTP Retry-After header carries,
			// and happens to be exactly what Go parses natively.
			if raw, ok := item["retryDelay"].(string); ok {
				if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
					detail.RetryDelay = &parsed
				}
			}
		case quotaFailureType:
			violations, ok := item["violations"].([]any)
			if !ok {
				continue
			}
			for _, entry := range violations {
				violation, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := violation["quotaId"].(string); ok && detail.QuotaID == "" {
					detail.QuotaID = id
				}
				if metric, ok := violation["quotaMetric"].(string); ok && detail.QuotaMetric == "" {
					detail.QuotaMetric = metric
				}
			}
		}
	}

	return detail
}

// classifyExhaustion decides whether a 429 is worth waiting out.
//
// Both a per-minute limit and a spent daily allowance arrive as 429, and they
// call for opposite responses: one clears while somebody watches, and the
// other does not clear today however long anything waits. Getting it wrong in
// either direction is expensive — giving up twenty seconds early, or retrying
// against a wall until the allowance that remains is gone too.
//
// The decision is made on the quota identifier rather than on the English
// message. Prose is written for people and changes without notice; a run that
// reads it is a run whose behaviour depends on a sentence nobody versioned.
func classifyExhaustion(detail quotaDetail) provider.ErrorKind {
	window := detail.QuotaID
	if window == "" {
		window = detail.QuotaMetric
	}
	lowered := strings.ToLower(window)

	switch {
	case strings.Contains(lowered, "perday"), strings.Contains(lowered, "per_day"),
		strings.Contains(lowered, "daily"),
		strings.Contains(lowered, "permonth"), strings.Contains(lowered, "per_month"):
		return provider.KindQuotaExhausted

	case strings.Contains(lowered, "perminute"), strings.Contains(lowered, "per_minute"),
		strings.Contains(lowered, "persecond"), strings.Contains(lowered, "per_second"):
		return provider.KindRateLimited
	}

	// Nothing named the window. A delay the server volunteered is the next
	// best evidence that waiting is the answer, since Google supplies one when
	// it expects the request to succeed after it.
	if detail.RetryDelay != nil {
		return provider.KindRateLimited
	}

	// Unrecognised and with no delay offered. Treated as the kind that stops,
	// because retrying into an unknown 429 is how an allowance gets spent
	// discovering that it was already spent.
	return provider.KindQuotaExhausted
}
