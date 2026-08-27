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

	var posted []string
	for _, segment := range splitForDiscord(body) {
		create := discord.MessageCreate{
			Content: segment,
			// Model output is not a licence to notify a room. Without this a
			// reply quoting "@everyone" from a file would ring everybody's
			// phone.
			AllowedMentions: &discord.AllowedMentions{},
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
