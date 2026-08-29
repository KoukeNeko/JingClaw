// Package control exposes the runtime over Connect RPC.
//
// Its only jobs are translating between wire and domain types and calling the
// runtime. No model, tool or storage logic belongs here: if a rule can be
// bypassed by talking to a different transport, it was written in the wrong
// place.
package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/media"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const (
	// How many events one database read may return. Bounding the batch keeps a
	// client that reconnects with after_seq=0 on a long session from being
	// materialized into memory all at once.
	eventBatchSize = 256

	// Emitted when a stream has been idle, so a client can tell "nothing is
	// happening" apart from "the connection died".
	heartbeatInterval = 20 * time.Second
)

// SessionStore is what the session service reads.
//
// Narrower than the whole of storage.Store: this server may not create a run
// or resolve an approval behind the runtime's back, and an interface that
// cannot express those calls is a stronger statement than a comment saying it
// does not make them.
type SessionStore interface {
	storage.EventStore

	Session(ctx context.Context, id domain.SessionID) (domain.Session, error)
	ListSessions(ctx context.Context) ([]domain.Session, error)
	ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error)
}

// Models is what a client is offered when choosing one.
//
// The provider itself, asked at the time. A local server has whatever
// somebody pulled onto that machine, so a list written here would be wrong on
// every deployment that is not this one.
type Models interface {
	Models(ctx context.Context) ([]provider.ModelInfo, error)
	Name() string
}

// AttachmentStore is where files sent with a turn are kept.
type AttachmentStore interface {
	PutBytes(ctx context.Context, content []byte, mediaType string) (artifact.Ref, error)
}

type Server struct {
	rt        *runtime.Runtime
	store     SessionStore
	hub       *event.Hub
	artifacts AttachmentStore

	// models answers "what could this session use instead", and defaultModel
	// is what a session with no choice of its own uses.
	models       Models
	defaultModel string
}

func NewServer(
	rt *runtime.Runtime,
	store SessionStore,
	hub *event.Hub,
	artifacts AttachmentStore,
	models Models,
	defaultModel string,
) *Server {
	return &Server{
		rt: rt, store: store, hub: hub, artifacts: artifacts,
		models: models, defaultModel: defaultModel,
	}
}

func (s *Server) CreateSession(
	ctx context.Context,
	req *connect.Request[controlv1.CreateSessionRequest],
) (*connect.Response[controlv1.CreateSessionResponse], error) {
	session, err := s.rt.CreateSession(ctx, req.Msg.GetTitle())
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.CreateSessionResponse{
		Session: sessionToProto(session),
	}), nil
}

// ListSessions is how a client that did not start a session finds it.
//
// A run outlives the client that began it. Without this, the web console
// opened after the fact has no way to discover the session somebody started
// from a terminal, which would make "clients are projections" untrue in
// practice however true it is in the design.
func (s *Server) ListSessions(
	ctx context.Context,
	_ *connect.Request[controlv1.ListSessionsRequest],
) (*connect.Response[controlv1.ListSessionsResponse], error) {
	sessions, err := s.store.ListSessions(ctx)
	if err != nil {
		return nil, toConnectError(err)
	}

	converted := make([]*controlv1.Session, 0, len(sessions))
	for _, session := range sessions {
		converted = append(converted, sessionToProto(session))
	}

	return connect.NewResponse(&controlv1.ListSessionsResponse{Sessions: converted}), nil
}

func (s *Server) ListRuns(
	ctx context.Context,
	req *connect.Request[controlv1.ListRunsRequest],
) (*connect.Response[controlv1.ListRunsResponse], error) {
	sessionID := domain.SessionID(req.Msg.GetSessionId())
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}

	runs, err := s.store.ListRuns(ctx, sessionID)
	if err != nil {
		return nil, toConnectError(err)
	}

	converted := make([]*controlv1.Run, 0, len(runs))
	for _, run := range runs {
		converted = append(converted, runToProto(run))
	}

	return connect.NewResponse(&controlv1.ListRunsResponse{Runs: converted}), nil
}

// GetSessionView answers what a session looks like now.
func (s *Server) GetSessionView(
	ctx context.Context,
	req *connect.Request[controlv1.GetSessionViewRequest],
) (*connect.Response[controlv1.GetSessionViewResponse], error) {
	sessionID := domain.SessionID(req.Msg.GetSessionId())
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}

	view, err := s.rt.SessionViewOf(ctx, sessionID, int(req.Msg.GetMaxMessages()))
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(sessionViewToProto(view)), nil
}

func (s *Server) SendTurn(
	ctx context.Context,
	req *connect.Request[controlv1.SendTurnRequest],
) (*connect.Response[controlv1.SendTurnResponse], error) {
	sessionID := domain.SessionID(req.Msg.GetSessionId())
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}

	origin := domain.RunOrigin{
		Kind:     domain.OriginLocalClient,
		ClientID: req.Msg.GetMeta().GetClientId(),
	}

	attachments, err := s.keepAttachments(ctx, req.Msg.GetAttachments())
	if err != nil {
		return nil, err
	}

	runID, messageID, err := s.rt.SendTurnTo(ctx, sessionID, domain.Turn{
		Text:        req.Msg.GetText(),
		Origin:      origin,
		Targets:     []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: origin.ClientID}},
		Attachments: attachments,
	})
	if err != nil {
		return nil, toConnectError(err)
	}

	// Deliberately no answer here. The client subscribes for that, which is why
	// closing a UI never kills an in-flight generation.
	return connect.NewResponse(&controlv1.SendTurnResponse{
		RunId:     string(runID),
		MessageId: string(messageID),
	}), nil
}

// keepAttachments puts what a client sent into the artifact store.
//
// Checked exactly as a file arriving from a chat platform is. A control client
// is trusted to make requests; that is not the same as its files being safe to
// decode, and the machine that runs out of memory decoding a malicious PNG is
// this one either way.
//
// A file that will not do is refused rather than dropped. The caller is a
// person at their own terminal who can see the answer and try again, which is
// not true of a message arriving from a channel.
func (s *Server) keepAttachments(
	ctx context.Context,
	sent []*controlv1.InlineAttachment,
) ([]domain.Attachment, error) {
	if len(sent) == 0 {
		return nil, nil
	}
	if s.artifacts == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("this daemon has nowhere to keep attachments"))
	}
	if len(sent) > media.MaxImagesPerMessage {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("a turn may carry %d files and this has %d",
				media.MaxImagesPerMessage, len(sent)))
	}

	kept := make([]domain.Attachment, 0, len(sent))
	for _, attachment := range sent {
		mediaType, err := media.CheckImage(attachment.GetMediaType(), attachment.GetData())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		ref, err := s.artifacts.PutBytes(ctx, attachment.GetData(), mediaType)
		if err != nil {
			return nil, toConnectError(err)
		}

		kept = append(kept, domain.Attachment{
			ArtifactID: ref.ID,
			Name:       attachment.GetName(),
			MediaType:  mediaType,
			Size:       ref.Size,
		})
	}

	return kept, nil
}

func (s *Server) InterruptRun(
	ctx context.Context,
	req *connect.Request[controlv1.InterruptRunRequest],
) (*connect.Response[controlv1.InterruptRunResponse], error) {
	status, err := s.rt.InterruptRun(ctx, domain.RunID(req.Msg.GetRunId()), req.Msg.GetReason())
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.InterruptRunResponse{
		Status: runStatusToProto(status),
	}), nil
}

func (s *Server) ListApprovals(
	ctx context.Context,
	req *connect.Request[controlv1.ListApprovalsRequest],
) (*connect.Response[controlv1.ListApprovalsResponse], error) {
	approvals, err := s.rt.PendingApprovals(ctx, domain.SessionID(req.Msg.GetSessionId()))
	if err != nil {
		return nil, toConnectError(err)
	}

	out := make([]*controlv1.Approval, 0, len(approvals))
	for _, approval := range approvals {
		out = append(out, approvalToProto(approval))
	}

	return connect.NewResponse(&controlv1.ListApprovalsResponse{Approvals: out}), nil
}

func (s *Server) DecideApproval(
	ctx context.Context,
	req *connect.Request[controlv1.DecideApprovalRequest],
) (*connect.Response[controlv1.DecideApprovalResponse], error) {
	if req.Msg.GetApprovalId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("approval_id is required"))
	}

	// A decision that says nothing is refused rather than read as a deny. The
	// two are indistinguishable to a boolean, which is how a client with a
	// mistyped field name comes to refuse tools on the operator's behalf and
	// report success.
	decision := req.Msg.GetDecision()
	if decision == controlv1.ApprovalDecision_APPROVAL_DECISION_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("decision must be APPROVAL_DECISION_ALLOW or APPROVAL_DECISION_DENY"))
	}

	// The client says allow or deny; the daemon owns what that unlocks. A
	// client cannot widen its own permissions by asking nicely.
	approval, err := s.rt.DecideApproval(ctx,
		domain.ApprovalID(req.Msg.GetApprovalId()),
		decision == controlv1.ApprovalDecision_APPROVAL_DECISION_ALLOW,
		rememberScopeFromProto(req.Msg.GetRemember()),
		req.Msg.GetMeta().GetClientId(),
	)
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.DecideApprovalResponse{
		Approval: approvalToProto(approval),
	}), nil
}

// SubscribeEvents replays everything after the client's cursor, then follows
// the log live.
//
// The loop is deliberately "read the store, then wait for a nudge": the hub
// only signals that something changed, so a dropped or coalesced notification
// costs nothing and a slow client cannot stall the runtime.
func (s *Server) SubscribeEvents(
	ctx context.Context,
	req *connect.Request[controlv1.SubscribeEventsRequest],
	stream *connect.ServerStream[controlv1.SubscribeEventsResponse],
) error {
	sessionID := domain.SessionID(req.Msg.GetSessionId())
	if sessionID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}

	head, err := s.store.Head(ctx, sessionID)
	if err != nil {
		return toConnectError(err)
	}

	// Subscribe before the first read, so an event appended in between is not
	// missed: it will still be sitting in the store when the loop reads.
	sub := s.hub.Subscribe(sessionID)
	defer sub.Close()

	if err := stream.Send(&controlv1.SubscribeEventsResponse{
		Value: &controlv1.SubscribeEventsResponse_Hello{
			Hello: &controlv1.StreamHello{HeadSeq: uint64(head)},
		},
	}); err != nil {
		return err
	}

	cursor := domain.Seq(req.Msg.GetAfterSeq())

	// A client resuming from before what is kept is told so, rather than
	// handed whatever survived. Sending the remainder would draw a
	// conversation missing its middle with nothing marking the gap, and that
	// reads as the agent having forgotten rather than as history discarded on
	// purpose.
	oldest, err := s.store.Oldest(ctx, sessionID)
	if err != nil {
		return toConnectError(err)
	}
	if oldest > 0 && cursor > 0 && cursor < oldest-1 {
		return stream.Send(&controlv1.SubscribeEventsResponse{
			Value: &controlv1.SubscribeEventsResponse_ResyncRequired{
				ResyncRequired: &controlv1.ResyncRequired{
					OldestSeq: uint64(oldest),
					HeadSeq:   uint64(head),
				},
			},
		})
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		// Drain everything currently available before going back to sleep.
		for {
			events, err := s.store.ListAfter(ctx, sessionID, cursor, eventBatchSize)
			if err != nil {
				return toConnectError(err)
			}
			if len(events) == 0 {
				break
			}

			for _, ev := range events {
				msg, err := eventToProto(ev)
				if err != nil {
					return connect.NewError(connect.CodeInternal, err)
				}

				if err := stream.Send(&controlv1.SubscribeEventsResponse{
					Value: &controlv1.SubscribeEventsResponse_Event{Event: msg},
				}); err != nil {
					// The client went away; its own send buffer is where
					// backpressure belongs.
					return err
				}
				cursor = ev.Seq
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-sub.Notify():

		case <-ticker.C:
			if err := stream.Send(&controlv1.SubscribeEventsResponse{
				Value: &controlv1.SubscribeEventsResponse_Heartbeat{
					Heartbeat: &controlv1.StreamHeartbeat{HeadSeq: uint64(cursor)},
				},
			}); err != nil {
				return err
			}
		}
	}
}

func toConnectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrSessionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, runtime.ErrRunNotFound),
		errors.Is(err, storage.ErrApprovalNotFound),
		errors.Is(err, storage.ErrQuestionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, runtime.ErrApprovalNotPending),
		errors.Is(err, storage.ErrApprovalDecided),
		errors.Is(err, storage.ErrQuestionAnswered):
		// Someone already answered. FailedPrecondition rather than an error
		// state: the prompt is settled, the client is simply late.
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, runtime.ErrNoAnswer):
		// The caller sent nothing. InvalidArgument rather than internal: this
		// is a client to fix, not a daemon.
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, runtime.ErrShuttingDown):
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// ListModels is what this daemon's provider offers, and which one a session
// is using.
func (s *Server) ListModels(
	ctx context.Context,
	req *connect.Request[controlv1.ListModelsRequest],
) (*connect.Response[controlv1.ListModelsResponse], error) {
	if s.models == nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("this daemon cannot list its models"))
	}

	available, err := s.models.Models(ctx)
	if err != nil {
		// A provider that cannot be asked is worth saying so about rather than
		// answering with an empty list, which a client would draw as "there
		// are no models" — a different and wrong thing.
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("could not ask the provider what it has: %w", err))
	}

	current := s.defaultModel
	if id := req.Msg.GetSessionId(); id != "" {
		session, err := s.store.Session(ctx, domain.SessionID(id))
		if err != nil {
			return nil, toConnectError(err)
		}
		if session.Model != "" {
			current = session.Model
		}
	}

	models := make([]*controlv1.ModelInfo, 0, len(available))
	for _, one := range available {
		models = append(models, &controlv1.ModelInfo{
			Id:              one.ID,
			DisplayName:     one.DisplayName,
			Description:     one.Description,
			ContextWindow:   int64(one.ContextWindow),
			ContextSource:   string(one.ContextSource),
			MaxOutputTokens: int64(one.MaxOutputTokens),
		})
	}

	return connect.NewResponse(&controlv1.ListModelsResponse{
		Provider: s.models.Name(),
		Models:   models,
		Current:  current,
		Default:  s.defaultModel,
	}), nil
}

// SetSessionModel chooses which model answers in a session.
func (s *Server) SetSessionModel(
	ctx context.Context,
	req *connect.Request[controlv1.SetSessionModelRequest],
) (*connect.Response[controlv1.SetSessionModelResponse], error) {
	id := domain.SessionID(req.Msg.GetSessionId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name the session"))
	}

	session, err := s.rt.SetSessionModel(ctx, id, req.Msg.GetModel())
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.SetSessionModelResponse{
		Session: sessionToProto(session),
	}), nil
}

// ListQuestions is what the agent has asked and nobody has answered.
//
// Alongside ListApprovals rather than folded into it: a client offers a
// different control for the two, and a list that mixed them would make every
// client work out which kind each row was.
func (s *Server) ListQuestions(
	ctx context.Context,
	req *connect.Request[controlv1.ListQuestionsRequest],
) (*connect.Response[controlv1.ListQuestionsResponse], error) {
	questions, err := s.rt.PendingQuestions(ctx, domain.SessionID(req.Msg.GetSessionId()))
	if err != nil {
		return nil, toConnectError(err)
	}

	out := make([]*controlv1.Question, 0, len(questions))
	for _, question := range questions {
		out = append(out, questionToProto(question))
	}
	return connect.NewResponse(&controlv1.ListQuestionsResponse{Questions: out}), nil
}

// AnswerQuestion unblocks a run that stopped to ask.
func (s *Server) AnswerQuestion(
	ctx context.Context,
	req *connect.Request[controlv1.AnswerQuestionRequest],
) (*connect.Response[controlv1.AnswerQuestionResponse], error) {
	answeredBy := req.Msg.GetMeta().GetClientId()
	if answeredBy == "" {
		answeredBy = "unknown"
	}

	question, err := s.rt.AnswerQuestion(ctx,
		domain.QuestionID(req.Msg.GetQuestionId()), req.Msg.GetAnswer(), answeredBy)
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.AnswerQuestionResponse{
		Question: questionToProto(question),
	}), nil
}
