package control

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
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

// WithdrawInbound carries a message being taken back inward.
//
// The caller says who and which message. Whether that person sent it, and
// whether it is still waiting to be answered, is settled here.
func (s *GatewayServer) WithdrawInbound(
	ctx context.Context,
	req *connect.Request[controlv1.WithdrawInboundRequest],
) (*connect.Response[controlv1.WithdrawInboundResponse], error) {
	message := req.Msg
	if message.GetPrincipalId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("principal_id is required"))
	}
	if message.GetIdempotencyKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key is required"))
	}

	withdrawn, err := s.ingress.Withdraw(ctx, gateway.Withdrawal{
		Principal: gateway.Principal{
			Platform:  gateway.Platform(message.GetPlatform()),
			AccountID: message.GetAccountId(),
			TenantID:  message.GetTenantId(),
			ID:        message.GetPrincipalId(),
		},
		InboundKey: message.GetIdempotencyKey(),
		MessageID:  message.GetPlatformMessageId(),
	})
	if err != nil {
		return nil, gatewayError(err)
	}

	return connect.NewResponse(&controlv1.WithdrawInboundResponse{Withdrawn: withdrawn}), nil
}

// DeliverDecision carries a button press inward.
//
// What the caller is trusted to say is who pressed and where, because that is
// what its platform told it. Whether that person may decide anything is
// settled below against the channel's binding: a gateway that could assert
// its own authority would be a bot token that can approve.
func (s *GatewayServer) DeliverDecision(
	ctx context.Context,
	req *connect.Request[controlv1.DeliverDecisionRequest],
) (*connect.Response[controlv1.DeliverDecisionResponse], error) {
	message := req.Msg
	if message.GetApprovalId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("approval_id is required"))
	}
	if message.GetPrincipalId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("principal_id is required"))
	}

	outcome, err := s.ingress.Decide(ctx, gateway.ApprovalDecision{
		Principal: gateway.Principal{
			Platform:    gateway.Platform(message.GetPlatform()),
			AccountID:   message.GetAccountId(),
			TenantID:    message.GetTenantId(),
			ID:          message.GetPrincipalId(),
			DisplayName: message.GetPrincipalDisplayName(),
			IsBot:       message.GetPrincipalIsBot(),
			Claims:      claimsFromProto(message.GetPrincipalClaims()),
		},
		Conversation: gateway.ConversationRef{
			Platform:  gateway.Platform(message.GetPlatform()),
			AccountID: message.GetAccountId(),
			TenantID:  message.GetTenantId(),
			ChannelID: message.GetChannelId(),
		},
		ApprovalID: domain.ApprovalID(message.GetApprovalId()),
		Allow:      message.GetAllow(),
	})
	if err != nil {
		return nil, gatewayError(err)
	}

	return connect.NewResponse(&controlv1.DeliverDecisionResponse{
		Outcome: outcomeToProto(outcome),
	}), nil
}

func outcomeToProto(outcome gateway.DecisionOutcome) controlv1.DecisionOutcome {
	switch outcome {
	case gateway.DecisionRecorded:
		return controlv1.DecisionOutcome_DECISION_OUTCOME_RECORDED
	case gateway.DecisionAlready:
		return controlv1.DecisionOutcome_DECISION_OUTCOME_ALREADY
	case gateway.DecisionUnavailable:
		return controlv1.DecisionOutcome_DECISION_OUTCOME_UNAVAILABLE
	default:
		// Refused covers "not an approver here" and "no such approval", which
		// are one value on the wire on purpose.
		return controlv1.DecisionOutcome_DECISION_OUTCOME_REFUSED
	}
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

// claimsFromProto carries a principal's claims across unchanged.
//
// Opaque on purpose: a Discord role and a GitHub collaborator status are not
// the same kind of thing, and giving them a shared meaning here would invent a
// hierarchy nobody wrote down.
func claimsFromProto(claims []*controlv1.PrincipalClaim) []gateway.Claim {
	carried := make([]gateway.Claim, 0, len(claims))
	for _, claim := range claims {
		carried = append(carried, gateway.Claim{
			Namespace: claim.GetNamespace(),
			Value:     claim.GetValue(),
		})
	}
	return carried
}

func inboundFromProto(message *controlv1.InboundMessage) gateway.InboundMessage {
	claims := claimsFromProto(message.GetPrincipalClaims())

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
		Text:        message.GetText(),
		Trigger:     triggerFromProto(message.GetTrigger()),
		OccurredAt:  message.GetOccurredAt().AsTime(),
		Attachments: inboundAttachmentsFromProto(message.GetAttachments()),
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
	case gateway.DispatchQuestion:
		return controlv1.DispatchKind_DISPATCH_KIND_QUESTION
	case gateway.DispatchLog:
		return controlv1.DispatchKind_DISPATCH_KIND_LOG
	default:
		// Reaching here is a kind the wire does not know. It is not a
		// dispatch that can be delivered: the far side reads UNSPECIFIED as
		// an empty kind and refuses to render it, so it sits in the outbox
		// and is offered again on every reconnect, forever. The test over
		// AllDispatchKinds is what keeps this branch unreachable.
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

// ChannelServer manages bindings.
//
// It is spoken by an operator's control client, not by a gateway. Which
// channels may reach which workspace is the operator's decision, and letting
// the gateway assert it about itself would make the allowlist meaningless.
type ChannelServer struct {
	store gateway.Store
	newID func() string
	now   func() time.Time
}

func NewChannelServer(store gateway.Store, newID func() string, now func() time.Time) *ChannelServer {
	if now == nil {
		now = time.Now
	}
	return &ChannelServer{store: store, newID: newID, now: now}
}

func (s *ChannelServer) ListBindings(
	ctx context.Context,
	_ *connect.Request[controlv1.ListBindingsRequest],
) (*connect.Response[controlv1.ListBindingsResponse], error) {
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return nil, gatewayError(err)
	}

	out := make([]*controlv1.Binding, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, bindingToProto(binding))
	}

	return connect.NewResponse(&controlv1.ListBindingsResponse{Bindings: out}), nil
}

func (s *ChannelServer) UpsertBinding(
	ctx context.Context,
	req *connect.Request[controlv1.UpsertBindingRequest],
) (*connect.Response[controlv1.UpsertBindingResponse], error) {
	incoming := req.Msg.GetBinding()
	if incoming == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("binding is required"))
	}
	if incoming.GetPlatform() == "" || incoming.GetChannelId() == "" || incoming.GetWorkspaceId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("platform, channel_id and workspace_id are required"))
	}

	binding := bindingFromProto(incoming)
	if binding.ID == "" {
		binding.ID = s.newID()
	}
	if binding.PermissionProfile == "" {
		// Traffic from a channel gets the strict profile unless an operator
		// deliberately says otherwise.
		binding.PermissionProfile = "gateway"
	}
	binding.CreatedAt = s.now()

	if err := s.store.UpsertBinding(ctx, binding); err != nil {
		return nil, gatewayError(err)
	}

	return connect.NewResponse(&controlv1.UpsertBindingResponse{
		Binding: bindingToProto(binding),
	}), nil
}

func (s *ChannelServer) DeleteBinding(
	ctx context.Context,
	req *connect.Request[controlv1.DeleteBindingRequest],
) (*connect.Response[controlv1.DeleteBindingResponse], error) {
	if err := s.store.DeleteBinding(ctx, req.Msg.GetBindingId()); err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&controlv1.DeleteBindingResponse{}), nil
}

func bindingToProto(binding gateway.Binding) *controlv1.Binding {
	claims := make([]*controlv1.PrincipalClaim, 0, len(binding.AllowedClaims))
	for _, claim := range binding.AllowedClaims {
		claims = append(claims, &controlv1.PrincipalClaim{Namespace: claim.Namespace, Value: claim.Value})
	}

	return &controlv1.Binding{
		Id:                binding.ID,
		Platform:          string(binding.Platform),
		AccountId:         binding.AccountID,
		TenantId:          binding.TenantID,
		ChannelId:         binding.ChannelID,
		WorkspaceId:       binding.WorkspaceID,
		PermissionProfile: binding.PermissionProfile,
		AllowedPrincipals: binding.AllowedPrincipals,
		AllowedClaims:     claims,
		CreatedAt:         timestamppb.New(binding.CreatedAt),
	}
}

func bindingFromProto(binding *controlv1.Binding) gateway.Binding {
	claims := make([]gateway.Claim, 0, len(binding.GetAllowedClaims()))
	for _, claim := range binding.GetAllowedClaims() {
		claims = append(claims, gateway.Claim{Namespace: claim.GetNamespace(), Value: claim.GetValue()})
	}

	return gateway.Binding{
		ID:                binding.GetId(),
		Platform:          gateway.Platform(binding.GetPlatform()),
		AccountID:         binding.GetAccountId(),
		TenantID:          binding.GetTenantId(),
		ChannelID:         binding.GetChannelId(),
		WorkspaceID:       binding.GetWorkspaceId(),
		PermissionProfile: binding.GetPermissionProfile(),
		AllowedPrincipals: binding.GetAllowedPrincipals(),
		AllowedClaims:     claims,
	}
}

// inboundAttachmentsFromProto is the files a message arrived with.
//
// Named apart from the one in fromproto.go, which converts what the daemon
// already stored. These are on their way in and still carry their bytes.
func inboundAttachmentsFromProto(
	attachments []*controlv1.InboundAttachment,
) []gateway.Attachment {
	if len(attachments) == 0 {
		return nil
	}

	converted := make([]gateway.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		converted = append(converted, gateway.Attachment{
			ID:          attachment.GetId(),
			Name:        attachment.GetName(),
			ContentType: attachment.GetContentType(),
			Size:        attachment.GetSize(),
			Data:        attachment.GetData(),
		})
	}
	return converted
}
