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
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
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

	ListSessions(ctx context.Context) ([]domain.Session, error)
	ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error)
}

type Server struct {
	rt    *runtime.Runtime
	store SessionStore
	hub   *event.Hub
}

func NewServer(rt *runtime.Runtime, store SessionStore, hub *event.Hub) *Server {
	return &Server{rt: rt, store: store, hub: hub}
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

	runID, messageID, err := s.rt.SendTurn(ctx, sessionID, req.Msg.GetText(), origin)
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

	// The client says allow or deny; the daemon owns what that unlocks. A
	// client cannot widen its own permissions by asking nicely.
	approval, err := s.rt.DecideApproval(ctx,
		domain.ApprovalID(req.Msg.GetApprovalId()),
		req.Msg.GetAllow(),
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
	case errors.Is(err, runtime.ErrRunNotFound), errors.Is(err, storage.ErrApprovalNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, runtime.ErrApprovalNotPending), errors.Is(err, storage.ErrApprovalDecided):
		// Someone already answered. FailedPrecondition rather than an error
		// state: the prompt is settled, the client is simply late.
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, runtime.ErrShuttingDown):
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
