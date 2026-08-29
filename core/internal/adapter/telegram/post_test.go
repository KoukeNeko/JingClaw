package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

func dispatchWith(t *testing.T, kind jcgateway.DispatchKind, runID domain.RunID, payload any) jcgateway.Dispatch {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return jcgateway.Dispatch{
		ID:      "dsp_1",
		RunID:   runID,
		Kind:    kind,
		Payload: string(encoded),
		Target:  jcgateway.ConversationRef{Platform: Platform, ChannelID: "555"},
	}
}

func bodyText(t *testing.T, one call) string {
	t.Helper()
	text, ok := one.Body["text"].(string)
	if !ok {
		t.Fatalf("the %s call carried no text: %+v", one.Method, one.Body)
	}
	return text
}

// An answer longer than Telegram accepts must be split, not trimmed: dropping
// the tail silently is worse than posting it in two parts.
func TestALongAnswerIsSplitRatherThanTrimmed(t *testing.T) {
	one, backend := newTestAdapter(t, nil)

	long := strings.Repeat("a sentence of a plausible length. ", 300)
	dispatch := dispatchWith(t, jcgateway.DispatchMessage, "run_1",
		jcgateway.MessagePayload{Text: long})

	ids, err := one.Post(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	sends := backend.sent("sendMessage")
	if len(sends) < 2 {
		t.Fatalf("posted %d messages; the answer should not have fitted in one", len(sends))
	}
	if len(ids) != len(sends) {
		t.Errorf("returned %d ids for %d messages", len(ids), len(sends))
	}

	var posted strings.Builder
	for _, send := range sends {
		text := bodyText(t, send)
		if len([]rune(text)) > maxMessageLength {
			t.Errorf("a segment is %d characters, over the %d limit",
				len([]rune(text)), maxMessageLength)
		}
		posted.WriteString(text)
	}
	if !strings.Contains(strings.Join(strings.Fields(posted.String()), " "),
		strings.Join(strings.Fields(long[:200]), " ")) {
		t.Error("the opening of the answer did not survive the split")
	}
}

// A status line is the answer to "what is it doing now", and the previous
// answer to that is of no interest. A run that touches ten files must not
// leave ten lines behind it.
func TestTheStatusLineIsRewrittenInPlace(t *testing.T) {
	one, backend := newTestAdapter(t, nil)
	ctx := context.Background()

	for _, detail := range []string{"read_file", "search_web", "write_file"} {
		dispatch := dispatchWith(t, jcgateway.DispatchStatus, "run_1",
			jcgateway.StatusPayload{State: "working", Detail: detail})
		if _, err := one.Post(ctx, dispatch); err != nil {
			t.Fatalf("post %s: %v", detail, err)
		}
	}

	if sends := backend.sent("sendMessage"); len(sends) != 1 {
		t.Errorf("posted %d status messages, want one that is then edited", len(sends))
	}
	if edits := backend.sent("editMessageText"); len(edits) != 2 {
		t.Errorf("made %d edits, want one per later status", len(edits))
	}
}

// A finished run releases its line. Held, the next run in the same chat would
// rewrite a line sitting at the bottom of the previous answer.
func TestAFinishedRunReleasesItsLine(t *testing.T) {
	one, _ := newTestAdapter(t, nil)
	ctx := context.Background()

	working := dispatchWith(t, jcgateway.DispatchStatus, "run_1",
		jcgateway.StatusPayload{State: "working", Detail: "read_file"})
	if _, err := one.Post(ctx, working); err != nil {
		t.Fatalf("post working: %v", err)
	}
	done := dispatchWith(t, jcgateway.DispatchStatus, "run_1",
		jcgateway.StatusPayload{State: "completed", DurationMS: 1200})
	if _, err := one.Post(ctx, done); err != nil {
		t.Fatalf("post completed: %v", err)
	}

	if _, held := one.liveStatus("run_1"); held {
		t.Error("the finished run is still holding a status line")
	}
}

// Each run edits its own line. Keyed by chat, a new run would rewrite the tail
// of the previous answer.
func TestTheStatusLineBelongsToItsRun(t *testing.T) {
	one, backend := newTestAdapter(t, nil)
	ctx := context.Background()

	for _, run := range []domain.RunID{"run_1", "run_2"} {
		dispatch := dispatchWith(t, jcgateway.DispatchStatus, run,
			jcgateway.StatusPayload{State: "working", Detail: "read_file"})
		if _, err := one.Post(ctx, dispatch); err != nil {
			t.Fatalf("post %s: %v", run, err)
		}
	}

	if sends := backend.sent("sendMessage"); len(sends) != 2 {
		t.Errorf("posted %d lines for two runs, want one each", len(sends))
	}
	if edits := backend.sent("editMessageText"); len(edits) != 0 {
		t.Errorf("made %d edits; one run rewrote the other's line", len(edits))
	}
}

// Telegram refuses a message whose entities do not balance, and a model writes
// an unmatched asterisk often enough that formatting would mean occasionally
// posting nothing at all.
func TestNothingIsSentWithAParseMode(t *testing.T) {
	one, backend := newTestAdapter(t, nil)

	dispatch := dispatchWith(t, jcgateway.DispatchMessage, "run_1",
		jcgateway.MessagePayload{Text: "an answer with an *unmatched asterisk"})
	if _, err := one.Post(context.Background(), dispatch); err != nil {
		t.Fatalf("post: %v", err)
	}

	for _, send := range backend.sent("sendMessage") {
		if mode, set := send.Body["parse_mode"]; set {
			t.Errorf("a message was sent with parse_mode %v, which Telegram may refuse", mode)
		}
	}
}

// A refusal must not carry the request. The bot token is in the URL, so an
// error that quoted it would put the credential in the log.
func TestAnErrorNeverCarriesTheToken(t *testing.T) {
	one, backend := newTestAdapter(t, nil)
	backend.mu.Lock()
	backend.fail["sendMessage"] = response[json.RawMessage]{
		ErrorCode: 429, Description: "Too Many Requests: retry after 30",
		Parameters: &responseParameters{RetryAfter: 30},
	}
	backend.mu.Unlock()

	dispatch := dispatchWith(t, jcgateway.DispatchMessage, "run_1",
		jcgateway.MessagePayload{Text: "hello"})
	_, err := one.Post(context.Background(), dispatch)
	if err == nil {
		t.Fatal("a refused send reported success")
	}
	if strings.Contains(err.Error(), "not-a-real-token") {
		t.Errorf("the error carries the bot token: %s", err)
	}

	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("the error is not an APIError: %T", err)
	}
	if refusal.RetryAfter == 0 {
		t.Error("retry_after was dropped, so a caller cannot honour it")
	}
}

// An answer handed over as a file is uploaded, not pasted into the chat.
func TestAFileIsUploaded(t *testing.T) {
	one, backend := newTestAdapter(t, nil)

	dispatch := dispatchWith(t, jcgateway.DispatchMessage, "run_1", jcgateway.MessagePayload{
		Text: "here it is",
		File: &jcgateway.MessageFile{Name: "answer.md", Content: []byte("# the whole answer")},
	})
	if _, err := one.Post(context.Background(), dispatch); err != nil {
		t.Fatalf("post: %v", err)
	}

	uploads := backend.sent("sendDocument")
	if len(uploads) != 1 {
		t.Fatalf("made %d uploads, want one", len(uploads))
	}
	if sends := backend.sent("sendMessage"); len(sends) != 0 {
		t.Errorf("the file was also pasted into the chat as %d messages", len(sends))
	}
	multipart, _ := uploads[0].Body["_multipart"].(string)
	if !strings.Contains(multipart, "answer.md") || !strings.Contains(multipart, "the whole answer") {
		t.Error("the upload does not carry the file")
	}
}
