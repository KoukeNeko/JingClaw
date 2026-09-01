// Package console turns what the agent is doing into something readable in a
// terminal that scrolls.
//
// The shape is a game server's console: lines are appended and never redrawn,
// and the newest is at the bottom. A line that has been printed is part of the
// terminal's scrollback, so an event that later changes state does not go back
// and edit its line — it prints another one. This is a record of what
// happened, not a display of what is true now.
package console

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Fields, in the order they are written.
//
// The order is fixed and the preview is last, because a preview is the only
// field that can be arbitrarily long. Filling a line with the payload and then
// cutting from the right is what loses the session and the state — the two
// fields that say whether the line is even about something you care about.
//
//	15:04:05  #session  KIND state  metadata · preview
const (
	timeFormat     = "15:04:05"
	sessionDigits  = 8
	artifactDigits = 12
	fieldGap       = "  "
)

// previewWidth is how much of a payload one line shows before the rest is left
// for the reader to ask about.
//
// Lines wrap rather than being cut to the terminal's width — a console does
// wrap, and a message cut at the edge of a window is a message whose ending
// depends on how wide somebody's terminal happens to be. This is a limit on
// how much of a payload is worth putting in the running log at all.
const previewWidth = 72

// Line is one event, ready to print.
type Line struct {
	At      time.Time
	Session domain.SessionID
	Kind    string
	State   string
	Meta    string
	Preview string
}

// String writes the fields in their fixed order.
func (l Line) String() string {
	var out strings.Builder
	out.WriteString(l.At.Format(timeFormat))
	out.WriteString(fieldGap)
	out.WriteString(shortSession(l.Session))
	out.WriteString(fieldGap)
	out.WriteString(l.Kind)

	if l.State != "" {
		out.WriteString(" ")
		out.WriteString(l.State)
	}
	if l.Meta != "" {
		out.WriteString(fieldGap)
		out.WriteString(l.Meta)
	}
	if l.Preview != "" {
		out.WriteString(fieldGap)
		out.WriteString(clip(l.Preview, previewWidth))
	}
	return out.String()
}

// shortSession is how a session is named in a log that many of them share.
//
// Prefixed, because a bare identifier in a column of them is hard to tell
// apart from a timestamp or a tool name at a glance.
func shortSession(id domain.SessionID) string {
	text := string(id)
	if len(text) > sessionDigits {
		text = text[:sessionDigits]
	}
	return "#" + text
}

// shortArtifact is an artifact id at the length a log line can carry.
//
// Short enough to sit on a line beside everything else, long enough that two
// of them in one session are not the same string. `open` takes a prefix, so
// what is printed is what can be typed.
func shortArtifact(id string) string {
	text := string(id)
	if cut := strings.IndexByte(text, '-'); cut >= 0 && cut+1 < len(text) {
		text = text[cut+1:]
	}
	if len(text) > artifactDigits {
		text = text[:artifactDigits]
	}
	return text
}

// Clip shortens text to what a line of the log shows of a payload.
//
// Exported so a listing shows as much of a command as the log does. Two
// widths for the same thing would mean an approval looked one way as it
// happened and another way when asked about.
func Clip(text string) string { return clip(text, previewWidth) }

// clip shortens text to a number of display cells.
//
// Cells rather than bytes or runes: a Chinese character occupies two columns
// and an emoji with a modifier occupies two while being several runes, and
// counting either of the other two puts the ellipsis in a different place than
// the one the reader sees.
func clip(text string, cells int) string {
	text = collapse(text)
	if ansi.GraphemeWidth.StringWidth(text) <= cells {
		return text
	}
	return ansi.GraphemeWidth.Truncate(text, cells, "…")
}

// collapse puts a multi-line payload onto one line.
//
// Only for the preview. A tool's output can be a hundred lines, and a hundred
// lines in the running log is the rest of the log gone.
func collapse(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t'
	})
	return strings.TrimSpace(strings.Join(fields, " "))
}

// Describe turns an event into a line, or says there is nothing to show.
//
// Deltas are not shown. A console that printed every token would be a console
// nobody could read, and the completed message says the same thing once.
func Describe(event domain.Event) (Line, bool) {
	line := Line{At: event.OccurredAt, Session: event.SessionID}

	switch payload := event.Payload.(type) {
	case domain.UserMessageAdded:
		line.Kind = "MESSAGE"
		line.Meta = origin(payload.Origin)
		line.Preview = payload.Text

	case domain.RunStateChanged:
		line.Kind = "RUN"
		line.State = string(payload.Status)
		line.Preview = payload.Reason

	case domain.AssistantMessageCompleted:
		line.Kind = "ANSWER"
		line.State = string(payload.StopReason)

	case domain.ToolCallRequested:
		line.Kind = "TOOL"
		line.State = "→"
		line.Meta = payload.Name
		line.Preview = payload.Arguments

	case domain.ToolCallCompleted:
		line.Kind = "TOOL"
		line.State = "✓"
		if payload.IsError {
			line.State = "✗"
		}
		line.Meta = payload.Name
		line.Preview = firstOf(payload.Summary, payload.Content)

		// Named, because otherwise the only way to reach a build log is to
		// know it exists. The id is what `open` takes, so it has to be on the
		// line somebody is reading when they decide they want it.
		if payload.Artifact != nil {
			line.Meta += " · output " + shortArtifact(payload.Artifact.ID)
		}

	case domain.ApprovalRequested:
		line.Kind = "APPROVAL"
		line.State = "pending"
		line.Meta = approvalMeta(payload)
		// The arguments as they were requested, never a summary of them. A
		// decision about whether to run a command has to be made against the
		// command, and a rewritten one is a different thing being approved.
		line.Preview = payload.Arguments

	case domain.ApprovalResolved:
		line.Kind = "APPROVAL"
		line.State = string(payload.Status)
		line.Meta = string(payload.ApprovalID)
		line.Preview = payload.DecidedBy.Describe()

	case domain.QuestionAsked:
		line.Kind = "QUESTION"
		line.State = "waiting"
		line.Meta = string(payload.QuestionID)
		line.Preview = payload.Prompt

	case domain.QuestionAnswered:
		line.Kind = "QUESTION"
		line.State = string(payload.Status)
		line.Meta = payload.AnsweredBy.Describe()
		line.Preview = payload.Answer

	case domain.PlanChanged:
		line.Kind = "PLAN"
		line.Meta = planProgress(payload.Items)
		line.Preview = currentStep(payload.Items)

	case domain.SkillActivated:
		line.Kind = "SKILL"
		line.Meta = payload.Name
		// The digest rather than the version: what a run followed is what was
		// read, and a file edited without touching its version line would
		// otherwise look like the same instructions.
		line.Preview = payload.Digest

	case domain.ConversationCompacted:
		line.Kind = "COMPACTED"
		line.Preview = payload.Summary

	default:
		return Line{}, false
	}

	return line, true
}

// approvalMeta is what has to survive next to an approval however long the
// command is.
func approvalMeta(payload domain.ApprovalRequested) string {
	parts := []string{payload.ToolName}
	if len(payload.Effects) > 0 {
		parts = append(parts, strings.Join(payload.Effects, ","))
	}
	return strings.Join(parts, " ")
}

// origin says where a message came from, since the same session can be
// reached from more than one place.
//
// The person's display name rather than their identifier: this is a line
// somebody reads, and an identifier is only useful once you already know
// whose it is.
func origin(from domain.RunOrigin) string {
	if from.Kind == "" {
		return ""
	}
	if from.Principal == nil || from.Principal.DisplayName == "" {
		return string(from.Kind)
	}
	return fmt.Sprintf("%s:%s", from.Kind, from.Principal.DisplayName)
}

// planProgress counts the steps that are done against all of them.
func planProgress(items []domain.PlanItem) string {
	if len(items) == 0 {
		return ""
	}
	done := 0
	for _, item := range items {
		if item.Status == domain.PlanCompleted {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(items))
}

// currentStep is the step a plan is on, which is the one thing about a plan
// that is worth a line in a running log.
func currentStep(items []domain.PlanItem) string {
	for _, item := range items {
		if item.Status == domain.PlanInProgress {
			return item.Title
		}
	}
	for _, item := range items {
		if item.Status != domain.PlanCompleted && item.Status != domain.PlanAbandoned {
			return item.Title
		}
	}
	return ""
}

func firstOf(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
