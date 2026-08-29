package discord

import (
	"github.com/disgoorg/disgo/discord"

	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// Discord refuses an over-long message outright, so splitting has to be
// correct: silently losing the tail of an answer is worse than posting it in
// two parts.
func TestStatusReactionsFollowRunLifecycle(t *testing.T) {
	for state, want := range map[string]string{
		"running":          "",
		"working":          "",
		"network_started":  "🌍",
		"memory_started":   "📓",
		"provider_started": "🧠",
		"completed":        "✅",
		"failed":           "❌",
		"cancelled":        "🛑",
	} {
		got, remove := reactionForStatus(state)
		if got != want || remove {
			t.Errorf("reactionForStatus(%q) = %q, want %q", state, got, want)
		}
	}
	if _, remove := reactionForStatus("network_finished"); !remove {
		t.Error("network_finished did not request removing the earth reaction")
	}
	if emoji, remove := reactionForStatus("memory_finished"); emoji != "" || remove {
		t.Errorf("memory_finished = %q, remove=%v; want notebook retained", emoji, remove)
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

	if segments := render.Split(short, discordStyle); len(segments) > defaultMaxMessages {
		t.Fatalf("the short answer already needs %d messages; the test proves nothing",
			len(segments))
	}
	if segments := render.Split(long, discordStyle); len(segments) <= defaultMaxMessages {
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
