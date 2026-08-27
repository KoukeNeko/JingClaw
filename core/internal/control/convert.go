package control

import (
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
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

func sessionToProto(s *domain.Session) *controlv1.Session {
	return &controlv1.Session{
		Id:        string(s.ID),
		Title:     s.Title,
		CreatedAt: timestamppb.New(s.CreatedAt),
		UpdatedAt: timestamppb.New(s.UpdatedAt),
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
				MessageId: string(p.MessageID),
				Text:      p.Text,
				Trust:     trustToProto(p.Trust),
				Origin:    originToProto(p.Origin),
			},
		}

	case domain.RunStateChanged:
		out.Payload = &controlv1.Event_RunStateChanged{
			RunStateChanged: &controlv1.RunStateChanged{
				Status: runStatusToProto(p.Status),
				Reason: p.Reason,
			},
		}

	case domain.AssistantTextDelta:
		out.Payload = &controlv1.Event_AssistantTextDelta{
			AssistantTextDelta: &controlv1.AssistantTextDelta{
				MessageId: string(p.MessageID),
				Text:      p.Text,
			},
		}

	case domain.AssistantMessageCompleted:
		out.Payload = &controlv1.Event_AssistantMessageCompleted{
			AssistantMessageCompleted: &controlv1.AssistantMessageCompleted{
				MessageId: string(p.MessageID),
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
