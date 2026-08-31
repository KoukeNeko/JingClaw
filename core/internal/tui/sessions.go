package tui

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Sessions is where the panel gets what it draws.
//
// Declared here, by the side that uses it, and in the vocabulary the panel
// thinks in. The alternative is the panel holding a generated client, which
// would mean every screen test needs a control-plane stub and every proto
// change reaches into the drawing code.
type Sessions interface {
	// List is the sessions to choose between, most recently touched first.
	List(ctx context.Context) ([]Summary, error)

	// Open is a session as it stands, and the sequence it stands at.
	Open(ctx context.Context, id domain.SessionID) (Screen, error)

	// Decide answers one request the agent stopped to ask about.
	Decide(ctx context.Context, decision Decision) error

	// Answer unblocks a run that stopped to ask a person something.
	Answer(ctx context.Context, answer Answer) error

	// Interrupt stops a run that is still going.
	Interrupt(ctx context.Context, run domain.RunID) error

	// ReadArtifact is stored output, read back whole.
	//
	// Whole because what it is for is handing to another program, and half a
	// build log is a file that opens and says the wrong thing.
	ReadArtifact(ctx context.Context, id string) ([]byte, error)

	// Watch is what happens after that sequence, until the context ends.
	//
	// A channel rather than a callback because the panel's loop is already
	// waiting on messages, and closing it is how the panel learns the stream
	// ended rather than having to ask.
	Watch(ctx context.Context, id domain.SessionID, after domain.Seq) <-chan Update
}

// Summary is one session in the list.
type Summary struct {
	ID    domain.SessionID
	Title string
}

// Decision is an answer to one request.
//
// Status is stated rather than implied by a flag. A boolean could not tell a
// deny from a field the server did not understand, which is how a client with
// a typo once refused tools on the operator's behalf and reported success.
type Decision struct {
	ID     domain.ApprovalID
	Status domain.ApprovalStatus
	Scope  domain.RememberScope
}

// Answer is what somebody typed, against the question it answers.
type Answer struct {
	ID   domain.QuestionID
	Text string
}

// Update is one thing that happened while watching.
//
// Three outcomes in one message because they arrive on one channel and the
// panel has to tell them apart: an event to fold, a gap it cannot fold over,
// or the stream ending badly.
type Update struct {
	Event *domain.Event

	// OldestSeq is set when the log moved past where the panel was, so the
	// events between are gone. Said rather than swallowed: a panel that
	// quietly resumed from the new oldest would draw a conversation with a
	// hole in it and nothing to show there had been one.
	OldestSeq domain.Seq

	Err error
}

// clientName identifies this panel to the daemon's stream bookkeeping.
const clientName = "tui"

// maxArtifactBytes bounds what the panel will write out to be opened.
//
// Generous, because the thing this exists for is a build log nobody wants
// truncated, and bounded because "whatever a tool produced" is not a size.
const maxArtifactBytes = 64 << 20

// overTheControlPlane is the Sessions the panel actually runs against.
type overTheControlPlane struct {
	daemon    controlv1connect.SessionServiceClient
	artifacts controlv1connect.ArtifactServiceClient
}

// Over adapts control-plane clients to what the panel needs.
func Over(
	daemon controlv1connect.SessionServiceClient,
	artifacts controlv1connect.ArtifactServiceClient,
) Sessions {
	return overTheControlPlane{daemon: daemon, artifacts: artifacts}
}

func (over overTheControlPlane) List(ctx context.Context) ([]Summary, error) {
	answer, err := over.daemon.ListSessions(ctx, connect.NewRequest(
		&controlv1.ListSessionsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("tui: asking for the sessions: %w", err)
	}

	listed := make([]Summary, 0, len(answer.Msg.GetSessions()))
	for _, session := range answer.Msg.GetSessions() {
		listed = append(listed, Summary{
			ID:    domain.SessionID(session.GetId()),
			Title: session.GetTitle(),
		})
	}
	return listed, nil
}

func (over overTheControlPlane) Open(
	ctx context.Context, id domain.SessionID,
) (Screen, error) {
	answer, err := over.daemon.GetSessionView(ctx, connect.NewRequest(
		&controlv1.GetSessionViewRequest{SessionId: string(id)}))
	if err != nil {
		return Screen{}, fmt.Errorf("tui: opening %s: %w", id, err)
	}
	return screenOf(answer.Msg), nil
}

// screenOf is the daemon's assembled view in the shape the panel folds onto.
//
// The daemon assembles it because assembling it is the part that costs, and
// because a client that rebuilt the whole conversation from events every time
// would do the same reconstruction the server already did. What the panel
// folds after this is only what arrives next.
func screenOf(view *controlv1.GetSessionViewResponse) Screen {
	screen := Screen{HeadSeq: domain.Seq(view.GetHeadSeq())}

	for _, message := range view.GetMessages() {
		drawn := Message{
			Role:      roleOf(message.GetRole()),
			Text:      message.GetText(),
			Reasoning: message.GetReasoning(),
		}
		for _, call := range message.GetToolCalls() {
			shown := ToolCall{
				ID:        call.GetCallId(),
				Name:      call.GetName(),
				Completed: call.GetCompleted(),
				IsError:   call.GetIsError(),
			}
			if stored := call.GetArtifact(); stored != nil {
				shown.Artifact = stored.GetId()
				shown.MediaType = stored.GetMediaType()
			}
			drawn.ToolCalls = append(drawn.ToolCalls, shown)
		}
		screen.Messages = append(screen.Messages, drawn)
	}

	for _, asked := range view.GetPendingApprovals() {
		screen.Waiting = append(screen.Waiting, Waiting{
			ID:          domain.ApprovalID(asked.GetId()),
			ToolName:    asked.GetToolName(),
			Summary:     asked.GetSummary(),
			Preview:     asked.GetPreview(),
			Arguments:   asked.GetArguments(),
			Effects:     asked.GetEffects(),
			ReadForeign: asked.GetReadForeign(),
		})
	}
	for _, question := range view.GetPendingQuestions() {
		asked := Asked{
			ID:     domain.QuestionID(question.GetId()),
			Prompt: question.GetPrompt(),
			Kind:   questionKindOf(question.GetKind()),
		}
		for _, option := range question.GetOptions() {
			asked.Options = append(asked.Options, Option{
				ID: option.GetId(), Label: option.GetLabel(), Detail: option.GetDetail(),
			})
		}
		screen.Asked = append(screen.Asked, asked)
	}
	for _, step := range view.GetPlan() {
		screen.Plan = append(screen.Plan, PlanStep{
			ID: step.GetId(), Title: step.GetTitle(),
			Status: control.PlanStatusFromProto(step.GetStatus()),
		})
	}
	if run := view.GetActiveRun(); run != nil {
		screen.ActiveRun = domain.RunID(run.GetId())
	}
	return screen
}

func roleOf(role controlv1.MessageRole) domain.MessageRole {
	if role == controlv1.MessageRole_MESSAGE_ROLE_USER {
		return domain.RoleUser
	}
	return domain.RoleAssistant
}

func (over overTheControlPlane) Watch(
	ctx context.Context, id domain.SessionID, after domain.Seq,
) <-chan Update {
	updates := make(chan Update)

	go func() {
		defer close(updates)

		stream, err := over.daemon.SubscribeEvents(ctx, connect.NewRequest(
			&controlv1.SubscribeEventsRequest{
				SessionId: string(id),
				AfterSeq:  uint64(after),
				ClientId:  clientName,
			}))
		if err != nil {
			send(ctx, updates, Update{Err: fmt.Errorf("tui: following %s: %w", id, err)})
			return
		}
		defer func() { _ = stream.Close() }()

		for stream.Receive() {
			switch frame := stream.Msg().GetValue().(type) {
			case *controlv1.SubscribeEventsResponse_Event:
				read, err := control.EventFromProto(frame.Event)
				if err != nil {
					// One the panel does not know. Said rather than dropped:
					// a client that silently ignores an unfamiliar event is
					// one where a new kind of them is invisible.
					send(ctx, updates, Update{Err: fmt.Errorf(
						"tui: an event this panel does not understand: %w", err)})
					continue
				}
				if !send(ctx, updates, Update{Event: &read}) {
					return
				}

			case *controlv1.SubscribeEventsResponse_ResyncRequired:
				if !send(ctx, updates, Update{
					OldestSeq: domain.Seq(frame.ResyncRequired.GetOldestSeq()),
				}) {
					return
				}
			}
		}

		if err := stream.Err(); err != nil && ctx.Err() == nil {
			send(ctx, updates, Update{Err: fmt.Errorf("tui: lost the daemon: %w", err)})
		}
	}()

	return updates
}

// send offers an update and says whether anyone is still listening.
//
// Selected against the context rather than sent plainly, because the panel
// stops reading the moment it leaves a session and a bare send would leave
// this goroutine parked on a channel nobody will ever read.
func send(ctx context.Context, updates chan<- Update, update Update) bool {
	select {
	case updates <- update:
		return true
	case <-ctx.Done():
		return false
	}
}

// errNoSessions is what an empty list means, kept as a value so the panel can
// say something other than an error about it.
var errNoSessions = errors.New("tui: no sessions yet")

func (over overTheControlPlane) Decide(ctx context.Context, decision Decision) error {
	_, err := over.daemon.DecideApproval(ctx, connect.NewRequest(
		&controlv1.DecideApprovalRequest{
			ApprovalId: string(decision.ID),
			Decision:   decisionToProto(decision.Status),
			Remember:   scopeToProto(decision.Scope),
		}))
	if err != nil {
		return fmt.Errorf("tui: deciding %s: %w", decision.ID, err)
	}
	return nil
}

// decisionToProto states the decision on the wire.
//
// Anything that is not an allow is a deny, because there is no third thing to
// send and a request the panel could not classify must not be sent as an
// unspecified decision the daemon would refuse after the person believed they
// had answered.
func decisionToProto(status domain.ApprovalStatus) controlv1.ApprovalDecision {
	if status == domain.ApprovalAllowed {
		return controlv1.ApprovalDecision_APPROVAL_DECISION_ALLOW
	}
	return controlv1.ApprovalDecision_APPROVAL_DECISION_DENY
}

func scopeToProto(scope domain.RememberScope) controlv1.RememberScope {
	if scope == domain.RememberSession {
		return controlv1.RememberScope_REMEMBER_SCOPE_SESSION
	}
	return controlv1.RememberScope_REMEMBER_SCOPE_ONCE
}

func (over overTheControlPlane) Interrupt(ctx context.Context, run domain.RunID) error {
	_, err := over.daemon.InterruptRun(ctx, connect.NewRequest(
		&controlv1.InterruptRunRequest{
			RunId:  string(run),
			Reason: "asked to stop from the panel",
		}))
	if err != nil {
		return fmt.Errorf("tui: interrupting %s: %w", run, err)
	}
	return nil
}

// questionKindOf says what shape of answer a question wants.
//
// Anything unrecognised is text, because a panel that offered no way to
// answer would leave the run parked with nothing anybody could do about it.
func questionKindOf(kind controlv1.QuestionKind) domain.QuestionKind {
	if kind == controlv1.QuestionKind_QUESTION_KIND_CHOICE {
		return domain.QuestionChoice
	}
	return domain.QuestionText
}

func (over overTheControlPlane) Answer(ctx context.Context, answer Answer) error {
	_, err := over.daemon.AnswerQuestion(ctx, connect.NewRequest(
		&controlv1.AnswerQuestionRequest{
			QuestionId: string(answer.ID),
			Answer:     answer.Text,
		}))
	if err != nil {
		return fmt.Errorf("tui: answering %s: %w", answer.ID, err)
	}
	return nil
}

func (over overTheControlPlane) ReadArtifact(ctx context.Context, id string) ([]byte, error) {
	if over.artifacts == nil {
		return nil, fmt.Errorf("tui: this panel cannot reach stored output")
	}

	stream, err := over.artifacts.ReadArtifact(ctx, connect.NewRequest(
		&controlv1.ReadArtifactRequest{Id: id, Limit: maxArtifactBytes}))
	if err != nil {
		return nil, fmt.Errorf("tui: reading %s: %w", id, err)
	}
	defer func() { _ = stream.Close() }()

	var read []byte
	for stream.Receive() {
		read = append(read, stream.Msg().GetChunk()...)
		if len(read) > maxArtifactBytes {
			// Said rather than truncated. A log cut off at an arbitrary point
			// opens and says the wrong thing, and nothing on the screen would
			// show that the end is missing.
			return nil, fmt.Errorf(
				"tui: %s is larger than this panel will write out (%d bytes)",
				id, maxArtifactBytes)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("tui: reading %s: %w", id, err)
	}
	return read, nil
}
