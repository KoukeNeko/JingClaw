package runtime

import (
	"context"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// DefaultViewMessages is how much of a conversation a view returns when the
// caller does not say. Enough to fill a screen and scroll a little.
const DefaultViewMessages = 50

// SessionView is the state of a session now.
//
// It exists because the alternative is every client replaying the whole event
// log to work out what is on the screen. That is correct, and it gets slower
// with every turn, until opening a conversation somebody has used for a week
// is a visible wait. It is also the same reconstruction written once per
// client, which is the same bug written once per client.
type SessionView struct {
	Session  domain.Session
	Messages []ViewMessage

	// Pending are approvals still waiting. Without them a client would draw a
	// conversation that looks finished and is actually blocked on somebody.
	Pending []domain.Approval

	// ActiveRun is the run in flight, if any.
	ActiveRun *domain.Run

	// HeadSeq is the last event this view accounts for. Subscribing after it
	// continues exactly where the view stops, so nothing between the two is
	// missed and nothing is drawn twice.
	HeadSeq domain.Seq

	// Truncated says older messages exist than the ones returned.
	Truncated bool

	// Questions are what the agent asked and nobody has answered. Like
	// pending approvals: without them a client draws a conversation that
	// looks finished and is actually waiting to be answered.
	Questions []domain.Question

	// Plan is what the agent said it was going to do, if it said anything.
	// Without it a client that opened a session rather than watching it
	// happen sees a run working through a plan it cannot see.
	Plan []domain.PlanItem
}

// ViewMessage is one turn as a client would draw it.
type ViewMessage struct {
	ID   domain.MessageID
	Role domain.MessageRole
	Text string

	// Reasoning is the model's working-out for this turn, where a provider
	// exposed it. Answered only on the control plane; the projector refuses
	// the events it is built from, so no chat platform can carry it.
	Reasoning string

	At        int64
	ToolCalls []ViewToolCall
	Seq       domain.Seq
}

// ViewToolCall is one tool a turn asked for.
type ViewToolCall struct {
	CallID    domain.ToolCallID
	Name      string
	Summary   string
	Completed bool
	IsError   bool

	// Artifact is stored output this call produced, when it produced any.
	// Carried here as well as on the event because a client that opened a
	// session rather than watching it happen has only this.
	Artifact *domain.Artifact
}

// SessionViewOf assembles the current state of a session.
//
// The whole log is still read, because the log is the truth and there is no
// snapshot to read instead. What this saves is not the reading — that is one
// query — but every client doing the reconstruction, and the wire cost of
// sending thousands of events to draw fifty messages.
func (r *Runtime) SessionViewOf(
	ctx context.Context,
	sessionID domain.SessionID,
	maxMessages int,
) (SessionView, error) {
	if maxMessages <= 0 {
		maxMessages = DefaultViewMessages
	}

	session, err := r.opts.Store.Session(ctx, sessionID)
	if err != nil {
		return SessionView{}, err
	}

	events, err := r.opts.Store.ListAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return SessionView{}, err
	}

	view := SessionView{Session: session}
	messages, active := foldEvents(events)

	view.HeadSeq = headSeq(events)
	view.ActiveRun = r.activeRun(ctx, active)

	if len(messages) > maxMessages {
		view.Truncated = true
		messages = messages[len(messages)-maxMessages:]
	}
	view.Messages = messages

	if view.Plan, err = r.opts.Store.Plan(ctx, sessionID); err != nil {
		return SessionView{}, err
	}
	if view.Questions, err = r.opts.Store.PendingQuestions(ctx, sessionID); err != nil {
		return SessionView{}, err
	}

	pending, err := r.PendingApprovals(ctx, sessionID)
	if err != nil {
		return SessionView{}, err
	}
	view.Pending = pending

	return view, nil
}

// foldEvents turns a log into the messages a client would draw.
func foldEvents(events []domain.Event) ([]ViewMessage, domain.RunID) {
	var (
		messages []ViewMessage
		active   domain.RunID

		// byMessage indexes into messages, so a delta can find the message it
		// belongs to without scanning back through everything.
		byMessage = map[domain.MessageID]int{}
		byCall    = map[domain.ToolCallID]struct{ message, call int }{}
	)

	appendMessage := func(id domain.MessageID, role domain.MessageRole, event domain.Event) int {
		messages = append(messages, ViewMessage{
			ID: id, Role: role, At: event.OccurredAt.UnixNano(), Seq: event.Seq,
		})
		index := len(messages) - 1
		if id != "" {
			byMessage[id] = index
		}
		return index
	}

	for _, event := range events {
		switch payload := event.Payload.(type) {
		case domain.UserMessageAdded:
			index := appendMessage(payload.MessageID, domain.RoleUser, event)
			messages[index].Text = payload.Text

		case domain.AssistantTextDelta:
			index, ok := byMessage[payload.MessageID]
			if !ok {
				index = appendMessage(payload.MessageID, domain.RoleAssistant, event)
			}
			messages[index].Text += payload.Text
			messages[index].Seq = event.Seq

		case domain.AssistantReasoningDelta:
			// Onto the same turn as the answer, in its own field. It arrives
			// before the turn has any text, so the message is created here as
			// readily as by a delta.
			index, ok := byMessage[payload.MessageID]
			if !ok {
				index = appendMessage(payload.MessageID, domain.RoleAssistant, event)
			}
			messages[index].Reasoning += payload.Text
			messages[index].Seq = event.Seq

		case domain.AssistantMessageCompleted:
			if index, ok := byMessage[payload.MessageID]; ok {
				messages[index].Seq = event.Seq
			}

		case domain.ToolCallRequested:
			// Attached to the assistant message being written, which is the
			// one that asked for it.
			index := lastAssistant(messages)
			if index < 0 {
				index = appendMessage("", domain.RoleAssistant, event)
			}
			messages[index].ToolCalls = append(messages[index].ToolCalls, ViewToolCall{
				CallID:  payload.CallID,
				Name:    payload.Name,
				Summary: summarise(payload.Name, payload.Arguments),
			})
			byCall[payload.CallID] = struct{ message, call int }{
				index, len(messages[index].ToolCalls) - 1,
			}
			messages[index].Seq = event.Seq

		case domain.ToolCallCompleted:
			at, ok := byCall[payload.CallID]
			if !ok {
				continue
			}
			call := &messages[at.message].ToolCalls[at.call]
			call.Completed = true
			call.IsError = payload.IsError
			if payload.Summary != "" {
				call.Summary = payload.Summary
			}
			call.Artifact = payload.Artifact

		case domain.ConversationCompacted:
			// Everything before the fold is a summary now. A client drawing
			// the messages themselves would show history the model no longer
			// has, so the view says where the fold is by starting after it.
			messages = nil
			byMessage = map[domain.MessageID]int{}
			byCall = map[domain.ToolCallID]struct{ message, call int }{}
			index := appendMessage("", domain.RoleAssistant, event)
			messages[index].Text = "[earlier turns were folded into a summary]"

		case domain.RunStateChanged:
			if payload.Status.IsTerminal() {
				if active == event.RunID {
					active = ""
				}
				continue
			}
			active = event.RunID
		}
	}

	return messages, active
}

func lastAssistant(messages []ViewMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == domain.RoleAssistant {
			return i
		}
	}
	return -1
}

func headSeq(events []domain.Event) domain.Seq {
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

func (r *Runtime) activeRun(ctx context.Context, id domain.RunID) *domain.Run {
	if id == "" {
		return nil
	}

	run, err := r.opts.Store.Run(ctx, id)
	if err != nil {
		return nil
	}
	// Re-read rather than trusted from the fold: a run that ended while this
	// was being assembled would otherwise be reported as still going.
	if run.Status.IsTerminal() {
		return nil
	}
	return &run
}

// summarise says what a call is doing, until its own summary arrives.
func summarise(name, arguments string) string {
	const maxLength = 120

	compact := strings.Join(strings.Fields(arguments), " ")
	if compact == "" || compact == "{}" {
		return name
	}
	if len(compact) > maxLength {
		compact = compact[:maxLength] + "…"
	}
	return name + " " + compact
}
