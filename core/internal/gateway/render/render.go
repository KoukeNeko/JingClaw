// Package render turns a dispatch into what a person will read.
//
// It lives outside the platform adapters because the wording is not a Discord
// decision. "That did not work" and the account of which tools a run used are
// the same sentences whoever is reading them; what differs between platforms
// is how long a message may be and how a subdued line is marked. Those are the
// two things Style carries.
//
// Before there was a second platform this was 600 lines inside the Discord
// adapter, and nothing said which of it was Discord's opinion and which was
// JingClaw's. Wanting the same summary on Telegram is what separated them.
package render

import (
	"encoding/json"
	"fmt"
	"strings"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Style is the little a platform gets to decide about presentation.
type Style struct {
	// MaxLength is the longest body the platform will accept.
	MaxLength int

	// SoftLength is where a split cuts, below the hard limit so that a
	// continuation marker or a reopened code fence still fits.
	SoftLength int

	// WorkingLine says whether this platform shows "what it is doing now" as
	// text. Discord does not: it puts a reaction on the message that asked,
	// which says the same thing without a line per tool call. Telegram's bots
	// may only react with a fixed set of emoji, so there it has to be text.
	WorkingLine bool

	// SubduedPrefix marks a line that is context rather than the answer —
	// Discord's small text, nothing at all where the platform has no such
	// thing.
	SubduedPrefix string

	// Bold and Italic wrap emphasis. Empty where the platform is being sent
	// plain text, because an unrendered "_like this_" is worse than no
	// emphasis: it is punctuation the reader has to ignore.
	Bold   string
	Italic string

	// Fence opens and closes a block of output meant to be read as output.
	// Empty where the platform would show the fence itself.
	Fence string

	// TableColumns is how wide a fixed-width table may be before it stops
	// being one. Zero means this platform shows no fixed-width text at all,
	// and every table becomes rows.
	//
	// A width rather than a "supports tables" flag, because no platform here
	// renders table syntax: the question is only whether a monospaced block
	// will line up, and that is a question about how much room there is.
	TableColumns int
}

func (s Style) bold(text string) string   { return s.Bold + text + s.Bold }
func (s Style) italic(text string) string { return s.Italic + text + s.Italic }

// block presents output as output — the alignment of a test failure is the
// information, so it is kept rather than reflowed.
func (s Style) block(text string) string {
	if s.Fence == "" {
		return text
	}
	return s.Fence + "\n" + text + "\n" + s.Fence
}

const ellipsis = "…"

// summaryMargin leaves room for the headline a summary hangs off.
const summaryMargin = 200

func (s Style) summaryLimit() int { return s.MaxLength - summaryMargin }

func (s Style) subdued(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "\n" + s.SubduedPrefix + strings.Join(lines, "\n"+s.SubduedPrefix)
}

// Dispatch turns one dispatch into what a person will read.
func Dispatch(dispatch jcgateway.Dispatch, style Style) (string, error) {
	switch dispatch.Kind {
	case jcgateway.DispatchMessage:
		var payload jcgateway.MessagePayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("render: could not decode message payload: %w", err)
		}
		return renderTables(NormalizeText(payload.Text), style), nil

	case jcgateway.DispatchQuestion:
		var payload jcgateway.QuestionPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("render: could not decode question payload: %w", err)
		}
		return renderQuestion(payload, style), nil

	case jcgateway.DispatchApproval:
		var payload jcgateway.ApprovalPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("render: could not decode approval payload: %w", err)
		}
		return renderApproval(payload, style), nil

	case jcgateway.DispatchLog:
		var payload jcgateway.LogPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("render: could not decode log payload: %w", err)
		}
		return renderLog(payload, style), nil

	case jcgateway.DispatchStatus:
		var payload jcgateway.StatusPayload
		if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
			return "", fmt.Errorf("render: could not decode status payload: %w", err)
		}
		return renderStatus(payload, style), nil

	default:
		return "", fmt.Errorf("render: unknown dispatch kind %q", dispatch.Kind)
	}
}

// NormalizeText undoes notation a model emits that no chat client renders.
//
// A model asked for prose still reaches for LaTeX when it reaches an arrow or
// a bold word, and every platform here shows that raw. Doing it once, above
// the adapters, is why the same answer reads the same everywhere.
func NormalizeText(text string) string {
	text = strings.NewReplacer(
		"$", "",
		`\(`, "", `\)`, "",
		`\[`, "", `\]`, "",
		`\%`, "%", `\_`, "_", `\#`, "#",
	).Replace(text)
	text = unwrapLatexCommand(text, "color", 2)
	for _, command := range []string{"text", "mathbf", "mathrm", "textbf", "textit", "underline", "boxed"} {
		text = unwrapLatexCommand(text, command, 1)
	}

	replacements := []string{
		`\rightarrow`, "→",
		`\leftarrow`, "←",
		`\leftrightarrow`, "↔",
		`\Rightarrow`, "⇒",
		`\Leftarrow`, "⇐",
		`\Leftrightarrow`, "⇔",
		`\to`, "→",
		`\times`, "×",
		`\neq`, "≠",
		`\leq`, "≤",
		`\geq`, "≥",
	}
	return strings.NewReplacer(replacements...).Replace(text)
}

func unwrapLatexCommand(text, command string, groupsToKeep int) string {
	prefix := "\\" + command
	for {
		start := strings.Index(text, prefix)
		if start < 0 || start+len(prefix) >= len(text) || text[start+len(prefix)] != '{' {
			return text
		}

		cursor := start + len(prefix)
		groups := make([]string, 0, groupsToKeep)
		end := cursor
		valid := true
		for groupIndex := 0; groupIndex < groupsToKeep; groupIndex++ {
			if end >= len(text) || text[end] != '{' {
				valid = false
				break
			}
			group, groupEnd, ok := latexGroup(text, end)
			if !ok {
				valid = false
				break
			}
			groups = append(groups, group)
			end = groupEnd
		}
		if !valid {
			return text
		}
		text = text[:start] + groups[groupsToKeep-1] + text[end:]
	}
}

func latexGroup(text string, start int) (string, int, bool) {
	depth := 0
	for index := start; index < len(text); index++ {
		switch text[index] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start+1 : index], index + 1, true
			}
		}
	}
	return "", 0, false
}

// renderApproval spells out what is being asked for.
//
// "The agent wants to use a tool" tells a reader nothing. What they need is
// the action, its target, and what will change — and, since approving from
// Discord is not enough on its own, where the decision is actually made.
func renderApproval(payload jcgateway.ApprovalPayload, style Style) string {
	var out strings.Builder

	out.WriteString(style.bold("Waiting for approval") + "\n")
	fmt.Fprintf(&out, "%s\n", style.block(payload.Summary))

	if len(payload.Effects) > 0 {
		out.WriteString("This will:\n")
		for _, effect := range payload.Effects {
			fmt.Fprintf(&out, "- %s\n", effect)
		}
	}

	switch payload.Route {
	case jcgateway.ApprovalByReply:
		// A console channel. Who can read and type here is settled by the
		// platform's own permissions, and the person reading this is the one
		// who owns it.
		fmt.Fprintf(&out, "\nReply `approve %s` or `deny %s`.",
			payload.ApprovalID, payload.ApprovalID)

	case jcgateway.ApprovalByPress:
		// A shared room with named approvers. The buttons are attached by the
		// platform adapter, so nothing is written here about how to press
		// them; what this line does is tell everyone else why nothing happens
		// when they try.
		out.WriteString("\nAn approver can decide this on the message itself.")

	default:
		// Typing is deliberately not offered. A request and its approval
		// arriving from the same account in a room other people can type in
		// is one unbroken chain, and whoever holds that account holds both
		// halves. A button is different: the platform says who pressed it.
		fmt.Fprintf(&out, "\nApprove it from a JingClaw client:\n%s",
			style.block("agent approve "+payload.ApprovalID))
	}

	return out.String()
}

// renderQuestion is the agent asking somebody something.
//
// The options are numbered as well as named: somebody answering from a phone
// types the shortest thing that works, and an id like "keep_compatible" is
// not it.
func renderQuestion(payload jcgateway.QuestionPayload, style Style) string {
	var out strings.Builder

	out.WriteString(style.bold("A question for you") + "\n")
	out.WriteString(payload.Prompt + "\n")

	for _, option := range payload.Options {
		fmt.Fprintf(&out, "- %s: %s", style.bold(option.ID), option.Label)
		if option.Detail != "" {
			fmt.Fprintf(&out, " — %s", option.Detail)
		}
		out.WriteString("\n")
	}

	if payload.AnswerableHere {
		// A console channel. Who can read and type here is settled by the
		// platform's own permissions, and the person reading this is the one
		// who owns it.
		fmt.Fprintf(&out, "\nReply `answer %s <your answer>`.", payload.QuestionID)
		return out.String()
	}

	// Everywhere else, deliberately not offered — for the same reason an
	// approval is not. A run steered from a room other people can type in is
	// steered by whoever is in the room.
	fmt.Fprintf(&out, "\nAnswer it from a JingClaw client:\n%s",
		style.block("agent answer "+payload.QuestionID+" <your answer>"))
	return out.String()
}

func renderStatus(payload jcgateway.StatusPayload, style Style) string {
	switch payload.State {
	case "running":
		return "👀"
	case "working":
		if !style.WorkingLine || payload.Detail == "" {
			return ""
		}
		return style.SubduedPrefix + "⋯ " + payload.Detail
	case "completed":
		return renderCompletionStatus(payload, style)
	case "cancelled":
		return style.italic("Stopped.") + renderSummary(payload.Summary, style)
	case "failed":
		headline := style.italic("That did not work.")
		if payload.Detail != "" {
			headline = style.italic("That did not work: " + payload.Detail)
		}
		return headline + renderSummary(payload.Summary, style)
	default:
		return ""
	}
}

func renderCompletionStatus(payload jcgateway.StatusPayload, style Style) string {
	if payload.Summary == nil {
		if payload.Detail == "" {
			return style.SubduedPrefix + style.italic("Done.")
		}
		return style.SubduedPrefix + "⏱ " + payload.Detail
	}

	parts := []string{}
	if duration := completionDuration(payload); duration != "" {
		parts = append(parts, "⏱ "+duration)
	}
	if usage := renderUsage(payload.Summary, payload.DurationMS); usage != "" {
		parts = append(parts, usage)
	}
	if model := renderModelPath(payload.Summary); model != "" {
		parts = append(parts, model)
	}
	if len(parts) == 0 {
		return ""
	}
	status := strings.Join(parts, " · ")

	// A run that produced no answer and asked for no tool is otherwise
	// indistinguishable from success: it completes, the tokens are spent, and
	// the line says how long it took. A model that thought and then said
	// nothing happens, and nobody should have to work out that nothing came.
	if payload.Summary.Silent {
		status = "⚠ returned nothing · " + status
	}

	if tools := renderToolList(payload.Summary.Tools, style); tools != "" {
		return tools + "\n" + style.SubduedPrefix + status
	}
	return style.SubduedPrefix + status
}

func completionDuration(payload jcgateway.StatusPayload) string {
	if payload.DurationMS > 0 {
		return formatDuration(payload.DurationMS)
	}
	return payload.Detail
}

func renderUsage(summary *jcgateway.RunSummary, durationMS int64) string {
	if summary.InputTokens == 0 && summary.OutputTokens == 0 {
		return ""
	}

	totalTokens := summary.InputTokens + summary.OutputTokens
	usage := fmt.Sprintf("↑%s ↓%s (%s)",
		formatStatusTokens(summary.InputTokens),
		formatStatusTokens(summary.OutputTokens),
		formatStatusTokens(totalTokens))
	if durationMS > 0 && summary.OutputTokens > 0 {
		rate := float64(summary.OutputTokens) * 1000 / float64(durationMS)
		usage += fmt.Sprintf(" · %.0f tok/s", rate)
	}
	return usage
}

func renderModelPath(summary *jcgateway.RunSummary) string {
	if summary.Provider == "" {
		return summary.Model
	}
	if summary.Model == "" {
		return summary.Provider
	}
	return summary.Provider + "/" + summary.Model
}

func renderToolList(tools []jcgateway.ToolUse, style Style) string {
	if len(tools) == 0 {
		return ""
	}

	lines := make([]string, 0, len(tools))
	for _, use := range tools {
		part := use.Name
		if use.Calls > 1 {
			part = fmt.Sprintf("%s ×%d", use.Name, use.Calls)
		}
		lines = append(lines, style.SubduedPrefix+"• "+part)
	}
	return strings.Join(lines, "\n")
}

// renderLog is one thing that happened, for a console channel.
//
// Subtext, because a log is context for the answer rather than the answer. A
// channel that shows twenty of these at the same weight as the reply has
// buried the reply.
func renderLog(payload jcgateway.LogPayload, style Style) string {
	var out strings.Builder

	mark := "·"
	if payload.IsError {
		mark = "✗"
	}
	fmt.Fprintf(&out, "%s%s `%s`", style.SubduedPrefix, mark, payload.Tool)

	if payload.DurationMS > 0 {
		fmt.Fprintf(&out, " %s", formatDuration(payload.DurationMS))
	}
	if summary := strings.Join(strings.Fields(payload.Summary), " "); summary != "" {
		const maxSummary = 160
		if len(summary) > maxSummary {
			summary = summary[:maxSummary] + "…"
		}
		fmt.Fprintf(&out, " — %s", summary)
	}
	if payload.Artifact != "" {
		// Named so it can be asked for, which is the only way the whole of it
		// reaches the channel.
		fmt.Fprintf(&out, "\n%s  stored as `%s`", style.SubduedPrefix, payload.Artifact)
	}

	// What it printed, in a code block rather than as subtext: this is output
	// meant to be read as output, and the alignment of a test failure is the
	// information.
	if output := strings.TrimSpace(payload.Output); output != "" {
		lead := ""
		if payload.OutputTruncated {
			lead = "…\n"
		}
		fmt.Fprintf(&out, "\n%s", style.block(lead+output))
	}

	return out.String()
}

// renderSummary accounts for a run that has ended.
//
// It is deliberately not a table. This sits under an answer somebody is
// reading, and the questions it exists to answer — what did it look at, what
// did that cost — are answered faster by three short lines than by a grid.
func renderSummary(summary *jcgateway.RunSummary, style Style) string {
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
	if rendered := style.subdued(summaryLines(summary, true)); len(rendered) <= style.summaryLimit() {
		return rendered
	}
	return bound(style.subdued(summaryLines(summary, false)), style)
}

// bound cuts a summary that is still too long after the addresses have gone.
func bound(rendered string, style Style) string {
	limit := style.summaryLimit()
	if len(rendered) <= limit {
		return rendered
	}

	runes := []rune(rendered)
	if len(runes) > limit {
		runes = runes[:limit]
	}

	// Cut back to a line boundary, so the result does not end mid-figure and
	// read as a number it is not.
	trimmed := string(runes)
	if at := strings.LastIndex(trimmed, "\n"+style.SubduedPrefix); at > 0 {
		trimmed = trimmed[:at]
	}
	return trimmed + "\n" + style.SubduedPrefix + "(this summary was too long to post in full)"
}

func summaryLines(summary *jcgateway.RunSummary, listSources bool) []string {
	var lines []string
	if who := renderModel(summary); who != "" {
		lines = append(lines, who)
	}
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

// renderModel says who answered, because that changes and the answers change
// with it.
func renderModel(summary *jcgateway.RunSummary) string {
	switch {
	case summary.Provider == "" && summary.Model == "":
		return ""
	case summary.Model == "":
		return summary.Provider
	case summary.Provider == "":
		return summary.Model
	default:
		return summary.Provider + " · " + summary.Model
	}
}

func formatStatusTokens(count int64) string {
	if count < 10000 {
		return fmt.Sprintf("%d", count)
	}
	return fmt.Sprintf("%.1fK", float64(count)/1000)
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
		if use.Milliseconds > 0 {
			part += " " + formatDuration(use.Milliseconds)
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

// formatDuration keeps a figure readable at the scale it happens to be.
//
// A tool call is anything from a millisecond to minutes, and one format cannot
// carry that range without either losing the fast ones or padding the slow
// ones with digits nobody reads.
func formatDuration(milliseconds int64) string {
	switch {
	case milliseconds < 1000:
		return fmt.Sprintf("%dms", milliseconds)
	case milliseconds < 60_000:
		return fmt.Sprintf("%.1fs", float64(milliseconds)/1000)
	default:
		minutes := milliseconds / 60_000
		seconds := (milliseconds % 60_000) / 1000
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
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

// Split cuts text into postable segments.
//
// Every platform here refuses an oversized message outright, so long output
// has to be split rather than trimmed: silently dropping the tail of an answer
// is worse than posting it in two parts. Breaks are preferred at paragraph, then line,
// then character boundaries, and a code fence left open by a break is closed
// and reopened so neither half renders as prose.
func Split(text string, style Style) []string {
	if len(text) <= style.MaxLength {
		return []string{text}
	}

	var (
		segments []string
		fence    string
	)

	remaining := text
	for len(remaining) > 0 {
		limit := style.SoftLength - len(fence)*2
		if len(remaining) <= limit {
			segments = append(segments, fence+remaining)
			break
		}

		cut := BreakPoint(remaining, limit)
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

// BreakPoint finds the nicest place to cut within limit.
func BreakPoint(text string, limit int) int {
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
