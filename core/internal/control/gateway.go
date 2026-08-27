package control

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// GatewayServer exposes the ingress a gateway process speaks.
//
// It is a separate handler from the session service so it can be reached with
// a credential scoped to it alone. A process holding somebody else's bot token
// should not be able to execute tools even if its library is compromised.
type GatewayServer struct {
	ingress *gateway.Ingress
	store   gateway.Store
	now     func() time.Time
}

func NewGatewayServer(ingress *gateway.Ingress, store gateway.Store, now func() time.Time) *GatewayServer {
	if now == nil {
		now = time.Now
	}
	return &GatewayServer{ingress: ingress, store: store, now: now}
}

func (s *GatewayServer) DeliverInbound(
	ctx context.Context,
	req *connect.Request[controlv1.DeliverInboundRequest],
) (*connect.Response[controlv1.DeliverInboundResponse], error) {
	message := req.Msg.GetMessage()
	if message == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message is required"))
	}

	accepted, err := s.ingress.Accept(ctx, inboundFromProto(message))
	if err != nil {
		return nil, gatewayError(err)
	}

	return connect.NewResponse(&controlv1.DeliverInboundResponse{
		SessionId: string(accepted.SessionID),
		RunId:     string(accepted.RunID),
		Duplicate: accepted.Duplicate,
	}), nil
}

// SubscribeDispatches streams undelivered work for one gateway account.
//
// The loop polls rather than waiting on a notification because delivery is not
// latency-critical in the way an interactive event stream is, and because
// redelivery after a failed post has to happen anyway: a gateway that dies
// mid-post is picked up on the next pass rather than needing its own recovery
// path.
func (s *GatewayServer) SubscribeDispatches(
	ctx context.Context,
	req *connect.Request[controlv1.SubscribeDispatchesRequest],
	stream *connect.ServerStream[controlv1.SubscribeDispatchesResponse],
) error {
	accountID := req.Msg.GetAccountId()
	if accountID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("account_id is required"))
	}

	cursor := gateway.DispatchSeq(req.Msg.GetAfterSeq())

	ticker := time.NewTicker(dispatchPollInterval)
	defer ticker.Stop()

	for {
		dispatches, err := s.store.DispatchesAfter(ctx, accountID, cursor, dispatchBatchSize)
		if err != nil {
			return gatewayError(err)
		}

		for _, dispatch := range dispatches {
			if err := stream.Send(&controlv1.SubscribeDispatchesResponse{
				Dispatch: dispatchToProto(dispatch),
			}); err != nil {
				return err
			}

			// The cursor advances on send, not on acknowledgement. An
			// unacknowledged dispatch stays undelivered in storage, so a
			// reconnecting gateway starting from zero still finds it.
			cursor = dispatch.Seq
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *GatewayServer) AcknowledgeDispatch(
	ctx context.Context,
	req *connect.Request[controlv1.AcknowledgeDispatchRequest],
) (*connect.Response[controlv1.AcknowledgeDispatchResponse], error) {
	if req.Msg.GetDispatchId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("dispatch_id is required"))
	}

	err := s.store.MarkDelivered(ctx, req.Msg.GetDispatchId(), req.Msg.GetPlatformMessageIds(), s.now())
	if err != nil {
		return nil, gatewayError(err)
	}

	return connect.NewResponse(&controlv1.AcknowledgeDispatchResponse{}), nil
}

const (
	dispatchBatchSize = 64

	// Delivery is not interactive, so a short poll is enough and avoids a
	// second notification mechanism alongside the event hub.
	dispatchPollInterval = 500 * time.Millisecond
)

func inboundFromProto(message *controlv1.InboundMessage) gateway.InboundMessage {
	claims := make([]gateway.Claim, 0, len(message.GetPrincipalClaims()))
	for _, claim := range message.GetPrincipalClaims() {
		claims = append(claims, gateway.Claim{Namespace: claim.GetNamespace(), Value: claim.GetValue()})
	}

	platform := gateway.Platform(message.GetPlatform())

	return gateway.InboundMessage{
		PlatformMessageID: message.GetPlatformMessageId(),
		IdempotencyKey:    message.GetIdempotencyKey(),
		Principal: gateway.Principal{
			Platform:    platform,
			AccountID:   message.GetAccountId(),
			TenantID:    message.GetTenantId(),
			ID:          message.GetPrincipalId(),
			DisplayName: message.GetPrincipalDisplayName(),
			IsBot:       message.GetPrincipalIsBot(),
			Claims:      claims,
		},
		Conversation: gateway.ConversationRef{
			Platform:      platform,
			AccountID:     message.GetAccountId(),
			TenantID:      message.GetTenantId(),
			ChannelID:     message.GetChannelId(),
			ThreadID:      message.GetThreadId(),
			RootMessageID: message.GetRootMessageId(),
		},
		Text:       message.GetText(),
		Trigger:    triggerFromProto(message.GetTrigger()),
		OccurredAt: message.GetOccurredAt().AsTime(),
	}
}

// triggerFromProto narrows anything unrecognised to ambient.
//
// A trigger this daemon does not understand must not be treated as an explicit
// request: the safe reading of an unknown value is the one that does nothing.
func triggerFromProto(trigger controlv1.MessageTrigger) gateway.Trigger {
	switch trigger {
	case controlv1.MessageTrigger_MESSAGE_TRIGGER_MENTION:
		return gateway.TriggerMention
	case controlv1.MessageTrigger_MESSAGE_TRIGGER_COMMAND:
		return gateway.TriggerCommand
	case controlv1.MessageTrigger_MESSAGE_TRIGGER_DIRECT:
		return gateway.TriggerDirect
	default:
		return gateway.TriggerAmbient
	}
}

func dispatchToProto(dispatch gateway.Dispatch) *controlv1.Dispatch {
	return &controlv1.Dispatch{
		Id:        dispatch.ID,
		Seq:       uint64(dispatch.Seq),
		SessionId: string(dispatch.SessionID),
		RunId:     string(dispatch.RunID),
		// The conversation travels encoded: only the adapter that produced it
		// knows how to address the platform it names.
		Target:    dispatch.Target.DeliveryTarget().Ref,
		Kind:      dispatchKindToProto(dispatch.Kind),
		Payload:   dispatch.Payload,
		CreatedAt: timestamppb.New(dispatch.CreatedAt),
	}
}

func dispatchKindToProto(kind gateway.DispatchKind) controlv1.DispatchKind {
	switch kind {
	case gateway.DispatchMessage:
		return controlv1.DispatchKind_DISPATCH_KIND_MESSAGE
	case gateway.DispatchApproval:
		return controlv1.DispatchKind_DISPATCH_KIND_APPROVAL
	case gateway.DispatchStatus:
		return controlv1.DispatchKind_DISPATCH_KIND_STATUS
	default:
		return controlv1.DispatchKind_DISPATCH_KIND_UNSPECIFIED
	}
}

// gatewayError maps refusals onto codes a caller can act on.
//
// A refusal is not a malfunction: a message from an unbound channel or an
// unlisted account is the system working, and the gateway should say so in the
// channel rather than retry.
func gatewayError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gateway.ErrBindingNotFound):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, gateway.ErrNotPermitted), errors.Is(err, gateway.ErrNotExplicit):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, gateway.ErrDispatchNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, gateway.ErrAlreadyDelivered):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return toConnectError(err)
	}
}
