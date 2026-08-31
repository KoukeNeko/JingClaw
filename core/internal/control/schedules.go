package control

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// CreateSchedule stores a standing instruction.
//
// Creating one is a decision somebody makes, so it arrives through the
// control plane like every other decision. What it is not is a licence for
// that person's authority to act later: the run a schedule makes carries the
// schedule as its origin, and the profile it runs under refuses everything a
// person would have been asked about.
func (s *Server) CreateSchedule(
	ctx context.Context,
	req *connect.Request[controlv1.CreateScheduleRequest],
) (*connect.Response[controlv1.CreateScheduleResponse], error) {
	if req.Msg.GetExpression() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("expression is required, such as \"0 9 * * *\" for every day at nine"))
	}
	if req.Msg.GetPrompt() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("prompt is required: a schedule has to have something to ask"))
	}

	created, err := s.rt.NewSchedule(ctx, domain.Schedule{
		Expression: req.Msg.GetExpression(),
		Zone:       req.Msg.GetZone(),
		Prompt:     req.Msg.GetPrompt(),
		SessionID:  domain.SessionID(req.Msg.GetSessionId()),

		// Who authorized the automation, which is not who acts when it runs.
		CreatedBy: whoIsAsking(req.Msg.GetMeta()),

		Deliver: deliveryTargetsFromProto(req.Msg.GetDeliver()),
		Missed:  domain.MissedPolicy(req.Msg.GetMissedPolicy()),
	})
	if err != nil {
		return nil, toConnectError(err)
	}

	return connect.NewResponse(&controlv1.CreateScheduleResponse{
		Schedule: scheduleToProto(created, s.nextFor(ctx, created)),
	}), nil
}

func (s *Server) ListSchedules(
	ctx context.Context,
	_ *connect.Request[controlv1.ListSchedulesRequest],
) (*connect.Response[controlv1.ListSchedulesResponse], error) {
	schedules, next, err := s.rt.Schedules(ctx)
	if err != nil {
		return nil, toConnectError(err)
	}

	converted := make([]*controlv1.Schedule, 0, len(schedules))
	for index, one := range schedules {
		converted = append(converted, scheduleToProto(one, next[index]))
	}
	return connect.NewResponse(&controlv1.ListSchedulesResponse{Schedules: converted}), nil
}

func (s *Server) DeleteSchedule(
	ctx context.Context,
	req *connect.Request[controlv1.DeleteScheduleRequest],
) (*connect.Response[controlv1.DeleteScheduleResponse], error) {
	id := domain.ScheduleID(req.Msg.GetScheduleId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("schedule_id is required"))
	}
	if err := s.rt.ForgetSchedule(ctx, id); err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&controlv1.DeleteScheduleResponse{}), nil
}

func (s *Server) SetSchedulePaused(
	ctx context.Context,
	req *connect.Request[controlv1.SetSchedulePausedRequest],
) (*connect.Response[controlv1.SetSchedulePausedResponse], error) {
	id := domain.ScheduleID(req.Msg.GetScheduleId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("schedule_id is required"))
	}

	changed, err := s.rt.PauseSchedule(ctx, id, req.Msg.GetPaused())
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&controlv1.SetSchedulePausedResponse{
		Schedule: scheduleToProto(changed, s.nextFor(ctx, changed)),
	}), nil
}

// nextFor is when one schedule next comes due, for a response about it.
func (s *Server) nextFor(ctx context.Context, one domain.Schedule) time.Time {
	schedules, next, err := s.rt.Schedules(ctx)
	if err != nil {
		return time.Time{}
	}
	for index, listed := range schedules {
		if listed.ID == one.ID {
			return next[index]
		}
	}
	return time.Time{}
}

func scheduleToProto(one domain.Schedule, next time.Time) *controlv1.Schedule {
	converted := &controlv1.Schedule{
		Id:           string(one.ID),
		Revision:     int32(one.Revision),
		Expression:   one.Expression,
		Zone:         one.Zone,
		Prompt:       one.Prompt,
		SessionId:    string(one.SessionID),
		CreatedBy:    originToProto(one.CreatedBy),
		Deliver:      deliveryTargetsToProto(one.Deliver),
		MissedPolicy: string(one.Missed),
		Paused:       one.Paused,
		CreatedAt:    timestamppb.New(one.CreatedAt),
	}
	// Absent rather than zero for a paused schedule: reporting a next time
	// would be saying it will run then, which it will not.
	if !next.IsZero() {
		converted.NextAt = timestamppb.New(next)
	}
	return converted
}
