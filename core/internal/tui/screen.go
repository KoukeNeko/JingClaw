package tui

import (
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Screen is a session as the panel draws it.
//
// Its own type rather than the runtime's SessionView, because this is what a
// client builds by watching events go past — starting from a view the daemon
// computed and folding on whatever arrives after it. The daemon has the log
// and can read any of it; the panel has a cursor and whatever it has seen.
type Screen struct {
	Messages []Message

	// Waiting is what somebody has to decide, in the order raised.
	//
	// The whole request rather than its id, because deciding needs what the
	// call actually is. A panel holding only ids would have to ask the daemon
	// again to draw the thing it is asking about.
	Waiting []Waiting

	// ActiveRun is the run in flight, empty when none is.
	ActiveRun domain.RunID

	Plan []PlanStep

	// HeadSeq is the last event folded in, which is where the panel resumes
	// after the stream drops.
	HeadSeq domain.Seq
}

// Message is one turn.
type Message struct {
	Role domain.MessageRole
	Text string

	// Reasoning is the model's working-out, kept apart from the answer it
	// arrives before. Joining them would draw the thinking as the reply.
	Reasoning string

	ToolCalls []ToolCall
}

// ToolCall is one tool a turn asked for, and how it ended.
type ToolCall struct {
	// ID is what a result comes back against. Not drawn anywhere; kept
	// because the name is not unique within a turn.
	ID string

	Name      string
	Completed bool
	IsError   bool

	// Artifact is the id of output too large to have been sent, empty when
	// there was none. The id and not the bytes: the panel fetches it only if
	// somebody asks to see it.
	Artifact string
}

// Waiting is one call the agent stopped to ask about.
type Waiting struct {
	ID       domain.ApprovalID
	ToolName string

	// Summary and Preview are the call rendered for whoever is deciding.
	// Preview is what a person reads; Arguments are what will actually run,
	// and a decision made against a rendering that disagreed with them would
	// be a decision about something else.
	Summary   string
	Preview   string
	Arguments string

	// Effects is what the daemon says this will touch.
	Effects []string

	// ReadForeign says the run had taken in text somebody else wrote before
	// it asked for this.
	//
	// Not a judgement about the call. It is the one thing the person deciding
	// cannot see for themselves: the request looks the same whether the agent
	// arrived at it or a page it read suggested it, and only the log knows.
	ReadForeign bool
}

// PlanStep is one step of what the agent said it would do.
type PlanStep struct {
	ID     string
	Title  string
	Status domain.PlanStatus
}

// Fold applies one event to a screen.
//
// Every event moves HeadSeq, including the ones that change nothing else. The
// cursor is a record of what has been read rather than of what was drawn, and
// a panel that only advanced it on events it understood would ask for the
// same unread ones forever.
func Fold(screen Screen, event domain.Event) Screen {
	screen.HeadSeq = event.Seq

	switch payload := event.Payload.(type) {
	case domain.UserMessageAdded:
		screen.Messages = append(screen.Messages, Message{
			Role: domain.RoleUser, Text: payload.Text,
		})

	case domain.AssistantTextDelta:
		screen.Messages, _ = openAssistant(screen.Messages)
		last(screen.Messages).Text += payload.Text

	case domain.AssistantReasoningDelta:
		screen.Messages, _ = openAssistant(screen.Messages)
		last(screen.Messages).Reasoning += payload.Text

	case domain.ToolCallRequested:
		screen.Messages, _ = openAssistant(screen.Messages)
		turn := last(screen.Messages)
		turn.ToolCalls = append(turn.ToolCalls,
			ToolCall{ID: string(payload.CallID), Name: payload.Name})

	case domain.ToolCallCompleted:
		artifact := ""
		if payload.Artifact != nil {
			artifact = payload.Artifact.ID
		}
		markCompleted(screen.Messages, string(payload.CallID), payload.IsError, artifact)

	case domain.ApprovalRequested:
		screen.Waiting = append(screen.Waiting, Waiting{
			ID:          payload.ApprovalID,
			ToolName:    payload.ToolName,
			Summary:     payload.Summary,
			Preview:     payload.Preview,
			Arguments:   payload.Arguments,
			Effects:     payload.Effects,
			ReadForeign: payload.ReadForeign,
		})

	case domain.ApprovalResolved:
		screen.Waiting = without(screen.Waiting, payload.ApprovalID)

	case domain.ConversationCompacted:
		// What came before the fold is a summary now. The notice replaces it
		// rather than joining it, so nobody reads folded turns as still being
		// in the conversation the model sees.
		screen.Messages = []Message{{Role: domain.RoleAssistant, Text: domain.FoldNotice}}

	case domain.PlanChanged:
		// The whole plan every time, replacing what was there. The event
		// carries the list and not the change, so a panel that attached late
		// reads one entry and knows where things stand.
		screen.Plan = nil
		for _, item := range payload.Items {
			screen.Plan = append(screen.Plan, PlanStep{
				ID: item.ID, Title: item.Title, Status: item.Status,
			})
		}

	case domain.RunStateChanged:
		switch payload.Status {
		case domain.RunCompleted, domain.RunFailed, domain.RunCancelled:
			if screen.ActiveRun == event.RunID {
				screen.ActiveRun = ""
			}
		default:
			screen.ActiveRun = event.RunID
		}
	}

	return screen
}

// FoldAll folds a whole sequence, which is what the panel does on attach.
func FoldAll(events []domain.Event) Screen {
	screen := Screen{}
	for _, event := range events {
		screen = Fold(screen, event)
	}
	return screen
}

// openAssistant makes sure the last message is an assistant turn to write on,
// starting one if it is not, and says whether it had to.
//
// A user turn closes the one before it: what arrives next is the answer to
// that turn rather than more of the previous answer.
func openAssistant(messages []Message) ([]Message, bool) {
	if len(messages) > 0 && messages[len(messages)-1].Role == domain.RoleAssistant {
		return messages, false
	}
	return append(messages, Message{Role: domain.RoleAssistant}), true
}

// last is the turn being written, by pointer so callers can add to it.
func last(messages []Message) *Message {
	return &messages[len(messages)-1]
}

// markCompleted closes the call the result belongs to.
//
// By id and not by name. Tools run at the same time and need not come back in
// the order they went out, so a search by name lands the result on whichever
// call it reaches first — drawing a failure against a call that succeeded.
func markCompleted(messages []Message, id string, failed bool, artifact string) {
	for message := len(messages) - 1; message >= 0; message-- {
		calls := messages[message].ToolCalls
		for call := len(calls) - 1; call >= 0; call-- {
			if calls[call].ID != id || calls[call].Completed {
				continue
			}
			calls[call].Completed = true
			calls[call].IsError = failed
			calls[call].Artifact = artifact
			return
		}
	}
}

// without drops one request, keeping the order of the rest.
func without(requests []Waiting, decided domain.ApprovalID) []Waiting {
	kept := make([]Waiting, 0, len(requests))
	for _, request := range requests {
		if request.ID != decided {
			kept = append(kept, request)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
