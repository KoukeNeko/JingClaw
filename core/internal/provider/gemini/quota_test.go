package gemini

import (
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// The error that actually took a run down, in the shape Google sends it.
//
// The English sentence said "Please retry in 20.090700289s" and the run was
// abandoned instead, because only the sentence was kept and nothing read the
// figure beside it.
func TestTheProductionRateLimitIsRecognisedAndWaitedOut(t *testing.T) {
	apiErr := genai.APIError{
		Code:    429,
		Status:  "RESOURCE_EXHAUSTED",
		Message: "You exceeded your current quota, please check your plan and billing details.",
		Details: []map[string]any{
			{
				"@type": quotaFailureType,
				"violations": []any{map[string]any{
					"quotaId":     "GenerateContentInputTokensPerModelPerMinute-FreeTier",
					"quotaMetric": "generativelanguage.googleapis.com/generate_content_free_tier_input_token_count",
				}},
			},
			{"@type": retryInfoType, "retryDelay": "20.090700289s"},
		},
	}

	detail := readQuotaDetail(apiErr)

	if detail.RetryDelay == nil {
		t.Fatal("the server said how long to wait and nothing read it")
	}
	if *detail.RetryDelay != 20090700289*time.Nanosecond {
		t.Errorf("retry delay is %v, want the 20.090700289s the server stated", *detail.RetryDelay)
	}
	if kind := classifyExhaustion(detail); kind != provider.KindRateLimited {
		t.Errorf("a per-minute token limit was classified %s, so the run gave up on something that clears in 20 seconds", kind)
	}
}

// The opposite mistake, which costs more: retrying against an allowance that
// does not come back today burns what is left of it.
func TestADailyAllowanceIsNotWaitedOut(t *testing.T) {
	for _, quotaID := range []string{
		"GenerateRequestsPerDayPerProjectPerModel-FreeTier",
		"generate_content_free_tier_requests_per_day",
	} {
		detail := quotaDetail{QuotaID: quotaID}
		if kind := classifyExhaustion(detail); kind != provider.KindQuotaExhausted {
			t.Errorf("%s was classified %s; retrying will not reach the other side of it", quotaID, kind)
		}
	}
}

// The decision must not rest on the English sentence. Prose is written for
// people and changes without notice, and behaviour that depends on it is
// behaviour nobody versioned.
func TestClassificationIgnoresTheProse(t *testing.T) {
	delay := 20 * time.Second

	// Identical wording, opposite windows.
	perMinute := quotaDetail{QuotaID: "SomethingPerMinute-FreeTier", RetryDelay: &delay}
	perDay := quotaDetail{QuotaID: "SomethingPerDay-FreeTier", RetryDelay: &delay}

	if classifyExhaustion(perMinute) != provider.KindRateLimited {
		t.Error("a per-minute window was not treated as worth waiting out")
	}
	// Even with a delay offered: the window is what decides, and a day is not
	// a wait somebody sits through.
	if classifyExhaustion(perDay) != provider.KindQuotaExhausted {
		t.Error("a daily allowance was treated as a short wait because a delay was attached")
	}
}

// An unnamed window with no delay offered is the case where guessing is worst,
// so it fails closed.
func TestAnUnrecognisedLimitDoesNotRetryBlindly(t *testing.T) {
	if kind := classifyExhaustion(quotaDetail{}); kind != provider.KindQuotaExhausted {
		t.Errorf("an unrecognised 429 with no stated delay was classified %s", kind)
	}

	delay := 5 * time.Second
	if kind := classifyExhaustion(quotaDetail{RetryDelay: &delay}); kind != provider.KindRateLimited {
		t.Errorf("a 429 that volunteered a delay was classified %s", kind)
	}
}

// Details that are missing, malformed, or of a shape nobody expected must not
// take the process down: this runs on whatever the other side chose to send.
func TestMalformedDetailsAreSurvived(t *testing.T) {
	for _, details := range [][]map[string]any{
		nil,
		{{"@type": retryInfoType}},
		{{"@type": retryInfoType, "retryDelay": "not a duration"}},
		{{"@type": retryInfoType, "retryDelay": 20}},
		{{"@type": quotaFailureType, "violations": "not a list"}},
		{{"@type": quotaFailureType, "violations": []any{"not an object"}}},
		{{"no type at all": true}},
	} {
		detail := readQuotaDetail(genai.APIError{Code: 429, Details: details})
		if detail.RetryDelay != nil {
			t.Errorf("a delay was invented from %v", details)
		}
	}
}
