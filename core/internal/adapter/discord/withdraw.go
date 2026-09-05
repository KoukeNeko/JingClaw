package discord

import (
	"context"
	"time"

	"github.com/disgoorg/disgo/events"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// withdrawTimeout bounds handing a withdrawal over. Nothing is fetched for
// it, so it needs less time than a message.
const withdrawTimeout = 15 * time.Second

// onReaction hears a person press the waiting mark on a message.
//
// The mark is the bot's own: it put 📥 on the message to say it was in line.
// The person pressing the same mark is the person saying "then take it out".
// Anything else pressed is somebody reacting, which is none of this program's
// business.
func (a *Adapter) onReaction(event *events.MessageReactionAdd) {
	if event.Emoji.Reaction() != waitingMark {
		return
	}
	if self := a.self(); self != 0 && event.UserID == self {
		return
	}
	if event.Member != nil && event.Member.User.Bot {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), withdrawTimeout)
	defer cancel()

	withdrawal := jcgateway.Withdrawal{
		Principal:  a.reactionPrincipal(event),
		InboundKey: inboundKey(event.MessageID),
		MessageID:  event.MessageID.String(),
	}
	if err := a.sink.Withdraw(ctx, withdrawal); err != nil {
		a.config.Logger.Warn("could not hand a withdrawal to the agent",
			"channel_id", event.ChannelID.String(),
			"message_id", event.MessageID.String(),
			"error", err,
		)
	}
}

// reactionPrincipal is who pressed, as far as the reaction says.
//
// A reaction carries less than a message does — no display name outside a
// guild, no roles in a DM — and that is enough: whether this person may take
// the message back is settled by whether they sent it, which is the id alone.
func (a *Adapter) reactionPrincipal(event *events.MessageReactionAdd) jcgateway.Principal {
	principal := jcgateway.Principal{
		Platform:  jcgateway.PlatformDiscord,
		AccountID: a.config.AccountID,
		TenantID:  tenantOf(event.GuildID),
		ID:        event.UserID.String(),
	}
	if event.Member != nil {
		principal.DisplayName = event.Member.User.Username
		principal.IsBot = event.Member.User.Bot
	}
	return principal
}
