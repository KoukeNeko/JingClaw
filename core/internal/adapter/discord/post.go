package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

const (
	// defaultMaxMessages is where an answer stops being something to read in a
	// channel. Three is about as much as anybody scrolls past willingly.
	defaultMaxMessages = 3

	// defaultMaxAttachmentBytes is well under what Discord accepts, because
	// the point is to be readable rather than to be the largest possible file.
	defaultMaxAttachmentBytes = 4 << 20

	// leadLength is how much of the answer goes in the message itself. Enough
	// to tell whether the file is worth opening.
	leadLength = 700

	ellipsis = "…"
)

// discordStyle is everything about presentation Discord gets to decide.
//
// "-# " is small text, which is what a run summary and a tool line should be:
// context under an answer rather than beside it at the same weight.
var discordStyle = render.Style{
	MaxLength:     maxMessageLength,
	SoftLength:    softMessageLength,
	SubduedPrefix: "-# ",
	Bold:          "**",
	Italic:        "_",
	Fence:         "```",
}

// Post delivers one dispatch and returns the ids Discord gave the messages.
//
// The ids are returned rather than discarded because the outbox records them:
// editing or referring to what was posted later needs to know what it was.
func (a *Adapter) Post(ctx context.Context, dispatch jcgateway.Dispatch) ([]string, error) {
	channelID, err := snowflake.Parse(targetChannel(dispatch.Target))
	if err != nil {
		return nil, fmt.Errorf("discord: unusable channel in dispatch %s: %w", dispatch.ID, err)
	}

	body, err := render.Dispatch(dispatch, discordStyle)
	if err != nil {
		return nil, err
	}
	if dispatch.Kind == jcgateway.DispatchStatus {
		return a.postReactionStatus(channelID, dispatch, body)
	}
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}

	// A status line is the answer to "what is it doing now", and the previous
	// answer to that question is of no interest once it changes. Rewriting the
	// one this run already put in the channel keeps a run that touches ten
	// files from leaving ten lines behind it.
	// A log line accumulates rather than replacing what came before it, which
	// is the whole difference between it and a status line.
	if dispatch.Kind == jcgateway.DispatchLog {
		// Split like any other message. A log line carries whatever a command
		// printed, and the platform's limit is counted in characters while
		// the bound on the output is in runes — one line of CJK output is
		// three times the bytes of the same length in English.
		return a.finishAsMessages(channelID, dispatch, render.Split(body, discordStyle))
	}

	// Something asked for by name, handed over as a file rather than pasted
	// into the channel.
	if file, carried := attachedFile(dispatch); carried {
		return a.postFile(channelID, file)
	}

	// An answer still being written is one message that keeps growing. Posting
	// each version would be the same paragraph five times.
	if answer, streaming := answerInProgress(dispatch); streaming {
		return a.postPartialAnswer(channelID, answer, body)
	}

	segments := render.Split(body, discordStyle)

	// Past a few messages, an answer stops being something to read in a
	// channel and starts being something to scroll past for the rest of the
	// day. A file is one line somebody can open if they want it.
	//
	// Only for the agent's own answers. An approval is short, and a status
	// line is bounded where it is rendered; turning either into an attachment
	// would hide the thing somebody has to act on.
	if shouldSendAsFile(dispatch.Kind, len(segments), a.maxMessages()) {
		return a.finishAsFile(channelID, dispatch, body)
	}

	return a.finishAsMessages(channelID, dispatch, segments)
}

func (a *Adapter) postReactionStatus(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	var payload jcgateway.StatusPayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return nil, fmt.Errorf("discord: decode reaction status: %w", err)
	}

	emoji, remove := reactionForStatus(payload.State)
	if emoji == "" && !remove {
		return nil, nil
	}
	conversation := dispatch.Target
	if conversation.SourceMessageID == "" {
		return nil, nil
	}
	messageID, err := snowflake.Parse(conversation.SourceMessageID)
	if err != nil {
		return nil, fmt.Errorf("discord: unusable source message in dispatch %s: %w", dispatch.ID, err)
	}
	if remove {
		if err := a.client.Rest.RemoveOwnReaction(channelID, messageID, emoji); err != nil {
			a.config.Logger.Warn("could not remove status reaction",
				"run_id", string(dispatch.RunID), "message_id", conversation.SourceMessageID,
				"emoji", emoji, "error", err)
		}
	} else if err := a.client.Rest.AddReaction(channelID, messageID, emoji); err != nil {
		a.config.Logger.Warn("could not add status reaction",
			"run_id", string(dispatch.RunID), "message_id", conversation.SourceMessageID,
			"emoji", emoji, "error", err)
	}
	if !isFinalStatus(dispatch.Payload) || strings.TrimSpace(body) == "" {
		return nil, nil
	}

	message, err := a.client.Rest.CreateMessage(channelID, messageWith(body))
	if err != nil {
		return nil, fmt.Errorf("discord: post final status to %s: %w", channelID, err)
	}
	return []string{message.ID.String()}, nil
}

func reactionForStatus(state string) (string, bool) {
	switch state {
	case "network_started":
		return "🌍", false
	case "network_finished":
		return "🌍", true
	case "memory_started":
		return "📓", false
	case "memory_finished":
		return "", false
	case "provider_started":
		return "🧠", false
	case "completed":
		return "✅", false
	case "failed":
		// Not a tick. A reader glances at the reaction and reads the text only
		// if something looks worth reading, so a run that failed under a ✅ is
		// one nobody finds out about.
		return "❌", false
	case "cancelled":
		return "🛑", false
	default:
		return "", false
	}
}

// answerInProgress reports the answer a dispatch is a version of, when it is
// not the last one.
func answerInProgress(dispatch jcgateway.Dispatch) (string, bool) {
	if dispatch.Kind != jcgateway.DispatchMessage {
		return "", false
	}

	var payload jcgateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return "", false
	}
	return payload.MessageID, payload.MessageID != "" && !payload.Final
}

// answerOf reports which answer a finished dispatch completes, if any.
func answerOf(dispatch jcgateway.Dispatch) string {
	var payload jcgateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return ""
	}
	return payload.MessageID
}

// postPartialAnswer keeps one message growing while the model writes.
//
// Only ever one, and only as much of it as a single message holds. An answer
// that outgrows that stops being rewritten and waits for the final version,
// which is where the decision between several messages and a file belongs.
func (a *Adapter) postPartialAnswer(
	channelID snowflake.ID,
	answer string,
	body string,
) ([]string, error) {
	shown, cut := boundToOneMessage(body)

	if existing, ok := a.liveAnswer(answer); ok {
		if cut {
			// Already at the limit; there is nothing new to show until the
			// answer is whole.
			return []string{existing.String()}, nil
		}

		_, err := a.client.Rest.UpdateMessage(channelID, existing, discord.MessageUpdate{
			Content:         &shown,
			AllowedMentions: &discord.AllowedMentions{},
		})
		if err == nil {
			return []string{existing.String()}, nil
		}

		a.config.Logger.Debug("could not extend the answer, posting a new message",
			"message_id", answer, "error", err)
		a.clearAnswer(answer)
	}

	message, err := a.client.Rest.CreateMessage(channelID, messageWith(shown))
	if err != nil {
		return nil, fmt.Errorf("discord: post to %s: %w", channelID, err)
	}

	a.setAnswer(answer, message.ID)
	return []string{message.ID.String()}, nil
}

// finishAsMessages posts the finished answer, extending the message it has
// been growing in if there is one.
func (a *Adapter) finishAsMessages(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	segments []string,
) ([]string, error) {
	var posted []string

	answer := answerOf(dispatch)
	defer a.clearAnswer(answer)

	for index, segment := range segments {
		if index == 0 {
			if existing, ok := a.liveAnswer(answer); ok {
				_, err := a.client.Rest.UpdateMessage(channelID, existing, discord.MessageUpdate{
					Content:         &segments[0],
					AllowedMentions: &discord.AllowedMentions{},
				})
				if err == nil {
					posted = append(posted, existing.String())
					continue
				}
				a.config.Logger.Debug("could not finish the answer in place",
					"message_id", answer, "error", err)
				a.clearAnswer(answer)
			}
		}

		// Controls go on the last message of an approval and nowhere else.
		// An approval that split into several would otherwise carry a set of
		// buttons above the effects they are agreeing to.
		create := messageWith(segment)
		if index == len(segments)-1 {
			if approvalID, wanted := approvalIDOf(dispatch); wanted && a.decider != nil {
				create.Components = []discord.LayoutComponent{approvalButtons(approvalID)}
			}
		}

		message, err := a.client.Rest.CreateMessage(channelID, create)
		if err != nil {
			// Whatever was already posted is reported, so a caller retrying
			// knows the delivery was partial rather than assuming none of it
			// landed.
			return posted, fmt.Errorf("discord: post to %s: %w", channelID, err)
		}
		posted = append(posted, message.ID.String())
	}

	return posted, nil
}

// boundToOneMessage keeps a partial answer inside a single message.
func boundToOneMessage(body string) (string, bool) {
	if len(body) <= softMessageLength {
		return body, false
	}

	cut := render.BreakPoint(body, softMessageLength-len(ellipsis))
	return body[:cut] + ellipsis, true
}

// isFinalStatus reports whether a run has said the last thing it will say.
func isFinalStatus(payload string) bool {
	var status jcgateway.StatusPayload
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		// Unreadable, so treated as final: releasing a line that had more to
		// say costs one extra message, and holding one that did not costs the
		// next run editing it.
		return true
	}

	switch status.State {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// The live status message is held in memory rather than in the outbox because
// it is a presentation detail: losing it across a restart costs one extra line
// in a channel, not a wrong one, and the log stays the only thing that has to
// be true.
func (a *Adapter) liveStatus(run domain.RunID) (snowflake.ID, bool) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	id, ok := a.statusMessages[run]
	return id, ok
}

func (a *Adapter) setStatus(run domain.RunID, message snowflake.ID) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	a.statusMessages[run] = message
}

func (a *Adapter) clearStatus(run domain.RunID) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	delete(a.statusMessages, run)
}

// shouldSendAsFile decides between a wall of messages and one attachment.
//
// Only the agent's own answers are eligible. An approval is the thing somebody
// has to act on and a status line is short by construction; turning either
// into a file would hide it behind a download.
func shouldSendAsFile(kind jcgateway.DispatchKind, segments, maxMessages int) bool {
	return kind == jcgateway.DispatchMessage && segments > maxMessages
}

// postAsFile hands over a long answer as an attachment.
//
// The lead line carries the opening of the answer, because a bare "see
// attached" tells a person nothing about whether they need to open it.
// attachedFile reports a file the dispatch carries, if it carries one.
func attachedFile(dispatch jcgateway.Dispatch) (jcgateway.MessageFile, bool) {
	if dispatch.Kind != jcgateway.DispatchMessage {
		return jcgateway.MessageFile{}, false
	}

	var payload jcgateway.MessagePayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return jcgateway.MessageFile{}, false
	}
	if payload.File == nil || len(payload.File.Content) == 0 {
		return jcgateway.MessageFile{}, false
	}
	return *payload.File, true
}

// postFile uploads what somebody asked for.
func (a *Adapter) postFile(channelID snowflake.ID, file jcgateway.MessageFile) ([]string, error) {
	content := file.Content
	truncated := false
	if limit := a.maxAttachmentBytes(); len(content) > limit {
		content = content[:limit]
		truncated = true
	}

	name := file.Name
	if name == "" {
		name = "artifact.txt"
	}

	lead := fmt.Sprintf("_%s_", describeFile(len(content), truncated))
	create := messageWith(lead)
	create.Files = []*discord.File{discord.NewFile(name, "", bytes.NewReader(content))}

	message, err := a.client.Rest.CreateMessage(channelID, create)
	if err != nil {
		return nil, fmt.Errorf("discord: post a file to %s: %w", channelID, err)
	}
	return []string{message.ID.String()}, nil
}

func (a *Adapter) finishAsFile(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	content, truncated := boundAttachment(body, a.maxAttachmentBytes())

	lead := fmt.Sprintf("%s\n\n_%s_", opening(body, leadLength), describeFile(len(content), truncated))

	create := messageWith(lead)
	create.Files = []*discord.File{discord.NewFile(
		attachmentName(dispatch), "the whole answer", strings.NewReader(content))}

	// An answer that grew past what one message holds may already have a
	// message of its own. It is released rather than edited: a file cannot be
	// added to a message that was posted without one, and the lead belongs
	// with the file.
	a.clearAnswer(answerOf(dispatch))

	message, err := a.client.Rest.CreateMessage(channelID, create)
	if err != nil {
		return nil, fmt.Errorf("discord: post a file to %s: %w", channelID, err)
	}

	return []string{message.ID.String()}, nil
}

func (a *Adapter) liveAnswer(answer string) (snowflake.ID, bool) {
	if answer == "" {
		return 0, false
	}

	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	id, ok := a.answerMessages[answer]
	return id, ok
}

func (a *Adapter) setAnswer(answer string, message snowflake.ID) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	a.answerMessages[answer] = message
}

func (a *Adapter) clearAnswer(answer string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()

	delete(a.answerMessages, answer)
}

// messageWith builds a post that cannot notify a room.
//
// Model output is not a licence to ring everybody's phone: without this, a
// reply quoting "@everyone" out of a file it just read would do exactly that.
func messageWith(content string) discord.MessageCreate {
	return discord.MessageCreate{
		Content:         content,
		AllowedMentions: &discord.AllowedMentions{},
	}
}

func (a *Adapter) maxMessages() int {
	if a.config.MaxMessages > 0 {
		return a.config.MaxMessages
	}
	return defaultMaxMessages
}

func (a *Adapter) maxAttachmentBytes() int {
	if a.config.MaxAttachmentBytes > 0 {
		return a.config.MaxAttachmentBytes
	}
	return defaultMaxAttachmentBytes
}

// attachmentName ties the file to the run it came from, so several in a
// channel can be told apart.
func attachmentName(dispatch jcgateway.Dispatch) string {
	suffix := string(dispatch.RunID)
	if index := strings.LastIndex(suffix, "_"); index >= 0 {
		suffix = suffix[index+1:]
	}
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	if suffix == "" {
		suffix = "answer"
	}

	// .txt rather than .md: Discord previews one and offers the other as a
	// download, and being able to read it without downloading is the point.
	return "jingclaw-" + strings.ToLower(suffix) + ".txt"
}

// opening is the first part of an answer, cut at a line so the lead does not
// end mid-word.
func opening(body string, limit int) string {
	if len(body) <= limit {
		return body
	}

	window := body[:limit]
	if index := strings.LastIndex(window, "\n"); index > limit/2 {
		window = window[:index]
	}
	return strings.TrimRight(window, " \n") + ellipsis
}

func describeFile(size int, truncated bool) string {
	if truncated {
		return fmt.Sprintf("the answer was too long to send whole; the first %s is attached",
			formatBytes(size))
	}
	return fmt.Sprintf("the whole answer is attached (%s)", formatBytes(size))
}

// boundAttachment keeps an upload inside what the platform will take.
func boundAttachment(body string, limit int) (string, bool) {
	if len(body) <= limit {
		return body, false
	}
	return body[:limit], true
}

func formatBytes(size int) string {
	switch {
	case size < 1024:
		return fmt.Sprintf("%d bytes", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

// targetChannel picks where to post: a thread when there is one, otherwise the
// channel the exchange started in.
func targetChannel(target jcgateway.ConversationRef) string {
	if target.ThreadID != "" {
		return target.ThreadID
	}
	return target.ChannelID
}
