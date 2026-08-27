package storage

import (
	"encoding/json"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Event payloads are stored as JSON rather than given struct tags in the
// domain package, so the persisted shape is an explicit decision here instead
// of an accident of how the domain types happen to be written. The same
// reasoning keeps protobuf translation confined to internal/control.
//
// These field names are on-disk format. Renaming one is a migration.

type userMessageAddedJSON struct {
	MessageID string        `json:"message_id"`
	Text      string        `json:"text"`
	Trust     string        `json:"trust"`
	Origin    runOriginJSON `json:"origin"`
}

type runStateChangedJSON struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type assistantTextDeltaJSON struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

type assistantMessageCompletedJSON struct {
	MessageID  string `json:"message_id"`
	StopReason string `json:"stop_reason,omitempty"`
}

type toolCallRequestedJSON struct {
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ProviderMetadata string `json:"provider_metadata,omitempty"`
}

type toolCallCompletedJSON struct {
	CallID     string `json:"call_id"`
	Name       string `json:"name"`
	Summary    string `json:"summary,omitempty"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type approvalRequestedJSON struct {
	ApprovalID string   `json:"approval_id"`
	CallID     string   `json:"call_id"`
	ToolName   string   `json:"tool_name"`
	Arguments  string   `json:"arguments"`
	Summary    string   `json:"summary,omitempty"`
	Effects    []string `json:"effects,omitempty"`
}

type approvalResolvedJSON struct {
	ApprovalID string `json:"approval_id"`
	CallID     string `json:"call_id"`
	ToolName   string `json:"tool_name"`
	Status     string `json:"status"`
	Scope      string `json:"scope,omitempty"`
	DecidedBy  string `json:"decided_by,omitempty"`
}

type conversationCompactedJSON struct {
	Summary        string `json:"summary"`
	ThroughSeq     uint64 `json:"through_seq"`
	MessagesFolded int    `json:"messages_folded,omitempty"`
	TokensBefore   int64  `json:"tokens_before,omitempty"`
	TokensAfter    int64  `json:"tokens_after,omitempty"`
}

type usageChangedJSON struct {
	InputTokens       int64 `json:"input_tokens,omitempty"`
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64 `json:"output_tokens,omitempty"`
	ReasoningTokens   int64 `json:"reasoning_tokens,omitempty"`
}

type runOriginJSON struct {
	Kind      string             `json:"kind"`
	ClientID  string             `json:"client_id,omitempty"`
	Principal *externalPrincipal `json:"principal,omitempty"`
}

type externalPrincipal struct {
	Platform    string `json:"platform"`
	AccountID   string `json:"account_id"`
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	DisplayName string `json:"display_name,omitempty"`
}

// EncodePayload serializes an event payload for storage.
func EncodePayload(payload domain.EventPayload) ([]byte, error) {
	switch p := payload.(type) {
	case domain.UserMessageAdded:
		return json.Marshal(userMessageAddedJSON{
			MessageID: string(p.MessageID),
			Text:      p.Text,
			Trust:     string(p.Trust),
			Origin:    encodeOrigin(p.Origin),
		})

	case domain.RunStateChanged:
		return json.Marshal(runStateChangedJSON{
			Status: string(p.Status),
			Reason: p.Reason,
		})

	case domain.AssistantTextDelta:
		return json.Marshal(assistantTextDeltaJSON{
			MessageID: string(p.MessageID),
			Text:      p.Text,
		})

	case domain.AssistantMessageCompleted:
		return json.Marshal(assistantMessageCompletedJSON{
			MessageID:  string(p.MessageID),
			StopReason: string(p.StopReason),
		})

	case domain.ToolCallRequested:
		return json.Marshal(toolCallRequestedJSON{
			CallID:           string(p.CallID),
			Name:             p.Name,
			Arguments:        p.Arguments,
			ProviderMetadata: p.ProviderMetadata,
		})

	case domain.ToolCallCompleted:
		return json.Marshal(toolCallCompletedJSON{
			CallID:     string(p.CallID),
			Name:       p.Name,
			Summary:    p.Summary,
			Content:    p.Content,
			IsError:    p.IsError,
			Truncated:  p.Truncated,
			DurationMS: p.DurationMS,
		})

	case domain.ConversationCompacted:
		return json.Marshal(conversationCompactedJSON{
			Summary:        p.Summary,
			ThroughSeq:     uint64(p.ThroughSeq),
			MessagesFolded: p.MessagesFolded,
			TokensBefore:   p.TokensBefore,
			TokensAfter:    p.TokensAfter,
		})

	case domain.ApprovalRequested:
		return json.Marshal(approvalRequestedJSON{
			ApprovalID: string(p.ApprovalID),
			CallID:     string(p.CallID),
			ToolName:   p.ToolName,
			Arguments:  p.Arguments,
			Summary:    p.Summary,
			Effects:    p.Effects,
		})

	case domain.ApprovalResolved:
		return json.Marshal(approvalResolvedJSON{
			ApprovalID: string(p.ApprovalID),
			CallID:     string(p.CallID),
			ToolName:   p.ToolName,
			Status:     string(p.Status),
			Scope:      string(p.Scope),
			DecidedBy:  p.DecidedBy,
		})

	case domain.UsageChanged:
		return json.Marshal(usageChangedJSON{
			InputTokens:       p.Usage.InputTokens,
			CachedInputTokens: p.Usage.CachedInputTokens,
			OutputTokens:      p.Usage.OutputTokens,
			ReasoningTokens:   p.Usage.ReasoningTokens,
		})

	default:
		// Writing an event the log cannot read back would corrupt history
		// silently, so this fails loudly instead.
		return nil, fmt.Errorf("storage: unhandled event payload %T", payload)
	}
}

// DecodePayload reconstructs an event payload from storage.
func DecodePayload(kind domain.EventKind, raw []byte) (domain.EventPayload, error) {
	switch kind {
	case domain.EventUserMessageAdded:
		var p userMessageAddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.UserMessageAdded{
			MessageID: domain.MessageID(p.MessageID),
			Text:      p.Text,
			Trust:     domain.TrustLevel(p.Trust),
			Origin:    decodeOrigin(p.Origin),
		}, nil

	case domain.EventRunStateChanged:
		var p runStateChangedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.RunStateChanged{
			Status: domain.RunStatus(p.Status),
			Reason: p.Reason,
		}, nil

	case domain.EventAssistantTextDelta:
		var p assistantTextDeltaJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.AssistantTextDelta{
			MessageID: domain.MessageID(p.MessageID),
			Text:      p.Text,
		}, nil

	case domain.EventAssistantMessageCompleted:
		var p assistantMessageCompletedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.AssistantMessageCompleted{
			MessageID:  domain.MessageID(p.MessageID),
			StopReason: domain.StopReason(p.StopReason),
		}, nil

	case domain.EventToolCallRequested:
		var p toolCallRequestedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ToolCallRequested{
			CallID:           domain.ToolCallID(p.CallID),
			Name:             p.Name,
			Arguments:        p.Arguments,
			ProviderMetadata: p.ProviderMetadata,
		}, nil

	case domain.EventToolCallCompleted:
		var p toolCallCompletedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ToolCallCompleted{
			CallID:     domain.ToolCallID(p.CallID),
			Name:       p.Name,
			Summary:    p.Summary,
			Content:    p.Content,
			IsError:    p.IsError,
			Truncated:  p.Truncated,
			DurationMS: p.DurationMS,
		}, nil

	case domain.EventConversationCompacted:
		var p conversationCompactedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ConversationCompacted{
			Summary:        p.Summary,
			ThroughSeq:     domain.Seq(p.ThroughSeq),
			MessagesFolded: p.MessagesFolded,
			TokensBefore:   p.TokensBefore,
			TokensAfter:    p.TokensAfter,
		}, nil

	case domain.EventApprovalRequested:
		var p approvalRequestedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ApprovalRequested{
			ApprovalID: domain.ApprovalID(p.ApprovalID),
			CallID:     domain.ToolCallID(p.CallID),
			ToolName:   p.ToolName,
			Arguments:  p.Arguments,
			Summary:    p.Summary,
			Effects:    p.Effects,
		}, nil

	case domain.EventApprovalResolved:
		var p approvalResolvedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ApprovalResolved{
			ApprovalID: domain.ApprovalID(p.ApprovalID),
			CallID:     domain.ToolCallID(p.CallID),
			ToolName:   p.ToolName,
			Status:     domain.ApprovalStatus(p.Status),
			Scope:      domain.RememberScope(p.Scope),
			DecidedBy:  p.DecidedBy,
		}, nil

	case domain.EventUsageChanged:
		var p usageChangedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.UsageChanged{Usage: domain.Usage{
			InputTokens:       p.InputTokens,
			CachedInputTokens: p.CachedInputTokens,
			OutputTokens:      p.OutputTokens,
			ReasoningTokens:   p.ReasoningTokens,
		}}, nil

	default:
		return nil, fmt.Errorf("storage: unknown event kind %q", kind)
	}
}

// EncodeOrigin and DecodeOrigin are exported for the runs table, which stores
// origin as a column rather than inside an event payload.
func EncodeOrigin(origin domain.RunOrigin) ([]byte, error) {
	return json.Marshal(encodeOrigin(origin))
}

func DecodeOrigin(raw []byte) (domain.RunOrigin, error) {
	var o runOriginJSON
	if err := json.Unmarshal(raw, &o); err != nil {
		return domain.RunOrigin{}, fmt.Errorf("storage: decode origin: %w", err)
	}
	return decodeOrigin(o), nil
}

func EncodeDeliveryTargets(targets []domain.DeliveryTarget) ([]byte, error) {
	type target struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref,omitempty"`
	}

	out := make([]target, 0, len(targets))
	for _, t := range targets {
		out = append(out, target{Kind: t.Kind, Ref: t.Ref})
	}
	return json.Marshal(out)
}

func DecodeDeliveryTargets(raw []byte) ([]domain.DeliveryTarget, error) {
	var decoded []struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("storage: decode delivery targets: %w", err)
	}
	if len(decoded) == 0 {
		return nil, nil
	}

	out := make([]domain.DeliveryTarget, 0, len(decoded))
	for _, t := range decoded {
		out = append(out, domain.DeliveryTarget{Kind: t.Kind, Ref: t.Ref})
	}
	return out, nil
}

func encodeOrigin(origin domain.RunOrigin) runOriginJSON {
	out := runOriginJSON{
		Kind:     string(origin.Kind),
		ClientID: origin.ClientID,
	}
	if origin.Principal != nil {
		out.Principal = &externalPrincipal{
			Platform:    origin.Principal.Platform,
			AccountID:   origin.Principal.AccountID,
			TenantID:    origin.Principal.TenantID,
			PrincipalID: origin.Principal.PrincipalID,
			DisplayName: origin.Principal.DisplayName,
		}
	}
	return out
}

func decodeOrigin(o runOriginJSON) domain.RunOrigin {
	out := domain.RunOrigin{
		Kind:     domain.RunOriginKind(o.Kind),
		ClientID: o.ClientID,
	}
	if o.Principal != nil {
		out.Principal = &domain.ExternalPrincipal{
			Platform:    o.Principal.Platform,
			AccountID:   o.Principal.AccountID,
			TenantID:    o.Principal.TenantID,
			PrincipalID: o.Principal.PrincipalID,
			DisplayName: o.Principal.DisplayName,
		}
	}
	return out
}
