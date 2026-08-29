package control

import (
	"time"

	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

// This file is the only place where domain types and wire types meet. Keeping
// the mapping explicit (rather than sharing one struct) is what allows the
// protocol and the runtime to evolve at different speeds.

func runStatusToProto(s domain.RunStatus) controlv1.RunStatus {
	switch s {
	case domain.RunQueued:
		return controlv1.RunStatus_RUN_STATUS_QUEUED
	case domain.RunRunning:
		return controlv1.RunStatus_RUN_STATUS_RUNNING
	case domain.RunAwaitingApproval:
		return controlv1.RunStatus_RUN_STATUS_AWAITING_APPROVAL
	case domain.RunAwaitingInput:
		return controlv1.RunStatus_RUN_STATUS_AWAITING_INPUT
	case domain.RunCancelling:
		return controlv1.RunStatus_RUN_STATUS_CANCELLING
	case domain.RunCompleted:
		return controlv1.RunStatus_RUN_STATUS_COMPLETED
	case domain.RunCancelled:
		return controlv1.RunStatus_RUN_STATUS_CANCELLED
	case domain.RunFailed:
		return controlv1.RunStatus_RUN_STATUS_FAILED
	default:
		return controlv1.RunStatus_RUN_STATUS_UNSPECIFIED
	}
}

func trustToProto(t domain.TrustLevel) controlv1.TrustLevel {
	switch t {
	case domain.TrustUntrusted:
		return controlv1.TrustLevel_TRUST_LEVEL_UNTRUSTED
	case domain.TrustUser:
		return controlv1.TrustLevel_TRUST_LEVEL_USER
	case domain.TrustWorkspace:
		return controlv1.TrustLevel_TRUST_LEVEL_WORKSPACE
	case domain.TrustSystem:
		return controlv1.TrustLevel_TRUST_LEVEL_SYSTEM
	default:
		return controlv1.TrustLevel_TRUST_LEVEL_UNSPECIFIED
	}
}

func originToProto(o domain.RunOrigin) *controlv1.RunOrigin {
	out := &controlv1.RunOrigin{ClientId: o.ClientID}

	switch o.Kind {
	case domain.OriginLocalClient:
		out.Kind = controlv1.RunOriginKind_RUN_ORIGIN_KIND_LOCAL_CLIENT
	case domain.OriginGateway:
		out.Kind = controlv1.RunOriginKind_RUN_ORIGIN_KIND_GATEWAY
	default:
		out.Kind = controlv1.RunOriginKind_RUN_ORIGIN_KIND_UNSPECIFIED
	}

	if o.Principal != nil {
		out.Principal = &controlv1.ExternalPrincipal{
			Platform:    o.Principal.Platform,
			AccountId:   o.Principal.AccountID,
			TenantId:    o.Principal.TenantID,
			PrincipalId: o.Principal.PrincipalID,
			DisplayName: o.Principal.DisplayName,
		}
	}

	return out
}

func stopReasonToProto(reason domain.StopReason) controlv1.StopReason {
	switch reason {
	case domain.StopEndTurn:
		return controlv1.StopReason_STOP_REASON_END_TURN
	case domain.StopMaxTokens:
		return controlv1.StopReason_STOP_REASON_MAX_TOKENS
	case domain.StopContentFilter:
		return controlv1.StopReason_STOP_REASON_CONTENT_FILTER
	case domain.StopCancelled:
		return controlv1.StopReason_STOP_REASON_CANCELLED
	case domain.StopError:
		return controlv1.StopReason_STOP_REASON_ERROR
	case domain.StopToolUse:
		return controlv1.StopReason_STOP_REASON_TOOL_USE
	default:
		return controlv1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

func approvalStatusToProto(status domain.ApprovalStatus) controlv1.ApprovalStatus {
	switch status {
	case domain.ApprovalPending:
		return controlv1.ApprovalStatus_APPROVAL_STATUS_PENDING
	case domain.ApprovalAllowed:
		return controlv1.ApprovalStatus_APPROVAL_STATUS_ALLOWED
	case domain.ApprovalDenied:
		return controlv1.ApprovalStatus_APPROVAL_STATUS_DENIED
	default:
		return controlv1.ApprovalStatus_APPROVAL_STATUS_UNSPECIFIED
	}
}

func rememberScopeToProto(scope domain.RememberScope) controlv1.RememberScope {
	switch scope {
	case domain.RememberOnce:
		return controlv1.RememberScope_REMEMBER_SCOPE_ONCE
	case domain.RememberSession:
		return controlv1.RememberScope_REMEMBER_SCOPE_SESSION
	default:
		return controlv1.RememberScope_REMEMBER_SCOPE_UNSPECIFIED
	}
}

func rememberScopeFromProto(scope controlv1.RememberScope) domain.RememberScope {
	if scope == controlv1.RememberScope_REMEMBER_SCOPE_SESSION {
		return domain.RememberSession
	}
	// Anything unrecognised narrows to a single use. A scope the daemon does
	// not understand must never be treated as broader than it is.
	return domain.RememberOnce
}

func approvalToProto(a domain.Approval) *controlv1.Approval {
	return &controlv1.Approval{
		Id:         string(a.ID),
		SessionId:  string(a.SessionID),
		RunId:      string(a.RunID),
		ToolCallId: string(a.ToolCallID),
		ToolName:   a.ToolName,
		Arguments:  a.Arguments,
		Summary:    a.Summary,
		Effects:    a.Effects,
		Status:     approvalStatusToProto(a.Status),
		CreatedAt:  timestamppb.New(a.CreatedAt),
	}
}

func usageToProto(usage domain.Usage) *controlv1.Usage {
	return &controlv1.Usage{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		ReasoningTokens:   usage.ReasoningTokens,
	}
}

func sessionToProto(s domain.Session) *controlv1.Session {
	return &controlv1.Session{
		Id:        string(s.ID),
		Title:     s.Title,
		CreatedAt: timestamppb.New(s.CreatedAt),
		UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
}

// runToProto is for a client that is looking at a session it did not start.
//
// Delivery targets are left out on purpose: they say where a reply is going,
// which for a gateway run means a channel on somebody else's platform, and a
// listing does not need it.
func runToProto(r domain.Run) *controlv1.Run {
	converted := &controlv1.Run{
		Id:        string(r.ID),
		SessionId: string(r.SessionID),
		Status:    runStatusToProto(r.Status),
		Origin:    originToProto(r.Origin),
		CreatedAt: timestamppb.New(r.CreatedAt),
	}
	if r.FinishedAt != nil {
		converted.FinishedAt = timestamppb.New(*r.FinishedAt)
	}
	return converted
}

func attachmentsToProto(attachments []domain.Attachment) []*controlv1.MessageAttachment {
	if len(attachments) == 0 {
		return nil
	}

	converted := make([]*controlv1.MessageAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		converted = append(converted, &controlv1.MessageAttachment{
			ArtifactId: attachment.ArtifactID,
			Name:       attachment.Name,
			MediaType:  attachment.MediaType,
			Size:       attachment.Size,
		})
	}
	return converted
}

func artifactToProto(ref *domain.Artifact) *controlv1.Artifact {
	if ref == nil {
		return nil
	}
	return &controlv1.Artifact{
		Id:        ref.ID,
		Size:      ref.Size,
		MediaType: ref.MediaType,
	}
}

func eventToProto(ev domain.Event) (*controlv1.Event, error) {
	out := &controlv1.Event{
		Id:         string(ev.ID),
		SessionId:  string(ev.SessionID),
		RunId:      string(ev.RunID),
		Seq:        uint64(ev.Seq),
		OccurredAt: timestamppb.New(ev.OccurredAt),
	}

	switch p := ev.Payload.(type) {
	case domain.UserMessageAdded:
		out.Payload = &controlv1.Event_UserMessageAdded{
			UserMessageAdded: &controlv1.UserMessageAdded{
				MessageId:   string(p.MessageID),
				Text:        p.Text,
				Trust:       trustToProto(p.Trust),
				Origin:      originToProto(p.Origin),
				Attachments: attachmentsToProto(p.Attachments),
			},
		}

	case domain.RunStateChanged:
		out.Payload = &controlv1.Event_RunStateChanged{
			RunStateChanged: &controlv1.RunStateChanged{
				Status:      runStatusToProto(p.Status),
				Reason:      p.Reason,
				FailureKind: p.FailureKind,
			},
		}

	case domain.AssistantTextDelta:
		out.Payload = &controlv1.Event_AssistantTextDelta{
			AssistantTextDelta: &controlv1.AssistantTextDelta{
				MessageId: string(p.MessageID),
				Text:      p.Text,
			},
		}

	case domain.AssistantReasoningDelta:
		out.Payload = &controlv1.Event_AssistantReasoningDelta{
			AssistantReasoningDelta: &controlv1.AssistantReasoningDelta{
				MessageId: string(p.MessageID),
				Text:      p.Text,
			},
		}

	case domain.AssistantMessageCompleted:
		out.Payload = &controlv1.Event_AssistantMessageCompleted{
			AssistantMessageCompleted: &controlv1.AssistantMessageCompleted{
				MessageId:  string(p.MessageID),
				StopReason: stopReasonToProto(p.StopReason),
			},
		}

	case domain.ToolCallRequested:
		out.Payload = &controlv1.Event_ToolCallRequested{
			ToolCallRequested: &controlv1.ToolCallRequested{
				CallId:    string(p.CallID),
				Name:      p.Name,
				Arguments: p.Arguments,
			},
		}

	case domain.ToolCallCompleted:
		out.Payload = &controlv1.Event_ToolCallCompleted{
			ToolCallCompleted: &controlv1.ToolCallCompleted{
				CallId:     string(p.CallID),
				Name:       p.Name,
				Summary:    p.Summary,
				Content:    p.Content,
				IsError:    p.IsError,
				Truncated:  p.Truncated,
				Artifact:   artifactToProto(p.Artifact),
				DurationMs: p.DurationMS,
			},
		}

	case domain.ApprovalRequested:
		out.Payload = &controlv1.Event_ApprovalRequested{
			ApprovalRequested: &controlv1.ApprovalRequested{
				ApprovalId: string(p.ApprovalID),
				CallId:     string(p.CallID),
				ToolName:   p.ToolName,
				Arguments:  p.Arguments,
				Summary:    p.Summary,
				Effects:    p.Effects,
			},
		}

	case domain.ApprovalResolved:
		out.Payload = &controlv1.Event_ApprovalResolved{
			ApprovalResolved: &controlv1.ApprovalResolved{
				ApprovalId: string(p.ApprovalID),
				CallId:     string(p.CallID),
				ToolName:   p.ToolName,
				Status:     approvalStatusToProto(p.Status),
				Scope:      rememberScopeToProto(p.Scope),
				DecidedBy:  p.DecidedBy,
			},
		}

	case domain.UsageChanged:
		out.Payload = &controlv1.Event_UsageChanged{
			UsageChanged: &controlv1.UsageChanged{Usage: usageToProto(p.Usage)},
		}

	case domain.RunDirections:
		out.Payload = &controlv1.Event_RunDirections{
			RunDirections: &controlv1.RunDirections{Text: p.Text},
		}

	case domain.ConversationCompacted:
		out.Payload = &controlv1.Event_ConversationCompacted{
			ConversationCompacted: &controlv1.ConversationCompacted{
				Summary:        p.Summary,
				ThroughSeq:     uint64(p.ThroughSeq),
				MessagesFolded: int32(p.MessagesFolded),
				TokensBefore:   p.TokensBefore,
				TokensAfter:    p.TokensAfter,
			},
		}

	default:
		// A payload the wire format cannot express must fail loudly. Silently
		// shipping an event with no payload would corrupt every client's
		// materialized view in a way that is very hard to trace back.
		return nil, fmt.Errorf("control: unhandled event payload %T", ev.Payload)
	}

	return out, nil
}

// sessionViewToProto renders a session view for the wire.
func sessionViewToProto(view runtime.SessionView) *controlv1.GetSessionViewResponse {
	out := &controlv1.GetSessionViewResponse{
		Session:   sessionToProto(view.Session),
		HeadSeq:   uint64(view.HeadSeq),
		Truncated: view.Truncated,
	}

	out.Messages = make([]*controlv1.ViewMessage, 0, len(view.Messages))
	for _, message := range view.Messages {
		out.Messages = append(out.Messages, viewMessageToProto(message))
	}

	out.PendingApprovals = make([]*controlv1.Approval, 0, len(view.Pending))
	for _, approval := range view.Pending {
		out.PendingApprovals = append(out.PendingApprovals, approvalToProto(approval))
	}

	if view.ActiveRun != nil {
		out.ActiveRun = runToProto(*view.ActiveRun)
	}

	return out
}

func viewMessageToProto(message runtime.ViewMessage) *controlv1.ViewMessage {
	out := &controlv1.ViewMessage{
		Id:        string(message.ID),
		Role:      messageRoleToProto(message.Role),
		Text:      message.Text,
		Reasoning: message.Reasoning,
		At:        timestamppb.New(time.Unix(0, message.At)),
		Seq:       uint64(message.Seq),
	}

	out.ToolCalls = make([]*controlv1.ViewToolCall, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &controlv1.ViewToolCall{
			CallId:    string(call.CallID),
			Name:      call.Name,
			Summary:   call.Summary,
			Completed: call.Completed,
			IsError:   call.IsError,
		})
	}

	return out
}

func messageRoleToProto(role domain.MessageRole) controlv1.MessageRole {
	switch role {
	case domain.RoleUser:
		return controlv1.MessageRole_MESSAGE_ROLE_USER
	case domain.RoleAssistant:
		return controlv1.MessageRole_MESSAGE_ROLE_ASSISTANT
	default:
		return controlv1.MessageRole_MESSAGE_ROLE_UNSPECIFIED
	}
}
