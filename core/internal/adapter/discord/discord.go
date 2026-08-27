// Package discord adapts Discord to the gateway contract.
//
// Everything Discord-specific stops here. Above it the system sees only
// gateway.InboundMessage and gateway.Dispatch, so the ingress rules, the
// permission profile and the outbox are the same whatever platform the traffic
// came from.
//
// The connection is an outbound WebSocket rather than a webhook endpoint. A
// webhook needs a public URL and a TLS certificate, which for an agent running
// on somebody's laptop or on a machine behind NAT means a tunnel — and a tunnel
// is exactly the thing a local-first design should not require.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

const (
	// Discord refuses a message body longer than this.
	maxMessageLength = 2000

	// Segments are cut below the hard limit so a suffix such as a continuation
	// marker always fits.
	softMessageLength = 1900
)

// Sink receives messages the adapter has accepted.
type Sink interface {
	Deliver(ctx context.Context, message jcgateway.InboundMessage) error
}

type Config struct {
	// Token is the bot credential. Never logged.
	Token string

	// AccountID names this bot within JingClaw, so bindings and the delivery
	// outbox can be scoped to it. It is JingClaw's own name for the account,
	// not Discord's.
	AccountID string

	Logger *slog.Logger
}

// Adapter connects to Discord and translates in both directions.
type Adapter struct {
	config Config
	sink   Sink

	client *bot.Client

	// selfID is written by the Ready handler and read by the message handler,
	// which run on different goroutines.
	//
	// It is not read from the cache after connecting: the gateway returns
	// before READY has necessarily been processed, and a zero id means every
	// mention comparison fails and the bot silently ignores the channel it was
	// invited to.
	selfID atomic.Uint64
}

func New(config Config, sink Sink) *Adapter {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Adapter{config: config, sink: sink}
}

// Run connects and serves until the context is cancelled.
func (a *Adapter) Run(ctx context.Context) error {
	client, err := disgo.New(a.config.Token,
		// Message Content is a privileged intent that needs Discord's approval
		// once an app is at all popular. It is deliberately not requested: a
		// bot can still read direct messages and anything that mentions it,
		// which is precisely the traffic that counts as addressed to the agent.
		// Asking for more would mean reading every message in every channel to
		// find the few meant for us.
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuildMessages|gateway.IntentDirectMessages),
			gateway.WithAutoReconnect(true),
		),
		bot.WithEventListenerFunc(a.onReady),
		bot.WithEventListenerFunc(a.onMessage),
	)
	if err != nil {
		return fmt.Errorf("discord: create client: %w", err)
	}
	a.client = client
	defer client.Close(context.WithoutCancel(ctx))

	if err := client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("discord: open gateway: %w", err)
	}

	// Discord sends READY before anything else, but the handler for it runs
	// asynchronously. Waiting means the process does not report itself
	// connected while still unable to recognise its own name.
	if err := a.awaitIdentity(ctx); err != nil {
		return err
	}

	a.config.Logger.Info("connected to discord",
		"account_id", a.config.AccountID,
		"bot_user", a.self().String(),
	)

	<-ctx.Done()
	return ctx.Err()
}

// onReady records who this bot is.
func (a *Adapter) onReady(event *events.Ready) {
	a.selfID.Store(uint64(event.User.ID))
}

func (a *Adapter) self() snowflake.ID {
	return snowflake.ID(a.selfID.Load())
}

// awaitIdentity blocks until the bot knows its own id.
//
// Without it the adapter can start handling messages while every mention
// comparison is against zero, which looks exactly like a bot that was never
// invited: it connects, logs happily, and ignores everything.
func (a *Adapter) awaitIdentity(ctx context.Context) error {
	const (
		timeout = 30 * time.Second
		poll    = 50 * time.Millisecond
	)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a.self() != 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}

	return fmt.Errorf("discord: connected but never received a ready event within %s", timeout)
}

// onMessage turns a Discord message into an inbound message, or ignores it.
//
// The handler must return quickly. Doing the agent's work here would block the
// gateway's heartbeat, Discord would drop the connection, the library would
// reconnect and redeliver, and the same request would start over — an
// expensive loop that looks like the bot working very hard.
func (a *Adapter) onMessage(event *events.MessageCreate) {
	message := event.Message

	// A bot acting on another bot's messages is an unbounded loop. The ingress
	// refuses these too; stopping here saves a round trip.
	if message.Author.Bot || message.WebhookID != nil {
		return
	}
	if self := a.self(); self != 0 && message.Author.ID == self {
		return
	}

	trigger, addressed := a.triggerFor(message, event.GuildID)
	if !addressed {
		// Overheard text. Without the Message Content intent there is usually
		// nothing to read anyway, and even with it, being mentioned is what
		// makes something a request.
		return
	}

	inbound := a.toInbound(message, event.GuildID, trigger)

	// A short, independent deadline: the handler must not wait on the agent,
	// and this call only hands the work over.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := a.sink.Deliver(ctx, inbound); err != nil {
		a.config.Logger.Warn("could not hand a message to the agent",
			"channel_id", message.ChannelID.String(),
			"message_id", message.ID.String(),
			"error", err,
		)
	}
}

// triggerFor decides whether a message was addressed to the agent.
func (a *Adapter) triggerFor(message discord.Message, guildID *snowflake.ID) (jcgateway.Trigger, bool) {
	if guildID == nil {
		// A direct message is addressed to the bot by construction.
		return jcgateway.TriggerDirect, true
	}

	self := a.self()
	if self == 0 {
		// Nothing can be recognised as addressed to a bot that does not know
		// its own name. Saying so beats ignoring the channel in silence.
		a.config.Logger.Warn("ignoring a message: the bot identity is not known yet",
			"channel_id", message.ChannelID.String())
		return jcgateway.TriggerAmbient, false
	}

	for _, mentioned := range message.Mentions {
		if mentioned.ID == self {
			return jcgateway.TriggerMention, true
		}
	}

	return jcgateway.TriggerAmbient, false
}

func (a *Adapter) toInbound(
	message discord.Message,
	guildID *snowflake.ID,
	trigger jcgateway.Trigger,
) jcgateway.InboundMessage {
	tenant := ""
	if guildID != nil {
		tenant = guildID.String()
	}

	conversation := jcgateway.ConversationRef{
		Platform:  jcgateway.PlatformDiscord,
		AccountID: a.config.AccountID,
		TenantID:  tenant,
		ChannelID: message.ChannelID.String(),
	}

	// A thread is the natural unit of work, so it becomes the session. Outside
	// one, the message that started the exchange stands in as a stable key.
	if message.Thread != nil {
		conversation.ThreadID = message.Thread.ID().String()
	} else {
		conversation.RootMessageID = message.ID.String()
	}

	principal := jcgateway.Principal{
		Platform:    jcgateway.PlatformDiscord,
		AccountID:   a.config.AccountID,
		TenantID:    tenant,
		ID:          message.Author.ID.String(),
		DisplayName: message.Author.Username,
		IsBot:       message.Author.Bot,
	}

	// Roles travel as opaque claims. A Discord role and a GitHub collaborator
	// status are not the same kind of thing, and flattening them would invent
	// a hierarchy nobody meant.
	if message.Member != nil {
		for _, role := range message.Member.RoleIDs {
			principal.Claims = append(principal.Claims, jcgateway.Claim{
				Namespace: "discord.role",
				Value:     role.String(),
			})
		}
	}

	return jcgateway.InboundMessage{
		PlatformMessageID: message.ID.String(),
		// Discord message ids are unique, which is exactly what deduplication
		// needs: a redelivery after a reconnect carries the same one.
		IdempotencyKey: "discord:" + message.ID.String(),
		Principal:      principal,
		Conversation:   conversation,
		Text:           stripMention(message.Content, a.self()),
		Trigger:        trigger,
		OccurredAt:     message.CreatedAt,
	}
}

// stripMention removes the bot's own mention from the text.
//
// "@JingClaw fix the tests" is a request to fix the tests; leaving the mention
// in makes the model reason about a string of digits it has no use for.
func stripMention(content string, selfID snowflake.ID) string {
	if selfID == 0 {
		return strings.TrimSpace(content)
	}

	for _, form := range []string{
		fmt.Sprintf("<@%s>", selfID),
		// Discord writes a nickname mention differently.
		fmt.Sprintf("<@!%s>", selfID),
	} {
		content = strings.ReplaceAll(content, form, "")
	}

	return strings.TrimSpace(content)
}
