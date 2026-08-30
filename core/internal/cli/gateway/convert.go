package gateway

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// Translation between the wire types and the gateway vocabulary lives here so
// the adapter never sees protobuf and the RPC layer never sees Discord.

func inboundToProto(message gateway.InboundMessage) *controlv1.InboundMessage {
	claims := make([]*controlv1.PrincipalClaim, 0, len(message.Principal.Claims))
	for _, claim := range message.Principal.Claims {
		claims = append(claims, &controlv1.PrincipalClaim{
			Namespace: claim.Namespace,
			Value:     claim.Value,
		})
	}

	return &controlv1.InboundMessage{
		Platform:             string(message.Conversation.Platform),
		AccountId:            message.Conversation.AccountID,
		TenantId:             message.Conversation.TenantID,
		ChannelId:            message.Conversation.ChannelID,
		ThreadId:             message.Conversation.ThreadID,
		RootMessageId:        message.Conversation.RootMessageID,
		PlatformMessageId:    message.PlatformMessageID,
		IdempotencyKey:       message.IdempotencyKey,
		PrincipalId:          message.Principal.ID,
		PrincipalDisplayName: message.Principal.DisplayName,
		PrincipalIsBot:       message.Principal.IsBot,
		PrincipalClaims:      claims,
		Text:                 message.Text,
		Trigger:              triggerToProto(message.Trigger),
		OccurredAt:           timestamppb.New(message.OccurredAt),
	}
}

func triggerToProto(trigger gateway.Trigger) controlv1.MessageTrigger {
	switch trigger {
	case gateway.TriggerMention:
		return controlv1.MessageTrigger_MESSAGE_TRIGGER_MENTION
	case gateway.TriggerCommand:
		return controlv1.MessageTrigger_MESSAGE_TRIGGER_COMMAND
	case gateway.TriggerDirect:
		return controlv1.MessageTrigger_MESSAGE_TRIGGER_DIRECT
	default:
		return controlv1.MessageTrigger_MESSAGE_TRIGGER_AMBIENT
	}
}

func dispatchFromProto(dispatch *controlv1.Dispatch) (gateway.Dispatch, error) {
	var target gateway.ConversationRef
	if err := json.Unmarshal([]byte(dispatch.GetTarget()), &target); err != nil {
		return gateway.Dispatch{}, fmt.Errorf("gateway: unusable delivery target: %w", err)
	}

	return gateway.Dispatch{
		ID:        dispatch.GetId(),
		Seq:       gateway.DispatchSeq(dispatch.GetSeq()),
		SessionID: domain.SessionID(dispatch.GetSessionId()),
		RunID:     domain.RunID(dispatch.GetRunId()),
		Target:    target,
		Kind:      dispatchKindFromProto(dispatch.GetKind()),
		Payload:   dispatch.GetPayload(),
		CreatedAt: dispatch.GetCreatedAt().AsTime(),
	}, nil
}

func dispatchKindFromProto(kind controlv1.DispatchKind) gateway.DispatchKind {
	switch kind {
	case controlv1.DispatchKind_DISPATCH_KIND_MESSAGE:
		return gateway.DispatchMessage
	case controlv1.DispatchKind_DISPATCH_KIND_APPROVAL:
		return gateway.DispatchApproval
	case controlv1.DispatchKind_DISPATCH_KIND_STATUS:
		return gateway.DispatchStatus
	default:
		return ""
	}
}
