package discord

import (
	"context"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// messageSource is the two questions asked of the platform about messages
// other than the one that arrived: what a reply points at, and what came just
// before it. Narrow so a test can answer them without a Discord connection.
type messageSource interface {
	GetMessage(channelID, messageID snowflake.ID, opts ...rest.RequestOpt) (*discord.Message, error)
	GetMessages(channelID, around, before, after snowflake.ID, limit int, opts ...rest.RequestOpt) ([]discord.Message, error)
}

const (
	// contextWindow is how many earlier messages are looked at for pictures.
	// A person posts a screenshot and then the question about it as a second
	// message; they do not post ten.
	contextWindow = 10

	// contextAge is how far back an earlier picture still counts as part of
	// this request rather than of some previous conversation.
	contextAge = 10 * time.Minute
)

// attachmentsFor decides which files belong to a request.
//
// The message that arrived is not always the one the picture is on. A person
// posts a screenshot and then, as a second message, asks about it; or they
// reply to something with a picture in it and ask about that. Fetching only
// what came on the message with the mention meant the agent answered "I
// cannot see any image" to somebody looking straight at one, and the
// question of why was unanswerable from any log.
//
// What is taken, and what is deliberately not:
//
//   - The message being replied to, whoever wrote it. Replying to a picture
//     and asking about it is the request.
//   - Earlier messages by the same person that did not address the agent —
//     those were never delivered, so nothing is being repeated — going back
//     ten minutes or until the agent's own last reply, whichever comes first.
//     The reply is the boundary: what came before it was the previous
//     conversation, and a picture from before it is not what "this" means.
//   - Nobody else's earlier messages. Somebody's picture in a busy channel is
//     not part of a stranger's request just because it was near it.
//   - Only the files. The words on an unaddressed message are overheard, and
//     overheard text is not a request; that rule is not changed here.
//
// Nothing is downloaded here. This says which; collectAttachments fetches,
// and its bound on how many applies to the whole request.
func (a *Adapter) attachmentsFor(
	ctx context.Context,
	message discord.Message,
	guildID *snowflake.ID,
) []discord.Attachment {
	own := len(message.Attachments)
	gathered := append([]discord.Attachment(nil), message.Attachments...)
	seen := make(map[snowflake.ID]bool, own)
	for _, attachment := range message.Attachments {
		seen[attachment.ID] = true
	}

	source := a.source()
	if source == nil {
		return gathered
	}

	fromReply := 0
	if referenced := a.repliedTo(ctx, source, message); referenced != nil {
		for _, attachment := range referenced.Attachments {
			if !seen[attachment.ID] {
				seen[attachment.ID] = true
				gathered = append(gathered, attachment)
				fromReply++
			}
		}
	}

	fromEarlier := 0
	for _, earlier := range a.earlierBy(ctx, source, message, guildID) {
		for _, attachment := range earlier.Attachments {
			if !seen[attachment.ID] {
				seen[attachment.ID] = true
				gathered = append(gathered, attachment)
				fromEarlier++
			}
		}
	}

	if fromReply > 0 || fromEarlier > 0 {
		// Said, because a file that arrives from a message other than the one
		// somebody sent is the kind of thing they will ask about.
		a.config.Logger.Info("took files from other messages",
			"own", own, "from_reply", fromReply, "from_earlier", fromEarlier)
	}
	return gathered
}

// source is where messages are read back from: a test's stand-in if one was
// given, else the connection, else nothing — before the connection exists
// there is nothing to ask.
func (a *Adapter) source() messageSource {
	if a.messages != nil {
		return a.messages
	}
	if a.client != nil && a.client.Rest != nil {
		return a.client.Rest
	}
	return nil
}

// repliedTo fetches the message a reply points at, or nothing.
func (a *Adapter) repliedTo(
	ctx context.Context,
	source messageSource,
	message discord.Message,
) *discord.Message {
	reference := message.MessageReference
	if reference == nil || reference.MessageID == nil {
		return nil
	}

	channel := message.ChannelID
	if reference.ChannelID != nil {
		channel = *reference.ChannelID
	}

	referenced, err := source.GetMessage(channel, *reference.MessageID, rest.WithCtx(ctx))
	if err != nil {
		// A reply to something deleted, or to a message this bot cannot
		// read. The request still stands without it.
		a.config.Logger.Warn("could not read the message this replies to",
			"message_id", reference.MessageID.String(), "error", err)
		return nil
	}
	return referenced
}

// earlierBy finds the sender's own recent messages that were never delivered.
func (a *Adapter) earlierBy(
	ctx context.Context,
	source messageSource,
	message discord.Message,
	guildID *snowflake.ID,
) []discord.Message {
	recent, err := source.GetMessages(message.ChannelID, 0, message.ID, 0, contextWindow, rest.WithCtx(ctx))
	if err != nil {
		a.config.Logger.Warn("could not read what came before a message",
			"channel_id", message.ChannelID.String(), "error", err)
		return nil
	}

	self := a.self()
	cutoff := message.CreatedAt.Add(-contextAge)

	// Newest first, which is the order the platform answers in and the order
	// that lets the agent's own reply act as a stop.
	var earlier []discord.Message
	for _, candidate := range recent {
		if self != 0 && candidate.Author.ID == self {
			break
		}
		if candidate.Author.ID != message.Author.ID {
			continue
		}
		if candidate.CreatedAt.Before(cutoff) {
			break
		}
		if _, addressed := a.triggerFor(candidate, guildID); addressed {
			// Delivered when it arrived; its files came with it then.
			continue
		}
		if len(candidate.Attachments) == 0 {
			continue
		}
		earlier = append(earlier, candidate)
	}
	return earlier
}
