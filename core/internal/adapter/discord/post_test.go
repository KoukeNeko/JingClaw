package discord

import (
	"fmt"
	"github.com/disgoorg/disgo/discord"

	"encoding/json"
	"log/slog"
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

// Every state a reader would act on renders; anything invented does not.
//
// "completed" renders now, where once it did not. That is not a change of
// mind about whether a channel wants told the obvious: these lines rewrite one
// message rather than accumulating, so the completed one replaces the line
// that would otherwise sit above the answer still claiming to be working.
func TestStatusRendersEveryStateAReaderWouldActOn(t *testing.T) {
	cases := map[string]bool{
		"running":   true,
		"working":   false,
		"completed": true,
		"failed":    true,
		"cancelled": true,
		"queued":    false,
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

func TestRunningStatusIsProviderAcknowledgement(t *testing.T) {
	if rendered := renderStatus(jcgateway.StatusPayload{State: "running"}); rendered != "👀" {
		t.Fatalf("running status = %q, want eyes acknowledgement", rendered)
	}
}

// The line says what it is doing, and the thing it is doing is the useful part.
func TestTheWorkingLineNamesTheTool(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{State: "working", Detail: "read_file notes.txt"})

	if rendered != "" {
		t.Errorf("the working status was not removed: %q", rendered)
	}
}

func TestStatusReactionsFollowRunLifecycle(t *testing.T) {
	for state, want := range map[string]string{
		"running":          "",
		"working":          "",
		"network_started":  "🌍",
		"provider_started": "🧠",
		"completed":        "✅",
		"failed":           "✅",
		"cancelled":        "✅",
	} {
		got, remove := reactionForStatus(state)
		if got != want || remove {
			t.Errorf("reactionForStatus(%q) = %q, want %q", state, got, want)
		}
	}
	if _, remove := reactionForStatus("network_finished"); !remove {
		t.Error("network_finished did not request removing the earth reaction")
	}
}

func TestCompletedStatusKeepsItsTextSummary(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:      "completed",
		DurationMS: 23900,
		Summary: &jcgateway.RunSummary{
			Provider:     "ollama",
			Model:        "gemma4:31b-cloud",
			InputTokens:  32800,
			OutputTokens: 306,
			Tools:        []jcgateway.ToolUse{{Name: "web_read", Calls: 2}},
		},
	})

	want := "-# • web_read ×2\n-# ⏱ 23.9s · ↑32.8K ↓306 (33.1K) · 13 tok/s · ollama/gemma4:31b-cloud"
	if rendered != want {
		t.Fatalf("completed status = %q, want %q", rendered, want)
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

func TestMessagePayloadNormalizesLatexSymbolsForDiscord(t *testing.T) {
	rendered, err := renderDispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: `{"text":"A \\rightarrow B and C \\leftrightarrow D"}`,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered != "A → B and C ↔ D" {
		t.Errorf("got %q, want normalized symbols", rendered)
	}
}

func TestMessagePayloadRemovesUnsupportedLatexFormatting(t *testing.T) {
	rendered := normalizeDiscordText(`$\color{red}{\text{上漲 356.23 點}}$，\rightarrow 目標`)
	if rendered != "上漲 356.23 點，→ 目標" {
		t.Errorf("got %q, want readable Discord text", rendered)
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

// Two mentions in one channel are one conversation.
//
// Keying on the arriving message's id gave each its own session, which from
// the channel looks exactly like an agent with no memory: ask it something,
// say "go ahead", and it has never heard of you.
func TestOneChannelIsOneConversation(t *testing.T) {
	first := jcgateway.ConversationRef{
		Platform: jcgateway.PlatformDiscord, AccountID: "main",
		TenantID: "guild_1", ChannelID: "channel_1",
	}
	second := first

	if first.Key() != second.Key() {
		t.Errorf("two messages in one channel have different keys:\n%s\n%s",
			first.Key(), second.Key())
	}
}

// A thread is how somebody says they want a separate conversation, so it has
// to actually be one.
func TestAThreadIsItsOwnConversation(t *testing.T) {
	channel := jcgateway.ConversationRef{
		Platform: jcgateway.PlatformDiscord, AccountID: "main",
		TenantID: "guild_1", ChannelID: "channel_1",
	}
	thread := channel
	thread.ThreadID = "thread_1"

	if channel.Key() == thread.Key() {
		t.Error("a thread shares its channel's history")
	}

	other := thread
	other.ThreadID = "thread_2"
	if thread.Key() == other.Key() {
		t.Error("two threads share one history")
	}
}

// Different channels, and different guilds, must not share a history either.
func TestConversationsAreSeparatedByWhereTheyAre(t *testing.T) {
	base := jcgateway.ConversationRef{
		Platform: jcgateway.PlatformDiscord, AccountID: "main",
		TenantID: "guild_1", ChannelID: "channel_1",
	}

	elsewhere := base
	elsewhere.ChannelID = "channel_2"
	if base.Key() == elsewhere.Key() {
		t.Error("two channels share one history")
	}

	otherGuild := base
	otherGuild.TenantID = "guild_2"
	if base.Key() == otherGuild.Key() {
		t.Error("two servers share one history")
	}
}

// The status line belongs to its run, not to the channel.
//
// Keyed by channel, a new run edited the line the previous one left behind —
// which by then was sitting at the bottom of the previous answer, so asking a
// fresh question rewrote the tail of the last one.
func TestTheStatusLineBelongsToItsRun(t *testing.T) {
	adapter := New(Config{AccountID: "main", Logger: slog.New(slog.DiscardHandler)}, nil)

	adapter.setStatus("run_1", 111)

	if _, ok := adapter.liveStatus("run_2"); ok {
		t.Error("a new run inherited the previous run's message")
	}

	found, ok := adapter.liveStatus("run_1")
	if !ok || found != 111 {
		t.Errorf("the run lost its own line: %v %v", found, ok)
	}
}

// A run that has ended will not say anything else, so its line is released —
// otherwise the map grows for the life of the process and, worse, the id stays
// available to be edited by mistake.
func TestAFinishedRunReleasesItsLine(t *testing.T) {
	for payload, final := range map[string]bool{
		`{"state":"completed","detail":"12s"}`: true,
		`{"state":"failed","detail":"boom"}`:   true,
		`{"state":"cancelled"}`:                true,
		`{"state":"running"}`:                  false,
		`{"state":"working","detail":"grep"}`:  false,
		`not json`:                             true,
	} {
		if got := isFinalStatus(payload); got != final {
			t.Errorf("%s treated as final=%v, want %v", payload, got, final)
		}
	}
}

// The answer does not release the line. It used to, which is why "Done in 12s"
// arrived as a second message below the answer instead of rewriting the line
// above it.
func TestAnAnswerDoesNotReleaseTheStatusLine(t *testing.T) {
	adapter := New(Config{AccountID: "main", Logger: slog.New(slog.DiscardHandler)}, nil)
	adapter.setStatus("run_1", 111)

	// Whatever Post does with a message dispatch, the run's line has to still
	// be there for the completion that follows it.
	if _, ok := adapter.liveStatus("run_1"); !ok {
		t.Fatal("the line was released before the run finished")
	}

	adapter.clearStatus("run_1")
	if _, ok := adapter.liveStatus("run_1"); ok {
		t.Error("the line survived being released")
	}
}

// A version of an answer that is still being written extends the message it is
// growing in; the last one settles it.
func TestAPartialAnswerIsRecognised(t *testing.T) {
	partial := jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: `{"text":"so far","message_id":"msg_1"}`,
	}
	answer, streaming := answerInProgress(partial)
	if !streaming || answer != "msg_1" {
		t.Errorf("a partial answer was not recognised: %q %v", answer, streaming)
	}

	final := jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: `{"text":"the whole thing","message_id":"msg_1","final":true}`,
	}
	if _, streaming := answerInProgress(final); streaming {
		t.Error("the final answer was treated as one still being written")
	}
	if got := answerOf(final); got != "msg_1" {
		t.Errorf("the final answer names %q", got)
	}

	// A status line is not an answer, however often it is rewritten.
	status := jcgateway.Dispatch{
		Kind:    jcgateway.DispatchStatus,
		Payload: `{"state":"working"}`,
	}
	if _, streaming := answerInProgress(status); streaming {
		t.Error("a status line was treated as an answer being written")
	}

	// And an answer from before streaming existed still has to post.
	old := jcgateway.Dispatch{Kind: jcgateway.DispatchMessage, Payload: `{"text":"hello"}`}
	if _, streaming := answerInProgress(old); streaming {
		t.Error("an answer with no id was treated as one being written")
	}
}

// While it is being written, an answer occupies one message. Deciding between
// several messages and a file belongs to the final version, when the whole
// thing is known.
func TestAPartialAnswerStaysInOneMessage(t *testing.T) {
	short := "still writing"
	shown, cut := boundToOneMessage(short)
	if shown != short || cut {
		t.Errorf("a short partial was altered: %q %v", shown, cut)
	}

	long := strings.Repeat("a sentence of a plausible length. ", 200)
	shown, cut = boundToOneMessage(long)

	if !cut {
		t.Fatal("a partial past one message was not cut")
	}
	if len(shown) > maxMessageLength {
		t.Errorf("the shown part is %d bytes, over Discord's %d", len(shown), maxMessageLength)
	}
	if !strings.HasSuffix(shown, ellipsis) {
		t.Error("the shown part does not say there is more coming")
	}
}

// Two answers being written at once must not share a message.
func TestEachAnswerGrowsInItsOwnMessage(t *testing.T) {
	adapter := New(Config{AccountID: "main", Logger: slog.New(slog.DiscardHandler)}, nil)

	adapter.setAnswer("msg_1", 111)
	adapter.setAnswer("msg_2", 222)

	if id, _ := adapter.liveAnswer("msg_1"); id != 111 {
		t.Errorf("msg_1 is growing in %v", id)
	}
	if id, _ := adapter.liveAnswer("msg_2"); id != 222 {
		t.Errorf("msg_2 is growing in %v", id)
	}

	adapter.clearAnswer("msg_1")
	if _, ok := adapter.liveAnswer("msg_1"); ok {
		t.Error("a finished answer kept its message")
	}
	if _, ok := adapter.liveAnswer("msg_2"); !ok {
		t.Error("finishing one answer released another")
	}

	// An answer with no id never had a message of its own.
	if _, ok := adapter.liveAnswer(""); ok {
		t.Error("an empty id found a message")
	}
}

// Discord does not always label a file. Refusing every unlabelled attachment
// would mean silently ignoring pictures for a reason nobody could see — and
// the guess here is only about whether to spend a download, because the
// ingress checks the bytes regardless.
func TestAFileWithNoLabelIsGuessedFromItsName(t *testing.T) {
	for name, want := range map[string]string{
		"shot.png":    "image/png",
		"photo.JPG":   "image/jpeg",
		"thing.jpeg":  "image/jpeg",
		"anim.webp":   "image/webp",
		"notes.txt":   "",
		"archive.zip": "",
		"noextension": "",
	} {
		got := contentTypeOf(discord.Attachment{Filename: name})
		if got != want {
			t.Errorf("%s guessed as %q, want %q", name, got, want)
		}
	}

	// A label that is there wins over the name, because it is the more
	// specific thing the platform knew.
	labelled := "image/webp"
	if got := contentTypeOf(discord.Attachment{
		Filename: "shot.png", ContentType: &labelled,
	}); got != labelled {
		t.Errorf("the label was ignored in favour of the name: %q", got)
	}
}

// The summary sits under an answer somebody is reading, so it has to say the
// three things they would ask and stop.
func TestSummaryReportsToolsSourcesAndCost(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{
		Tools: []jcgateway.ToolUse{
			{Name: "web_read", Calls: 2},
			{Name: "read_file", Calls: 3, Failed: 1},
		},
		Sources: []jcgateway.Source{
			{Kind: "web", Ref: "https://example.com/docs", Retained: true},
			{Kind: "file", Ref: "notes.md", Retained: true},
		},
		InputTokens:       4231,
		CachedInputTokens: 3000,
		OutputTokens:      380,
	})

	for _, want := range []string{
		"web_read ×2",
		"read_file ×3 (1 failed)",
		"https://example.com/docs",
		"notes.md",
		"4231 in / 380 out",
		"3000 of the input was cached",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, rendered)
		}
	}
}

func TestCompletedStatusUsesCompactUsageLine(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:      "completed",
		DurationMS: 36600,
		Summary: &jcgateway.RunSummary{
			Provider:     "google-ai-studio",
			Model:        "gemma-4-31b-it",
			InputTokens:  15100,
			OutputTokens: 648,
			Tools:        []jcgateway.ToolUse{{Name: "read_file", Calls: 2}},
		},
	})

	want := "-# • read_file ×2\n-# ⏱ 36.6s · ↑15.1K ↓648 (15.7K) · 18 tok/s · google-ai-studio/gemma-4-31b-it"
	if rendered != want {
		t.Fatalf("completed status = %q, want %q", rendered, want)
	}
}

// The claim the summary must never make. Nothing here can observe a model
// declining to rely on something it was shown; material folded into a summary
// may well have shaped the answer through that summary. Calling it unused
// would be inventing a causal fact out of a structural one.
func TestSummaryNeverClaimsASourceWentUnused(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{
		Sources: []jcgateway.Source{
			{Kind: "web", Ref: "https://example.com/kept", Retained: true},
			{Kind: "web", Ref: "https://example.com/folded", Retained: false},
		},
	})

	for _, forbidden := range []string{"unused", "not used", "ignored", "discarded"} {
		if strings.Contains(strings.ToLower(rendered), forbidden) {
			t.Errorf("the summary claims a source went unused (%q):\n%s", forbidden, rendered)
		}
	}
	if !strings.Contains(rendered, "folded into a summary") {
		t.Errorf("the summary does not say what actually happened:\n%s", rendered)
	}
}

// Zero means the provider reported nothing, not that nothing was spent.
func TestSummaryOmitsTokensRatherThanReportingZero(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:   "completed",
		Summary: &jcgateway.RunSummary{Tools: []jcgateway.ToolUse{{Name: "grep", Calls: 1}}},
	})

	if strings.Contains(rendered, "0 in") || strings.Contains(rendered, "in / 0 out") {
		t.Errorf("an unreported token count was printed as zero:\n%s", rendered)
	}
}

// Work that failed halfway was still paid for, and that is the case a reader
// most often wants accounted for.
func TestAFailedRunStillAccountsForItself(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:  "failed",
		Detail: "the model gave up",
		Summary: &jcgateway.RunSummary{
			Tools:        []jcgateway.ToolUse{{Name: "web_read", Calls: 1, Failed: 1}},
			InputTokens:  12000,
			OutputTokens: 90,
		},
	})

	if !strings.Contains(rendered, "the model gave up") {
		t.Errorf("the failure reason is gone:\n%s", rendered)
	}
	if !strings.Contains(rendered, "12.0k in / 90 out") {
		t.Errorf("a failed run does not report its cost:\n%s", rendered)
	}
}

// A run with nothing to report adds nothing. A summary that is always there
// and usually empty is noise under every answer.
func TestNoSummaryAddsNothing(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{State: "completed", Detail: "3s"})

	if rendered != "-# ⏱ 3s" {
		t.Errorf("an empty summary added something:\n%q", rendered)
	}
}

// A status line does not go through the splitter, and Discord refuses an
// oversized message outright — so an unbounded summary would take the "Done"
// line down with it. Worse than no summary at all.
func TestSummaryStaysPostable(t *testing.T) {
	sources := make([]jcgateway.Source, 0, 12)
	for i := range 12 {
		sources = append(sources, jcgateway.Source{
			Kind: "web",
			// The shape that actually causes this: a real URL with a few
			// hundred characters of tracking parameters on the end.
			Ref: "https://example.com/a/very/long/path/segment/" + strings.Repeat("tracking-parameter-", 20) +
				string(rune('a'+i)),
			Retained: i%2 == 0,
		})
	}

	rendered := renderSummary(&jcgateway.RunSummary{Sources: sources, InputTokens: 90000, OutputTokens: 1200})

	if len(rendered) > maxMessageLength {
		t.Fatalf("the status line is %d characters, over Discord's %d limit",
			len(rendered), maxMessageLength)
	}
	// Bounded, not silent: the reader still learns what the run cost.
	for _, want := range []string{"90.0k in / 1200 out"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("bounding the summary lost %q:\n%s", want, rendered)
		}
	}
}

// One absurd address must not crowd out the others when the list does fit.
func TestALongAddressIsShortenedNotDropped(t *testing.T) {
	rendered := renderSummary(&jcgateway.RunSummary{Sources: []jcgateway.Source{
		{Kind: "web", Ref: "https://example.com/" + strings.Repeat("x", 400), Retained: true},
		{Kind: "file", Ref: "notes.md", Retained: true},
	}})

	if !strings.Contains(rendered, "https://example.com/xxx") {
		t.Errorf("the long address was dropped entirely:\n%s", rendered)
	}
	if !strings.Contains(rendered, "…") {
		t.Errorf("the long address was not shortened:\n%s", rendered)
	}
	if !strings.Contains(rendered, "notes.md") {
		t.Errorf("a long address crowded out the one after it:\n%s", rendered)
	}
}

// The tool list has no bound of its own: an MCP server may register many tools
// with names nobody here chose. Whatever it produces still has to be postable.
func TestAnEnormousToolListIsStillPostable(t *testing.T) {
	tools := make([]jcgateway.ToolUse, 0, 200)
	for i := range 200 {
		tools = append(tools, jcgateway.ToolUse{
			Name:  fmt.Sprintf("mcp_some_server_a_rather_long_tool_name_number_%d", i),
			Calls: 1,
		})
	}

	rendered := renderSummary(&jcgateway.RunSummary{Tools: tools, InputTokens: 500, OutputTokens: 10})

	if len(rendered) > maxMessageLength {
		t.Fatalf("the status line is %d characters, over Discord's %d limit",
			len(rendered), maxMessageLength)
	}
	if !strings.Contains(rendered, "too long to post in full") {
		t.Errorf("the summary was cut without saying so:\n%s", rendered)
	}
}

// A dispatch carrying a file is posted as one, rather than having its bytes
// pasted into the channel as text.
func TestADispatchCarryingAFileIsRecognised(t *testing.T) {
	payload, err := json.Marshal(jcgateway.MessagePayload{
		Final: true,
		File: &jcgateway.MessageFile{
			Name:      "abc123.txt",
			Content:   []byte("the whole build log"),
			MediaType: "text/plain",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	file, carried := attachedFile(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(payload),
	})
	if !carried {
		t.Fatal("a dispatch carrying a file was not recognised")
	}
	if string(file.Content) != "the whole build log" {
		t.Errorf("the content is %q", file.Content)
	}
}

// An ordinary answer is not mistaken for one.
func TestAnOrdinaryMessageCarriesNoFile(t *testing.T) {
	payload, err := json.Marshal(jcgateway.MessagePayload{Text: "hello", Final: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, carried := attachedFile(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(payload),
	}); carried {
		t.Error("an ordinary answer was treated as a file")
	}

	// An empty file is not a file either: posting an attachment with no bytes
	// would be a message that says nothing and cannot be opened.
	empty, err := json.Marshal(jcgateway.MessagePayload{File: &jcgateway.MessageFile{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, carried := attachedFile(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(empty),
	}); carried {
		t.Error("a file with no content was posted")
	}
}

// A console channel is a log. What ran, how long it took, whether it worked.
func TestALogLineSaysWhatRanAndHowLongItTook(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool:       "exec_command",
		Summary:    "go test ./...: ok",
		DurationMS: 1800,
		Artifact:   "sha256-abc123",
	})

	for _, want := range []string{"exec_command", "1.8s", "go test", "sha256-abc123"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the log line does not mention %q:\n%s", want, rendered)
		}
	}
	// Subtext, because a log is context for the answer rather than the answer.
	if !strings.HasPrefix(rendered, "-# ") {
		t.Errorf("a log line is shown at the same weight as the reply:\n%s", rendered)
	}
}

func TestAFailedCallIsMarkedAsOne(t *testing.T) {
	rendered := renderLog(jcgateway.LogPayload{
		Tool: "web_read", Summary: "404", DurationMS: 90, IsError: true,
	})
	if !strings.Contains(rendered, "✗") {
		t.Errorf("a failure is not distinguishable from a success:\n%s", rendered)
	}
}

// Durations span milliseconds to minutes, and one format cannot carry that
// range without losing the fast ones or padding the slow ones.
func TestDurationsAreReadableAtEveryScale(t *testing.T) {
	for ms, want := range map[int64]string{
		5:       "5ms",
		999:     "999ms",
		1500:    "1.5s",
		59_000:  "59.0s",
		61_000:  "1m01s",
		600_000: "10m00s",
	} {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

// Which model answered, because that changes and the answers change with it.
func TestTheSummarySaysWhoAnswered(t *testing.T) {
	rendered := renderStatus(jcgateway.StatusPayload{
		State:  "completed",
		Detail: "12s",
		Summary: &jcgateway.RunSummary{
			Provider:     "ollama",
			Model:        "gemma4:31b-cloud",
			Tools:        []jcgateway.ToolUse{{Name: "read_file", Calls: 2, Milliseconds: 40}},
			InputTokens:  100,
			OutputTokens: 20,
		},
	})

	if !strings.Contains(rendered, "ollama/gemma4:31b-cloud") {
		t.Errorf("the summary does not say who answered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "-# • read_file ×2") {
		t.Errorf("the completion status does not list its tools:\n%s", rendered)
	}
}
