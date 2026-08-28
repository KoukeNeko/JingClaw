package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
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

// Post delivers one dispatch and returns the ids Discord gave the messages.
//
// The ids are returned rather than discarded because the outbox records them:
// editing or referring to what was posted later needs to know what it was.
func (a *Adapter) Post(ctx context.Context, dispatch jcgateway.Dispatch) ([]string, error) {
	channelID, err := snowflake.Parse(targetChannel(dispatch.Target))
	if err != nil {
		return nil, fmt.Errorf("discord: unusable channel in dispatch %s: %w", dispatch.ID, err)
	}

	body, err := renderDispatch(dispatch)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}

	segments := splitForDiscord(body)

	// Past a few messages, an answer stops being something to read in a
	// channel and starts being something to scroll past for the rest of the
	// day. A file is one line somebody can open if they want it.
	//
	// Only for the agent's own answers: an approval or a status line is short
	// by construction, and turning one into an attachment would hide the thing
	// somebody has to act on.
	if shouldSendAsFile(dispatch.Kind, len(segments), a.maxMessages()) {
		return a.postAsFile(channelID, dispatch, body)
	}

	var posted []string
	for _, segment := range segments {
		message, err := a.client.Rest.CreateMessage(channelID, messageWith(segment))
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
func (a *Adapter) postAsFile(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	content, truncated := boundAttachment(body, a.maxAttachmentBytes())

	lead := fmt.Sprintf("%s\n\n_%s_", opening(body, leadLength), describeFile(len(content), truncated))

	create := messageWith(lead)
	create.Files = []*discord.File{discord.NewFile(
		attachmentName(dispatch), "the whole answer", strings.NewReader(content))}

	message, err := a.client.Rest.CreateMessage(channelID, create)
	if err != nil {
		return nil, fmt.Errorf("discord: post a file to %s: %w", channelID, err)
	}

	return []string{message.ID.String()}, nil
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

// renderDispatch turns a dispatch into what a person will read.
func renderDispatch(dispatch jcgateway.Dispatch) (string, error) {
	switch dispatch.Kind {
	case jcgateway.DispatchMessage:
		var payload jcgateway.MessagePayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("discord: decode message payload: %w", err)
		}
		return payload.Text, nil

	case jcgateway.DispatchApproval:
		var payload jcgateway.ApprovalPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("discord: decode approval payload: %w", err)
		}
		return renderApproval(payload), nil

	case jcgateway.DispatchStatus:
		var payload jcgateway.StatusPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("discord: decode status payload: %w", err)
		}
		return renderStatus(payload), nil

	default:
		return "", fmt.Errorf("discord: unknown dispatch kind %q", dispatch.Kind)
	}
}

// renderApproval spells out what is being asked for.
//
// "The agent wants to use a tool" tells a reader nothing. What they need is
// the action, its target, and what will change — and, since approving from
// Discord is not enough on its own, where the decision is actually made.
func renderApproval(payload jcgateway.ApprovalPayload) string {
	var out strings.Builder

	out.WriteString("**Waiting for approval**\n")
	fmt.Fprintf(&out, "```\n%s\n```\n", payload.Summary)

	if len(payload.Effects) > 0 {
		out.WriteString("This will:\n")
		for _, effect := range payload.Effects {
			fmt.Fprintf(&out, "- %s\n", effect)
		}
	}

	// Approving from a chat message is deliberately not offered. A request and
	// its approval arriving from the same account is one unbroken chain, and
	// whoever holds that account holds both halves.
	fmt.Fprintf(&out, "\nApprove it from a JingClaw client:\n```\nagent approve %s\n```", payload.ApprovalID)

	return out.String()
}

func renderStatus(payload jcgateway.StatusPayload) string {
	switch payload.State {
	case "running":
		return "_Working on it…_"
	case "cancelled":
		return "_Stopped._"
	case "failed":
		if payload.Detail == "" {
			return "_That did not work._"
		}
		return fmt.Sprintf("_That did not work: %s_", payload.Detail)
	default:
		return ""
	}
}

// splitForDiscord cuts text into postable segments.
//
// Discord refuses anything over its limit outright, so long output has to be
// split rather than trimmed: silently dropping the tail of an answer is worse
// than posting it in two parts. Breaks are preferred at paragraph, then line,
// then character boundaries, and a code fence left open by a break is closed
// and reopened so neither half renders as prose.
func splitForDiscord(text string) []string {
	if len(text) <= maxMessageLength {
		return []string{text}
	}

	var (
		segments []string
		fence    string
	)

	remaining := text
	for len(remaining) > 0 {
		limit := softMessageLength - len(fence)*2
		if len(remaining) <= limit {
			segments = append(segments, fence+remaining)
			break
		}

		cut := breakPoint(remaining, limit)
		chunk := fence + remaining[:cut]
		remaining = strings.TrimLeft(remaining[cut:], "\n")

		// A fence opened in this chunk has to be closed here and reopened in
		// the next, or one half renders as code and the other as prose.
		if fence = openFence(chunk); fence != "" {
			chunk += "\n```"
			fence += "\n"
		}

		segments = append(segments, chunk)
	}

	return segments
}

// breakPoint finds the nicest place to cut within limit.
func breakPoint(text string, limit int) int {
	window := text[:limit]

	if index := strings.LastIndex(window, "\n\n"); index > limit/2 {
		return index
	}
	if index := strings.LastIndex(window, "\n"); index > limit/2 {
		return index
	}
	if index := strings.LastIndex(window, " "); index > limit/2 {
		return index
	}
	return limit
}

// openFence returns the opening fence still unclosed at the end of a chunk.
func openFence(chunk string) string {
	var (
		open     bool
		language string
	)

	for _, line := range strings.Split(chunk, "\n") {
		if !strings.HasPrefix(line, "```") {
			continue
		}
		if open {
			open, language = false, ""
			continue
		}
		open = true
		language = strings.TrimSpace(strings.TrimPrefix(line, "```"))
	}

	if !open {
		return ""
	}
	return "```" + language
}
