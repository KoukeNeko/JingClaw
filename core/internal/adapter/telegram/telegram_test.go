package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// stub is Telegram as far as these tests are concerned.
//
// A fake rather than a mock of the client: what is worth checking is the
// request that would go over the wire, and a test that asserts on a method
// call proves the adapter calls its own code.
type stub struct {
	mu sync.Mutex

	// queued is handed out one batch per getUpdates call, so a test can say
	// what arrives and when.
	queued [][]update

	calls  []call
	nextID int64

	// fail, when set for a method, is the error Telegram returns for it.
	fail map[string]response[json.RawMessage]
}

type call struct {
	Method string
	Body   map[string]any
}

func newStub() *stub {
	return &stub{nextID: 1000, fail: map[string]response[json.RawMessage]{}}
}

func (s *stub) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		body := map[string]any{}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				_ = json.Unmarshal(raw, &body)
			} else {
				body["_multipart"] = string(raw)
			}
		}

		s.mu.Lock()
		s.calls = append(s.calls, call{Method: method, Body: body})
		refusal, refused := s.fail[method]
		var result any
		switch {
		case refused:
		case method == "getMe":
			result = self{ID: 42, Username: "jingclaw_bot"}
		case method == "getUpdates":
			if len(s.queued) > 0 {
				result, s.queued = s.queued[0], s.queued[1:]
			} else {
				result = []update{}
			}
		default:
			s.nextID++
			result = sentMessage{MessageID: s.nextID}
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if refused {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(refusal)
			return
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t := struct{}{}
			_ = t
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": json.RawMessage(encoded)})
	}))
}

func (s *stub) sent(method string) []call {
	s.mu.Lock()
	defer s.mu.Unlock()

	var found []call
	for _, one := range s.calls {
		if one.Method == method {
			found = append(found, one)
		}
	}
	return found
}

// collector is a sink that remembers what it was given.
type collector struct {
	mu       sync.Mutex
	messages []jcgateway.InboundMessage
	ready    chan struct{}
	want     int
}

func newCollector(want int) *collector {
	return &collector{ready: make(chan struct{}), want: want}
}

func (c *collector) Deliver(_ context.Context, message jcgateway.InboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	if len(c.messages) == c.want {
		close(c.ready)
	}
	return nil
}

func (c *collector) await(t *testing.T) []jcgateway.InboundMessage {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the messages never arrived")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messages
}

func newTestAdapter(t *testing.T, into Sink, queued ...[]update) (*Adapter, *stub) {
	t.Helper()

	backend := newStub()
	backend.queued = queued
	server := backend.serve()
	t.Cleanup(server.Close)

	one := New(Config{
		Token:     "not-a-real-token",
		AccountID: "main",
		APIBase:   server.URL,
	}, into)
	one.username = "jingclaw_bot"

	return one, backend
}

func textMessage(chatID, messageID int64, chatType, text string, entities ...entity) update {
	return update{
		UpdateID: messageID,
		Message: &message{
			MessageID: messageID,
			From:      &user{ID: 7, Username: "someone", First: "Someone"},
			Chat:      chat{ID: chatID, Type: chatType},
			Date:      time.Now().Unix(),
			Text:      text,
			Entities:  entities,
		},
	}
}

// A group message that does not name the bot is a conversation between people.
// Acting on it would make the agent a participant nobody invited.
func TestOnlyAMentionCountsInAGroup(t *testing.T) {
	into := newCollector(1)
	one, _ := newTestAdapter(t, into,
		[]update{
			textMessage(-100, 1, "group", "what did you think of the release?"),
			textMessage(-100, 2, "group", "@jingclaw_bot summarise it",
				entity{Type: "mention", Offset: 0, Length: 13}),
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if len(delivered) != 1 {
		t.Fatalf("delivered %d messages, want only the mention", len(delivered))
	}
	if delivered[0].Trigger != jcgateway.TriggerMention {
		t.Errorf("trigger is %q, want a mention", delivered[0].Trigger)
	}
	if delivered[0].Text != "summarise it" {
		t.Errorf("text is %q; the mention should have been stripped", delivered[0].Text)
	}
}

// In a private chat there is nobody else to be talking to.
func TestAPrivateChatNeedsNoMention(t *testing.T) {
	into := newCollector(1)
	one, _ := newTestAdapter(t, into,
		[]update{textMessage(555, 1, "private", "what time is it in Taipei?")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if delivered[0].Trigger != jcgateway.TriggerDirect {
		t.Errorf("trigger is %q, want a direct message", delivered[0].Trigger)
	}
	if delivered[0].Conversation.ChannelID != "555" {
		t.Errorf("channel is %q, want the chat id", delivered[0].Conversation.ChannelID)
	}
}

// A mention is a byte range Telegram marked, not a string that looks like one.
// Matching the text would answer somebody quoting a conversation about the bot.
func TestAnUnmarkedNameIsNotAMention(t *testing.T) {
	into := newCollector(1)
	one, _ := newTestAdapter(t, into,
		[]update{
			textMessage(-100, 1, "group", "I told @jingclaw_bot to stop replying to everything"),
			textMessage(-100, 2, "group", "@jingclaw_bot are you there",
				entity{Type: "mention", Offset: 0, Length: 13}),
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if len(delivered) != 1 {
		t.Fatalf("delivered %d messages; the unmarked name should not have counted", len(delivered))
	}
	if delivered[0].Text != "are you there" {
		t.Errorf("the wrong message got through: %q", delivered[0].Text)
	}
}

// Offsets are in UTF-16 code units, so a mention after non-ASCII text lands in
// the wrong place if the text is indexed by byte.
func TestAMentionAfterNonASCIITextIsFound(t *testing.T) {
	into := newCollector(1)
	text := "早安 @jingclaw_bot 幫我看一下"
	one, _ := newTestAdapter(t, into,
		[]update{textMessage(-100, 1, "group", text,
			entity{Type: "mention", Offset: 3, Length: 13})},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if delivered[0].Text != "早安 幫我看一下" {
		t.Errorf("text is %q, want the mention removed and the rest kept", delivered[0].Text)
	}
}

// Two automations talking each other into a loop is refused on every platform.
func TestABotIsIgnored(t *testing.T) {
	into := newCollector(1)
	fromBot := textMessage(555, 1, "private", "hello")
	fromBot.Message.From.IsBot = true

	one, _ := newTestAdapter(t, into,
		[]update{fromBot, textMessage(555, 2, "private", "hello from a person")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if len(delivered) != 1 || delivered[0].Text != "hello from a person" {
		t.Errorf("a bot's message was accepted: %+v", delivered)
	}
}

// The same message arriving twice must not become two runs. Telegram deletes
// an update once it is acknowledged, but a crash between the two loses that.
func TestOneMessageHasOneIdempotencyKey(t *testing.T) {
	into := newCollector(2)
	one, _ := newTestAdapter(t, into,
		[]update{textMessage(555, 1, "private", "first")},
		[]update{textMessage(555, 2, "private", "second")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if delivered[0].IdempotencyKey == delivered[1].IdempotencyKey {
		t.Errorf("two messages share the key %q", delivered[0].IdempotencyKey)
	}
	if delivered[0].IdempotencyKey != "555:1" {
		t.Errorf("key is %q, want the chat and message ids", delivered[0].IdempotencyKey)
	}
}

// An update is acknowledged whatever happens to it. Leaving one unacknowledged
// because it was refused would offer it again forever.
func TestARefusedUpdateIsStillAcknowledged(t *testing.T) {
	into := newCollector(1)
	one, backend := newTestAdapter(t, into,
		[]update{textMessage(-100, 17, "group", "not for the bot")},
		[]update{textMessage(555, 18, "private", "for the bot")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()
	into.await(t)

	polls := backend.sent("getUpdates")
	if len(polls) < 2 {
		t.Fatalf("only %d polls; the test proves nothing", len(polls))
	}
	if offset, _ := polls[1].Body["offset"].(float64); offset != 18 {
		t.Errorf("the second poll resumed at %v, want past the refused update", polls[1].Body["offset"])
	}
}

// An emoji is one rune and two UTF-16 code units, so a mention after one is
// where a rune-indexed reader stops finding it. This is the case that fails
// quietly: indexing by rune survives Chinese, which is what gets tested first.
func TestAMentionAfterAnEmojiIsFound(t *testing.T) {
	into := newCollector(1)
	one, _ := newTestAdapter(t, into,
		[]update{textMessage(-100, 1, "group", "👋 @jingclaw_bot hi",
			entity{Type: "mention", Offset: 3, Length: 13})},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if delivered[0].Text != "👋 hi" {
		t.Errorf("text is %q, want the mention removed and the rest kept", delivered[0].Text)
	}
}

// An entity claiming a span past the end of the text must not panic. Nothing
// says this cannot arrive, and a gateway that crashes on a malformed update is
// a gateway anybody can stop.
func TestAnImpossibleEntityIsIgnored(t *testing.T) {
	into := newCollector(1)
	one, _ := newTestAdapter(t, into,
		[]update{
			textMessage(-100, 1, "group", "short",
				entity{Type: "mention", Offset: 0, Length: 900}),
			textMessage(555, 2, "private", "still working"),
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = one.Run(ctx) }()

	delivered := into.await(t)
	if len(delivered) != 1 || delivered[0].Text != "still working" {
		t.Errorf("the impossible entity was not ignored: %+v", delivered)
	}
}
