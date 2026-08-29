package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Platform is what a binding names to reach this adapter.
const Platform = jcgateway.Platform("telegram")

// Sink receives inbound messages, which is the only thing this adapter is
// allowed to do with them.
type Sink interface {
	Deliver(ctx context.Context, message jcgateway.InboundMessage) error
}

type Config struct {
	// Token is the bot credential. Never logged.
	Token string

	// AccountID names this bot within JingClaw, so bindings and the outbox can
	// be scoped to it. JingClaw's own name for the account, not Telegram's.
	AccountID string

	// APIBase is the Telegram API root. Configurable so a test can point it
	// somewhere it controls, which is the difference between checking this
	// adapter and checking the internet.
	APIBase string

	// MaxUploadBytes bounds what is uploaded. Zero uses the default, which is
	// well under what Telegram accepts.
	MaxUploadBytes int

	Logger *slog.Logger
}

// Adapter polls Telegram and posts back.
//
// Long polling rather than a webhook: a webhook needs a public address and a
// certificate, and the agent this serves runs on somebody's laptop behind a
// router. Polling costs a held connection and needs nothing.
type Adapter struct {
	config Config
	sink   Sink
	client *http.Client

	// username is learned at startup and read on every message. A mention is
	// matched against it, and an empty one would match nothing — the bot would
	// sit in a group silently ignoring everybody.
	username string

	// offset is the update id to resume from. Telegram deletes an update once
	// it has been acknowledged this way, which is what stops the same message
	// being answered twice across a restart.
	offset int64

	// live tracks the status message posted per run, so it can be edited in
	// place rather than accumulating one line per tool call.
	//
	// Keyed by run and not by chat: keyed by chat, a new run would edit the
	// line the previous one left behind, which by then is sitting at the
	// bottom of the previous answer.
	statusMu sync.Mutex
	live     map[string]int64
}

func New(config Config, sink Sink) *Adapter {
	if config.APIBase == "" {
		config.APIBase = "https://api.telegram.org"
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}

	return &Adapter{
		config: config,
		sink:   sink,
		// No overall timeout: a long poll is a request that is meant to wait.
		client: &http.Client{},
		live:   map[string]int64{},
	}
}

// Run polls until the context ends.
func (a *Adapter) Run(ctx context.Context) error {
	if err := a.learnSelf(ctx); err != nil {
		return err
	}

	a.config.Logger.Info("connected to telegram",
		"account_id", a.config.AccountID, "bot_user", a.username)

	const pollSeconds = 30

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		updates, err := a.poll(ctx, pollSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A poll that fails is ordinary — a dropped connection, a restart
			// at the other end. Ending the process over it would take the
			// agent down with the network.
			a.config.Logger.Warn("could not poll telegram", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		for _, one := range updates {
			// Acknowledged whatever happens to it. A message this refuses is
			// still handled, and leaving it unacknowledged would offer it
			// again forever.
			if one.UpdateID >= a.offset {
				a.offset = one.UpdateID + 1
			}
			a.handle(ctx, one)
		}
	}
}

func (a *Adapter) learnSelf(ctx context.Context) error {
	var me self
	if err := a.call(ctx, "getMe", nil, &me); err != nil {
		return fmt.Errorf("telegram: getMe: %w", err)
	}
	if me.Username == "" {
		return errors.New("telegram: the bot has no username, so a mention can never match")
	}
	a.username = me.Username
	return nil
}

func (a *Adapter) poll(ctx context.Context, seconds int) ([]update, error) {
	var updates []update
	body := map[string]any{
		"offset":          a.offset,
		"timeout":         seconds,
		"allowed_updates": []string{"message"},
	}

	// The request outlives the poll interval, so its own deadline is longer
	// than what the server was asked to wait.
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds+15)*time.Second)
	defer cancel()

	if err := a.call(pollCtx, "getUpdates", body, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// handle turns one update into work, or refuses it.
func (a *Adapter) handle(ctx context.Context, one update) {
	message := one.Message
	if message == nil || message.From == nil {
		return
	}

	// A bot triggering the agent is how two automations talk each other into
	// a loop, refused here as it is on every platform.
	if message.From.IsBot {
		return
	}

	trigger, ok := a.triggerFor(message)
	if !ok {
		return
	}

	inbound := jcgateway.InboundMessage{
		PlatformMessageID: strconv.FormatInt(message.MessageID, 10),
		// Telegram numbers updates per bot, and a message id per chat. The
		// pair is what is unique, and it is what stops a redelivery after a
		// restart being answered twice.
		IdempotencyKey: fmt.Sprintf("%d:%d", message.Chat.ID, message.MessageID),
		Principal: jcgateway.Principal{
			Platform:    Platform,
			AccountID:   a.config.AccountID,
			TenantID:    strconv.FormatInt(message.Chat.ID, 10),
			ID:          strconv.FormatInt(message.From.ID, 10),
			DisplayName: displayName(message.From),
			IsBot:       message.From.IsBot,
		},
		Conversation: a.conversationFor(message),
		Text:         a.strip(message),
		Trigger:      trigger,
		OccurredAt:   time.Unix(message.Date, 0).UTC(),
	}

	if err := a.sink.Deliver(ctx, inbound); err != nil {
		a.config.Logger.Error("could not deliver a telegram message",
			"chat_id", message.Chat.ID, "error", err)
	}
}

// triggerFor decides whether a message is a request.
//
// In a private chat everything is: there is nobody else to be talking to. In a
// group only a mention counts, because a channel the bot sits in is a
// conversation between people and acting on all of it would make the agent a
// participant nobody invited.
func (a *Adapter) triggerFor(message *message) (jcgateway.Trigger, bool) {
	if message.Chat.Type == "private" {
		return jcgateway.TriggerDirect, true
	}
	if a.mentioned(message) {
		return jcgateway.TriggerMention, true
	}
	return jcgateway.Trigger(""), false
}

// mentioned reports whether the bot was named.
//
// Read from the entities rather than by searching the text: the same @name is
// a mention only where Telegram marked one, and a bot that matched the string
// would answer somebody quoting a conversation about it.
func (a *Adapter) mentioned(message *message) bool {
	units := utf16.Encode([]rune(message.Text))
	for _, marked := range message.Entities {
		if named, ok := markedText(units, marked); ok && a.isSelf(named) {
			return true
		}
	}
	return false
}

func (a *Adapter) isSelf(named string) bool {
	return strings.EqualFold(named, "@"+a.username)
}

// markedText is the span an entity covers.
//
// Offsets are counted in UTF-16 code units, which is neither bytes nor runes.
// Indexed by byte, a mention after any non-ASCII text lands in the wrong
// place; indexed by rune it survives Chinese and fails on an emoji, which is
// the worse of the two because it looks correct in testing.
func markedText(units []uint16, marked entity) (string, bool) {
	if marked.Type != "mention" {
		return "", false
	}
	if marked.Offset < 0 || marked.Length < 0 || marked.Offset+marked.Length > len(units) {
		return "", false
	}
	return string(utf16.Decode(units[marked.Offset : marked.Offset+marked.Length])), true
}

// strip removes the mention that summoned the agent.
//
// What is left is the request. Leaving it in means the model reads its own
// name as part of the instruction, which it sometimes answers.
func (a *Adapter) strip(message *message) string {
	units := utf16.Encode([]rune(message.Text))

	for _, marked := range message.Entities {
		named, ok := markedText(units, marked)
		if !ok || !a.isSelf(named) {
			continue
		}
		remaining := append([]uint16{}, units[:marked.Offset]...)
		remaining = append(remaining, units[marked.Offset+marked.Length:]...)
		units = remaining
		break
	}

	// Collapsed rather than merely trimmed: removing a mention from the middle
	// of a sentence leaves two spaces where one word used to be.
	return strings.Join(strings.Fields(string(utf16.Decode(units))), " ")
}

// conversationFor names the conversation a message belongs to.
//
// A chat is the conversation. Telegram has no threads in the sense Discord
// does, and a reply is a link between two messages rather than a place —
// treating each reply chain as its own conversation would give the agent a
// new memory every time somebody answered it.
func (a *Adapter) conversationFor(message *message) jcgateway.ConversationRef {
	return jcgateway.ConversationRef{
		Platform:        Platform,
		AccountID:       a.config.AccountID,
		TenantID:        strconv.FormatInt(message.Chat.ID, 10),
		ChannelID:       strconv.FormatInt(message.Chat.ID, 10),
		SourceMessageID: strconv.FormatInt(message.MessageID, 10),
	}
}

func displayName(from *user) string {
	if from.Username != "" {
		return "@" + from.Username
	}
	return from.First
}

// call posts to one API method and decodes its answer.
func (a *Adapter) call(ctx context.Context, method string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}

	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return a.do(ctx, method, contentType, payload, out)
}

// do sends one request and decodes Telegram's envelope.
func (a *Adapter) do(ctx context.Context, method, contentType string, payload io.Reader, out any) error {
	url := fmt.Sprintf("%s/bot%s/%s", a.config.APIBase, a.config.Token, method)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, payload)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	httpResponse, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = httpResponse.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(httpResponse.Body, 8*1024*1024))
	if err != nil {
		return err
	}

	var envelope response[json.RawMessage]
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("telegram: %s answered unreadably: %w", method, err)
	}
	if !envelope.OK {
		// The token is in the URL, so an error must never carry the request.
		return &APIError{
			Method:      method,
			Code:        envelope.ErrorCode,
			Description: envelope.Description,
			RetryAfter:  retryAfterOf(envelope),
		}
	}

	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func retryAfterOf(envelope response[json.RawMessage]) time.Duration {
	if envelope.Parameters == nil || envelope.Parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(envelope.Parameters.RetryAfter) * time.Second
}

// APIError is a refusal from Telegram.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	base := fmt.Sprintf("telegram: %s refused (%d): %s", e.Method, e.Code, e.Description)
	if e.RetryAfter > 0 {
		base += fmt.Sprintf("; retry after %s", e.RetryAfter)
	}
	return base
}

// upload posts a multipart request, which is how a file reaches Telegram.
//
// Separate from call rather than folded into it: the JSON path is every other
// request here, and giving it a branch for a case it never takes would make
// the common one harder to read than the rare one.
func (a *Adapter) upload(
	ctx context.Context,
	method string,
	fields map[string]string,
	fileField, fileName string,
	content []byte,
	out any,
) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	for name, value := range fields {
		if err := form.WriteField(name, value); err != nil {
			return err
		}
	}
	part, err := form.CreateFormFile(fileField, fileName)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}

	return a.do(ctx, method, form.FormDataContentType(), &body, out)
}
