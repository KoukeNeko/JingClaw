package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
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

	// A status line is the answer to "what is it doing now", and the previous
	// answer to that question is of no interest once it changes. Rewriting the
	// one this run already put in the channel keeps a run that touches ten
	// files from leaving ten lines behind it.
	if dispatch.Kind == jcgateway.DispatchStatus {
		return a.postStatus(channelID, dispatch, body)
	}

	// An answer still being written is one message that keeps growing. Posting
	// each version would be the same paragraph five times.
	if answer, streaming := answerInProgress(dispatch); streaming {
		return a.postPartialAnswer(channelID, answer, body)
	}

	segments := splitForDiscord(body)

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

// boundToOneMessage keeps a partial answer inside a single message.
func boundToOneMessage(body string) (string, bool) {
	if len(body) <= softMessageLength {
		return body, false
	}

	cut := breakPoint(body, softMessageLength-len(ellipsis))
	return body[:cut] + ellipsis, true
}

// postStatus keeps one status line per channel and rewrites it.
//
// Where it fails, it posts instead: a message somebody deleted, or one from
// before this process started, must not stop the run being able to say what it
// is doing.
func (a *Adapter) postStatus(
	channelID snowflake.ID,
	dispatch jcgateway.Dispatch,
	body string,
) ([]string, error) {
	// A run that has ended will not say anything else, so its line is released
	// here rather than being left for the next run to edit by accident.
	if isFinalStatus(dispatch.Payload) {
		defer a.clearStatus(dispatch.RunID)
	}

	if existing, ok := a.liveStatus(dispatch.RunID); ok {
		_, err := a.client.Rest.UpdateMessage(channelID, existing, discord.MessageUpdate{
			Content:         &body,
			AllowedMentions: &discord.AllowedMentions{},
		})
		if err == nil {
			// The same id as before, so the outbox keeps pointing at the
			// message that is actually in the channel.
			return []string{existing.String()}, nil
		}

		a.config.Logger.Debug("could not rewrite the status line, posting a new one",
			"run_id", string(dispatch.RunID), "error", err)
		a.clearStatus(dispatch.RunID)
	}

	message, err := a.client.Rest.CreateMessage(channelID, messageWith(body))
	if err != nil {
		return nil, fmt.Errorf("discord: post to %s: %w", channelID, err)
	}

	a.setStatus(dispatch.RunID, message.ID)
	return []string{message.ID.String()}, nil
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
	case "working":
		if payload.Detail == "" {
			return "_Working on it…_"
		}
		return fmt.Sprintf("_Working on it — `%s`_", payload.Detail)
	case "completed":
		headline := "_Done._"
		if payload.Detail != "" {
			headline = fmt.Sprintf("_Done in %s._", payload.Detail)
		}
		return headline + renderSummary(payload.Summary)
	case "cancelled":
		return "_Stopped._" + renderSummary(payload.Summary)
	case "failed":
		headline := "_That did not work._"
		if payload.Detail != "" {
			headline = fmt.Sprintf("_That did not work: %s_", payload.Detail)
		}
		return headline + renderSummary(payload.Summary)
	default:
		return ""
	}
}

// renderSummary accounts for a run that has ended.
//
// It is deliberately not a table. This sits under an answer somebody is
// reading, and the questions it exists to answer — what did it look at, what
// did that cost — are answered faster by three short lines than by a grid.
func renderSummary(summary *jcgateway.RunSummary) string {
	if summary == nil {
		return ""
	}

	// A status line does not go through the splitter, and Discord refuses an
	// oversized message outright — so an unbounded summary would take the
	// "Done" line down with it, which is worse than no summary at all.
	//
	// Naming the addresses is what usually grows, so that goes first. The
	// hard bound after it is not redundant: the tool list is as long as the
	// number of tools installed, and an MCP server may register many with
	// names nobody here chose.
	if rendered := joinSummary(summaryLines(summary, true)); len(rendered) <= maxSummaryLength {
		return rendered
	}
	return bound(joinSummary(summaryLines(summary, false)))
}

// bound cuts a summary that is still too long after the addresses have gone.
func bound(rendered string) string {
	if len(rendered) <= maxSummaryLength {
		return rendered
	}

	runes := []rune(rendered)
	if len(runes) > maxSummaryLength {
		runes = runes[:maxSummaryLength]
	}

	// Cut back to a line boundary, so the result does not end mid-figure and
	// read as a number it is not.
	trimmed := string(runes)
	if at := strings.LastIndex(trimmed, "\n-# "); at > 0 {
		trimmed = trimmed[:at]
	}
	return trimmed + "\n-# (this summary was too long to post in full)"
}

// maxSummaryLength leaves room for the headline the summary hangs off.
const maxSummaryLength = maxMessageLength - 200

func summaryLines(summary *jcgateway.RunSummary, listSources bool) []string {
	var lines []string
	if tools := renderTools(summary.Tools); tools != "" {
		lines = append(lines, tools)
	}
	if listSources {
		lines = append(lines, renderSources(summary)...)
	} else if counted := countSources(summary); counted != "" {
		lines = append(lines, counted)
	}
	if cost := renderCost(summary); cost != "" {
		lines = append(lines, cost)
	}
	if summary.Partial {
		// Said rather than hidden. A list that is quietly short reads as a
		// complete account of a run that did less than it did.
		lines = append(lines, "began before this gateway did, so the lists above are partial")
	}
	return lines
}

func joinSummary(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "\n-# " + strings.Join(lines, "\n-# ")
}

// countSources is what is said when naming them all will not fit.
func countSources(summary *jcgateway.RunSummary) string {
	total := len(summary.Sources) + summary.SourcesOmitted
	if total == 0 {
		return ""
	}

	var folded int
	for _, source := range summary.Sources {
		if !source.Retained {
			folded++
		}
	}
	if folded == 0 {
		return fmt.Sprintf("read %d sources, too many to name here", total)
	}
	return fmt.Sprintf("read %d sources, too many to name here; %d were folded into a summary before answering",
		total, folded)
}

func renderTools(tools []jcgateway.ToolUse) string {
	if len(tools) == 0 {
		return ""
	}

	parts := make([]string, 0, len(tools))
	for _, use := range tools {
		part := use.Name
		if use.Calls > 1 {
			part = fmt.Sprintf("%s ×%d", use.Name, use.Calls)
		}
		if use.Failed > 0 {
			part += fmt.Sprintf(" (%d failed)", use.Failed)
		}
		parts = append(parts, part)
	}
	return "used " + strings.Join(parts, ", ")
}

// renderSources says what a run drew on, and what it no longer had in front of
// it by the time it answered.
//
// The second group is not "sources it did not use". Nothing here can know
// that: material folded into a summary may well have shaped the answer through
// the summary. The claim made is only the one the log can support.
func renderSources(summary *jcgateway.RunSummary) []string {
	var retained, folded []string
	for _, source := range summary.Sources {
		if source.Retained {
			retained = append(retained, shortenRef(source.Ref))
		} else {
			folded = append(folded, shortenRef(source.Ref))
		}
	}

	var lines []string
	if len(retained) > 0 {
		lines = append(lines, "read "+strings.Join(retained, ", "))
	}
	if len(folded) > 0 {
		lines = append(lines,
			"read earlier, folded into a summary before answering: "+strings.Join(folded, ", "))
	}
	if summary.SourcesOmitted > 0 {
		lines = append(lines, fmt.Sprintf("and %d more not listed", summary.SourcesOmitted))
	}
	return lines
}

// shortenRef keeps an address identifiable without letting one of them fill
// the line. Tracking parameters routinely run to hundreds of characters and
// none of them help a reader recognise where something came from.
func shortenRef(ref string) string {
	const maxRefLength = 96

	runes := []rune(ref)
	if len(runes) <= maxRefLength {
		return ref
	}
	return string(runes[:maxRefLength]) + "…"
}

func renderCost(summary *jcgateway.RunSummary) string {
	if summary.InputTokens == 0 && summary.OutputTokens == 0 {
		// Zero means the provider reported nothing, not that nothing was
		// spent. Printing "0 tokens" would be stating a figure that is wrong.
		return ""
	}

	cost := fmt.Sprintf("%s in / %s out",
		formatTokens(summary.InputTokens), formatTokens(summary.OutputTokens))
	if summary.CachedInputTokens > 0 {
		cost += fmt.Sprintf(" (%s of the input was cached)", formatTokens(summary.CachedInputTokens))
	}
	return cost
}

// formatTokens keeps large counts readable. The exact figure matters to nobody
// reading a chat channel; the order of magnitude is the whole message.
func formatTokens(count int64) string {
	if count < 10000 {
		return fmt.Sprintf("%d", count)
	}
	return fmt.Sprintf("%.1fk", float64(count)/1000)
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
