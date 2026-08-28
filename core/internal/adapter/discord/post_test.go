package discord

import (
	"encoding/json"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Discord refuses an over-long message outright, so splitting has to be
// correct: silently losing the tail of an answer is worse than posting it in
// two parts.

func TestShortTextIsPostedWhole(t *testing.T) {
	segments := splitForDiscord("a short answer")

	if len(segments) != 1 || segments[0] != "a short answer" {
		t.Errorf("got %d segments: %q", len(segments), segments)
	}
}

func TestEverySegmentFitsDiscordsLimit(t *testing.T) {
	long := strings.Repeat("This is a sentence of a plausible length. ", 300)

	for i, segment := range splitForDiscord(long) {
		if len(segment) > maxMessageLength {
			t.Errorf("segment %d is %d bytes, over the %d limit", i, len(segment), maxMessageLength)
		}
	}
}

// Nothing may be dropped: the reader is entitled to the whole answer.
func TestSplittingLosesNoWords(t *testing.T) {
	var builder strings.Builder
	for i := range 400 {
		builder.WriteString("word")
		builder.WriteString(string(rune('a' + i%26)))
		builder.WriteString(" ")
	}
	original := builder.String()

	rejoined := strings.Join(splitForDiscord(original), " ")

	for _, word := range strings.Fields(original) {
		if !strings.Contains(rejoined, word) {
			t.Fatalf("splitting lost %q", word)
			return
		}
	}
}

// A code fence broken across a boundary would render one half as code and the
// other as prose.
func TestCodeFencesAreClosedAndReopened(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("Here is the change:\n\n```go\n")
	for range 200 {
		builder.WriteString("\tfmt.Println(\"a line of code that is fairly long\")\n")
	}
	builder.WriteString("```\n")

	segments := splitForDiscord(builder.String())
	if len(segments) < 2 {
		t.Fatalf("expected the sample to split; got %d segment(s)", len(segments))
	}

	for i, segment := range segments {
		if strings.Count(segment, "```")%2 != 0 {
			t.Errorf("segment %d leaves a fence open:\n%s", i, tail(segment, 120))
		}
	}

	// The continuation has to reopen with the same language, or the second
	// half loses its highlighting.
	if !strings.HasPrefix(segments[1], "```go") {
		t.Errorf("the continuation does not reopen the fence: %q", head(segments[1], 40))
	}
}

func TestBreaksPreferParagraphThenLine(t *testing.T) {
	paragraph := strings.Repeat("word ", 300)
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph

	segments := splitForDiscord(text)
	if len(segments) < 2 {
		t.Fatalf("expected a split; got %d segment(s)", len(segments))
	}

	// A break at a paragraph boundary leaves no partial word at the seam.
	first := segments[0]
	if strings.HasSuffix(first, "wor") || strings.HasSuffix(first, "wo") {
		t.Errorf("the split cut a word in half: %q", tail(first, 40))
	}
}

// An approval message must say what will happen and where the decision is
// actually made.
func TestApprovalRendersTheActionAndWhereToDecide(t *testing.T) {
	rendered := renderApproval(jcgateway.ApprovalPayload{
		ApprovalID: "apr_123",
		ToolName:   "write_file",
		Summary:    "write_file README.md",
		Effects:    []string{"Modifies README.md", "Cannot be undone by running this again"},
	})

	for _, want := range []string{"write_file README.md", "Modifies README.md", "apr_123", "agent approve"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the approval does not mention %q:\n%s", want, rendered)
		}
	}
}

func TestStatusRendersOnlyMeaningfulStates(t *testing.T) {
	cases := map[string]bool{
		"running":   true,
		"failed":    true,
		"cancelled": true,
		"queued":    false,
		"completed": false,
	}

	for state, wantText := range cases {
		rendered := renderStatus(jcgateway.StatusPayload{State: state})
		if wantText && rendered == "" {
			t.Errorf("%q renders nothing", state)
		}
		if !wantText && rendered != "" {
			t.Errorf("%q renders %q; a reader does not need it", state, rendered)
		}
	}
}

func TestUnknownDispatchKindIsAnError(t *testing.T) {
	_, err := renderDispatch(jcgateway.Dispatch{Kind: "invented", Payload: "{}"})
	if err == nil {
		t.Error("an unrecognised dispatch kind was rendered anyway")
	}
}

func TestMessagePayloadRoundTrips(t *testing.T) {
	encoded, err := json.Marshal(jcgateway.MessagePayload{Text: "測試訊息"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rendered, err := renderDispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(encoded),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "測試訊息" {
		t.Errorf("got %q", rendered)
	}
}

// A thread is where the conversation lives; posting to its parent channel
// would scatter a threaded exchange back into the room.
func TestPostsGoToTheThreadWhenThereIsOne(t *testing.T) {
	withThread := jcgateway.ConversationRef{ChannelID: "channel_1", ThreadID: "thread_1"}
	if got := targetChannel(withThread); got != "thread_1" {
		t.Errorf("got %q, want the thread", got)
	}

	withoutThread := jcgateway.ConversationRef{ChannelID: "channel_1"}
	if got := targetChannel(withoutThread); got != "channel_1" {
		t.Errorf("got %q, want the channel", got)
	}
}

// "@JingClaw fix the tests" is a request to fix the tests. Leaving the mention
// in makes the model reason about a string of digits it has no use for.
func TestMentionIsStrippedFromTheRequest(t *testing.T) {
	const selfID = 1234567890

	cases := map[string]string{
		"<@1234567890> fix the tests":  "fix the tests",
		"<@!1234567890> fix the tests": "fix the tests",
		"fix the tests <@1234567890>":  "fix the tests",
		"  <@1234567890>   spaced  ":   "spaced",
		"no mention here":              "no mention here",
	}

	for input, want := range cases {
		if got := stripMention(input, selfID); got != want {
			t.Errorf("stripMention(%q) = %q, want %q", input, got, want)
		}
	}

	// Somebody else's mention is part of what the user wrote and stays.
	if got := stripMention("<@999> and <@1234567890> hello", selfID); !strings.Contains(got, "<@999>") {
		t.Errorf("another user's mention was removed: %q", got)
	}
}

func head(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

// An answer split into eight messages is a channel somebody has to scroll past
// for the rest of the day. Past a few, it becomes a file.
func TestALongAnswerBecomesAFile(t *testing.T) {
	short := strings.Repeat("a sentence of a plausible length. ", 40)
	long := strings.Repeat("a sentence of a plausible length. ", 400)

	if segments := splitForDiscord(short); len(segments) > defaultMaxMessages {
		t.Fatalf("the short answer already needs %d messages; the test proves nothing",
			len(segments))
	}
	if segments := splitForDiscord(long); len(segments) <= defaultMaxMessages {
		t.Fatalf("the long answer only needs %d messages; the test proves nothing",
			len(segments))
	}
}

// A bare "see attached" tells a person nothing about whether they need to open
// it, so the message carries the opening of the answer.
func TestTheLeadCarriesTheOpeningOfTheAnswer(t *testing.T) {
	body := "Here is what I found.\n\n" + strings.Repeat("detail. ", 2000)

	lead := opening(body, leadLength)

	if !strings.HasPrefix(lead, "Here is what I found.") {
		t.Errorf("the lead does not start with the answer: %q", lead)
	}
	if len(lead) > leadLength+len(ellipsis) {
		t.Errorf("the lead is %d bytes, past the %d it may take", len(lead), leadLength)
	}
	if !strings.HasSuffix(lead, ellipsis) {
		t.Error("the lead does not say it is only the beginning")
	}
}

func TestAShortAnswerIsItsOwnLead(t *testing.T) {
	const body = "All three tests pass."

	if lead := opening(body, leadLength); lead != body {
		t.Errorf("a short answer was cut: %q", lead)
	}
}

// Several files in one channel have to be tellable apart, and the name has to
// be something Discord will show rather than only offer as a download.
func TestTheFileIsNamedForItsRun(t *testing.T) {
	name := attachmentName(jcgateway.Dispatch{RunID: "run_01M123M5TY6TVHY0HKX5VCTSSP"})

	if !strings.HasSuffix(name, ".txt") {
		t.Errorf("name is %q; .txt is what Discord will preview", name)
	}
	if !strings.Contains(name, "vctssp") {
		t.Errorf("name is %q and does not identify the run", name)
	}

	// A run with no id still has to produce a usable filename.
	if bare := attachmentName(jcgateway.Dispatch{}); !strings.HasSuffix(bare, ".txt") {
		t.Errorf("a dispatch with no run id produced %q", bare)
	}
}

// An upload past what the platform takes fails the whole delivery, so it is
// bounded here — and the reader is told, rather than quietly given a file that
// stops mid-sentence.
func TestAnOversizedAnswerIsBoundedAndSaysSo(t *testing.T) {
	body := strings.Repeat("x", 100)

	content, truncated := boundAttachment(body, 40)
	if len(content) != 40 || !truncated {
		t.Fatalf("bounding produced %d bytes, truncated=%v", len(content), truncated)
	}
	if described := describeFile(len(content), truncated); !strings.Contains(described, "too long") {
		t.Errorf("the reader is not told it is partial: %q", described)
	}

	whole, truncated := boundAttachment(body, 1000)
	if len(whole) != 100 || truncated {
		t.Errorf("an answer that fits was cut: %d bytes, truncated=%v", len(whole), truncated)
	}
	if described := describeFile(len(whole), truncated); strings.Contains(described, "too long") {
		t.Errorf("a whole answer was described as partial: %q", described)
	}
}

// An approval is the thing somebody has to act on and a status line is short
// by construction. Turning either into a file would hide it behind a download.
func TestOnlyAnswersBecomeFiles(t *testing.T) {
	const overLimit = defaultMaxMessages + 1

	if !shouldSendAsFile(jcgateway.DispatchMessage, overLimit, defaultMaxMessages) {
		t.Error("a long answer was not sent as a file")
	}
	for _, kind := range []jcgateway.DispatchKind{
		jcgateway.DispatchApproval,
		jcgateway.DispatchStatus,
	} {
		if shouldSendAsFile(kind, overLimit, defaultMaxMessages) {
			t.Errorf("a %s was hidden behind a download", kind)
		}
	}

	// And an answer that fits stays in the channel, where it can be read
	// without opening anything.
	if shouldSendAsFile(jcgateway.DispatchMessage, defaultMaxMessages, defaultMaxMessages) {
		t.Error("an answer that fits was sent as a file")
	}
}
