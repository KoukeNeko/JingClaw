package gateway

import (
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

func requested(record *runRecord, id, name, arguments string) {
	record.requested(domain.ToolCallRequested{
		CallID:    domain.ToolCallID(id),
		Name:      name,
		Arguments: arguments,
	})
}

func completed(record *runRecord, id, name string, seq domain.Seq, failed bool) {
	record.completed(domain.ToolCallCompleted{
		CallID:  domain.ToolCallID(id),
		Name:    name,
		IsError: failed,
	}, seq)
}

func TestCountsToolCallsAndFailures(t *testing.T) {
	record := newRunRecord()

	requested(record, "1", "read_file", `{"path":"a.go"}`)
	completed(record, "1", "read_file", 10, false)
	requested(record, "2", "read_file", `{"path":"b.go"}`)
	completed(record, "2", "read_file", 11, true)
	requested(record, "3", "grep", `{"query":"x"}`)
	completed(record, "3", "grep", 12, false)

	summary := record.summarise()

	if len(summary.Tools) != 2 {
		t.Fatalf("tools: %+v", summary.Tools)
	}
	// Most-used first, so the same run always reads the same way.
	if summary.Tools[0].Name != "read_file" || summary.Tools[0].Calls != 2 {
		t.Errorf("first tool is %+v", summary.Tools[0])
	}
	if summary.Tools[0].Failed != 1 {
		t.Errorf("a failed call was not counted: %+v", summary.Tools[0])
	}
}

// A fetch that failed is not a source. Listing the address it tried would put
// pages the agent never read into an account of what it drew on.
func TestAFailedCallIsNotASource(t *testing.T) {
	record := newRunRecord()

	requested(record, "1", "web_read", `{"url":"https://example.com/gone"}`)
	completed(record, "1", "web_read", 10, true)

	if sources := record.summarise().Sources; len(sources) != 0 {
		t.Errorf("a failed fetch was listed as a source: %+v", sources)
	}
}

func TestTheSamePageReadTwiceIsOneSource(t *testing.T) {
	record := newRunRecord()

	requested(record, "1", "web_read", `{"url":"https://example.com/"}`)
	completed(record, "1", "web_read", 10, false)
	requested(record, "2", "web_read", `{"url":"https://example.com/"}`)
	completed(record, "2", "web_read", 11, false)

	summary := record.summarise()
	if len(summary.Sources) != 1 {
		t.Errorf("the same page counted twice: %+v", summary.Sources)
	}
	if summary.Tools[0].Calls != 2 {
		t.Errorf("the second call was not counted: %+v", summary.Tools[0])
	}
}

// Only things brought in from outside the agent's own reasoning count. A grep
// searches material already accounted for.
func TestOnlyContentBearingToolsProduceSources(t *testing.T) {
	record := newRunRecord()

	for i, call := range []struct{ name, arguments string }{
		{"grep", `{"query":"needle"}`},
		{"glob_files", `{"pattern":"**/*.go"}`},
		{"exec_command", `{"program":"go"}`},
		{"recall", `{"query":"what did they say"}`},
	} {
		id := string(rune('a' + i))
		requested(record, id, call.name, call.arguments)
		completed(record, id, call.name, domain.Seq(10+i), false)
	}

	if sources := record.summarise().Sources; len(sources) != 0 {
		t.Errorf("a search or a memory was counted as a source: %+v", sources)
	}
}

// The one claim this can actually make: whether the content itself was still
// in front of the model, as opposed to whatever a summary said about it.
func TestCompactionMarksEarlierSourcesAsNoLongerThemselves(t *testing.T) {
	record := newRunRecord()

	requested(record, "1", "web_read", `{"url":"https://example.com/early"}`)
	completed(record, "1", "web_read", 10, false)
	requested(record, "2", "web_read", `{"url":"https://example.com/late"}`)
	completed(record, "2", "web_read", 30, false)

	record.compactedThrough = 20

	summary := record.summarise()
	byRef := map[string]bool{}
	for _, source := range summary.Sources {
		byRef[source.Ref] = source.Retained
	}

	if byRef["https://example.com/early"] {
		t.Error("a source folded into a summary is still reported as retained")
	}
	if !byRef["https://example.com/late"] {
		t.Error("a source after the fold is not reported as retained")
	}
}

// A list that is quietly short reads as a complete account of a run that did
// less than it did.
func TestALongListIsBoundedAndSaysHowMuchIsMissing(t *testing.T) {
	record := newRunRecord()

	const read = maxListedSources + 5
	for i := range read {
		id := string(rune('a' + i))
		path := `{"path":"file` + id + `.go"}`
		requested(record, id, "read_file", path)
		completed(record, id, "read_file", domain.Seq(10+i), false)
	}

	summary := record.summarise()
	if len(summary.Sources) != maxListedSources {
		t.Errorf("listed %d sources, want %d", len(summary.Sources), maxListedSources)
	}
	if summary.SourcesOmitted != read-maxListedSources {
		t.Errorf("omitted count is %d, want %d", summary.SourcesOmitted, read-maxListedSources)
	}
}

// A run resumed after a restart never saw its own beginning. Saying so is the
// difference between an incomplete account and a wrong one.
func TestARunNotSeenFromTheStartSaysItIsPartial(t *testing.T) {
	if !newRunRecord().summarise().Partial {
		t.Error("a run whose start was never observed does not report itself partial")
	}

	seen := newRunRecord()
	seen.seen = true
	if seen.summarise().Partial {
		t.Error("a run observed from its start reports itself partial")
	}
}

// Token counts are cumulative, so the last report is the whole run's — which
// is why they stay right even when the tool list cannot.
func TestUsageIsTakenAsCumulative(t *testing.T) {
	record := newRunRecord()
	record.usage = domain.Usage{InputTokens: 100, OutputTokens: 20}
	record.usage = domain.Usage{InputTokens: 4200, CachedInputTokens: 3000, OutputTokens: 380}

	summary := record.summarise()
	if summary.InputTokens != 4200 || summary.OutputTokens != 380 {
		t.Errorf("usage is %+v", summary)
	}
	if summary.CachedInputTokens != 3000 {
		t.Errorf("cached input was lost: %+v", summary)
	}
}

// The incident this exists to prevent.
//
// A rate limit put the provider's entire reply into a channel with fourteen
// people in it: the billing endpoint, the quota metric, the account's limit,
// and a link to the pricing page. None of that is the channel's business, and
// all of it was there because the only thing the projector had to work with
// was the upstream sentence.
func TestAFailureTellsAChannelNothingAboutTheOperatorsAccount(t *testing.T) {
	leaked := "You exceeded your current quota, please check your plan and billing details. " +
		"Quota exceeded for metric: generativelanguage.googleapis.com/" +
		"generate_content_free_tier_input_token_count, limit: 16000, model: gemma-4-31b"

	said := explainFailure("rate_limited")

	for _, secret := range []string{
		"quota", "billing", "googleapis.com", "16000", "gemma-4-31b", "plan",
	} {
		if strings.Contains(strings.ToLower(said), secret) {
			t.Errorf("the channel is told %q, which leaks %q", said, secret)
		}
	}
	if strings.Contains(said, leaked) {
		t.Error("the upstream message was passed through verbatim")
	}
	// Still useful: a reader has to learn whether waiting will help.
	if !strings.Contains(said, "try again") {
		t.Errorf("the channel is not told what to do: %q", said)
	}
}

// The two situations a reader most needs told apart. Waiting fixes one and
// will never fix the other, and saying "try again shortly" to somebody whose
// allowance is gone until tomorrow wastes their evening.
func TestWaitingAndNotWaitingAreSaidDifferently(t *testing.T) {
	temporary := explainFailure("rate_limited")
	exhausted := explainFailure("quota_exhausted")

	if temporary == exhausted {
		t.Fatalf("a short rate limit and a spent allowance read identically: %q", temporary)
	}
	if !strings.Contains(temporary, "shortly") {
		t.Errorf("a temporary limit does not suggest waiting: %q", temporary)
	}
	if strings.Contains(strings.ToLower(exhausted), "shortly") {
		t.Errorf("a spent allowance suggests waiting, which will not help: %q", exhausted)
	}
	if !strings.Contains(exhausted, "operator") {
		t.Errorf("a spent allowance does not point at who can fix it: %q", exhausted)
	}
}

// Whatever arrives, including a classification this build has never heard of,
// produces something safe rather than an empty status or a passthrough.
func TestEveryFailureKindProducesSomethingSafe(t *testing.T) {
	for _, kind := range []string{
		"rate_limited", "quota_exhausted", "overloaded", "transient",
		"invalid_request", "auth", "not_found", "context_overflow",
		"content_filtered", "interrupted", "retry_budget_exhausted",
		"", "a_kind_from_a_provider_that_does_not_exist_yet",
	} {
		said := explainFailure(kind)
		if strings.TrimSpace(said) == "" {
			t.Errorf("%q produced nothing to say", kind)
		}
	}
}
