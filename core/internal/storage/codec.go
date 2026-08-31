package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Event payloads are stored as JSON rather than given struct tags in the
// domain package, so the persisted shape is an explicit decision here instead
// of an accident of how the domain types happen to be written. The same
// reasoning keeps protobuf translation confined to internal/control.
//
// These field names are on-disk format. Renaming one is a migration.

type userMessageAddedJSON struct {
	MessageID   string           `json:"message_id"`
	Text        string           `json:"text"`
	Trust       string           `json:"trust"`
	Origin      runOriginJSON    `json:"origin"`
	Attachments []attachmentJSON `json:"attachments,omitempty"`
}

type attachmentJSON struct {
	ArtifactID string `json:"artifact_id,omitempty"`
	Name       string `json:"name,omitempty"`
	MediaType  string `json:"media_type,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

type runStateChangedJSON struct {
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	FailureKind string `json:"failure_kind,omitempty"`
}

type assistantTextDeltaJSON struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

type assistantReasoningDeltaJSON struct {
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
	CallID     string            `json:"call_id"`
	Name       string            `json:"name"`
	Summary    string            `json:"summary,omitempty"`
	Content    string            `json:"content"`
	IsError    bool              `json:"is_error,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
	Foreign    bool              `json:"foreign,omitempty"`
	From       domain.Provenance `json:"from,omitempty"`
	Artifact   *artifactJSON     `json:"artifact,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
}

type artifactJSON struct {
	ID        string `json:"id"`
	Size      int64  `json:"size,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type planItemJSON struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type planChangedJSON struct {
	Items []planItemJSON `json:"items"`
}

type questionOptionJSON struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

type questionAskedJSON struct {
	QuestionID string               `json:"question_id"`
	CallID     string               `json:"call_id"`
	Prompt     string               `json:"prompt"`
	Kind       string               `json:"kind"`
	Options    []questionOptionJSON `json:"options,omitempty"`
}

type questionAnsweredJSON struct {
	QuestionID string        `json:"question_id"`
	CallID     string        `json:"call_id"`
	Status     string        `json:"status"`
	Answer     string        `json:"answer,omitempty"`
	AnsweredBy decidedByJSON `json:"answered_by,omitempty"`
}

type approvalRequestedJSON struct {
	ApprovalID  string   `json:"approval_id"`
	CallID      string   `json:"call_id"`
	ToolName    string   `json:"tool_name"`
	Arguments   string   `json:"arguments"`
	Summary     string   `json:"summary,omitempty"`
	Effects     []string `json:"effects,omitempty"`
	Preview     string   `json:"preview,omitempty"`
	ReadForeign bool     `json:"read_foreign,omitempty"`
}

type approvalResolvedJSON struct {
	ApprovalID string        `json:"approval_id"`
	CallID     string        `json:"call_id"`
	ToolName   string        `json:"tool_name"`
	Status     string        `json:"status"`
	Scope      string        `json:"scope,omitempty"`
	DecidedBy  decidedByJSON `json:"decided_by,omitempty"`
}

type skillActivatedJSON struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest"`
}

type conversationCompactedJSON struct {
	Summary        string `json:"summary"`
	ThroughSeq     uint64 `json:"through_seq"`
	MessagesFolded int    `json:"messages_folded,omitempty"`
	TokensBefore   int64  `json:"tokens_before,omitempty"`
	TokensAfter    int64  `json:"tokens_after,omitempty"`
}

type runDirectionsJSON struct {
	Text string `json:"text"`
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

// decidedByJSON reads a field that used to be one string and is now an origin.
//
// The log is append-only, so the events already in it are the specification
// for what this must still read. Written before the split, "who decided this"
// held a single string, and the only shape ever produced by a running
// deployment was "<platform>:<principal id>" from a button press — the one
// path that did know a person.
//
// New values are written as an object and read as one. Old values are read as
// what they were: somebody a platform named, arriving through a gateway.
type decidedByJSON struct {
	runOriginJSON
}

func (d *decidedByJSON) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		var old string
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		d.runOriginJSON = fromTheOldString(old)
		return nil
	}
	return json.Unmarshal(raw, &d.runOriginJSON)
}

// fromTheOldString reads a value written before the field carried an origin.
func fromTheOldString(old string) runOriginJSON {
	if old == "" {
		return runOriginJSON{}
	}

	platform, id, split := strings.Cut(old, ":")
	if !split || platform == "" || id == "" {
		// Anything else was a client name or a platform on its own, and
		// neither says who. Kept where it is rather than promoted into a
		// principal, which is the mistake this split exists to undo.
		return runOriginJSON{Kind: string(domain.OriginGateway), ClientID: old}
	}
	return runOriginJSON{
		Kind:      string(domain.OriginGateway),
		Principal: &externalPrincipal{Platform: platform, PrincipalID: id},
	}
}

type externalPrincipal struct {
	Platform    string `json:"platform"`
	AccountID   string `json:"account_id"`
	TenantID    string `json:"tenant_id"`
	PrincipalID string `json:"principal_id"`
	DisplayName string `json:"display_name,omitempty"`
}

func encodeAttachments(attachments []domain.Attachment) []attachmentJSON {
	if len(attachments) == 0 {
		return nil
	}

	encoded := make([]attachmentJSON, 0, len(attachments))
	for _, attachment := range attachments {
		encoded = append(encoded, attachmentJSON{
			ArtifactID: attachment.ArtifactID,
			Name:       attachment.Name,
			MediaType:  attachment.MediaType,
			Size:       attachment.Size,
		})
	}
	return encoded
}

func decodeAttachments(stored []attachmentJSON) []domain.Attachment {
	if len(stored) == 0 {
		return nil
	}

	decoded := make([]domain.Attachment, 0, len(stored))
	for _, attachment := range stored {
		decoded = append(decoded, domain.Attachment{
			ArtifactID: attachment.ArtifactID,
			Name:       attachment.Name,
			MediaType:  attachment.MediaType,
			Size:       attachment.Size,
		})
	}
	return decoded
}

func encodeArtifact(ref *domain.Artifact) *artifactJSON {
	if ref == nil {
		return nil
	}
	return &artifactJSON{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}
}

func decodeArtifact(stored *artifactJSON) *domain.Artifact {
	if stored == nil {
		return nil
	}
	return &domain.Artifact{ID: stored.ID, Size: stored.Size, MediaType: stored.MediaType}
}

// EncodePayload serializes an event payload for storage.
func EncodePayload(payload domain.EventPayload) ([]byte, error) {
	switch p := payload.(type) {
	case domain.UserMessageAdded:
		return json.Marshal(userMessageAddedJSON{
			MessageID:   string(p.MessageID),
			Text:        p.Text,
			Trust:       string(p.Trust),
			Origin:      encodeOrigin(p.Origin),
			Attachments: encodeAttachments(p.Attachments),
		})

	case domain.RunStateChanged:
		return json.Marshal(runStateChangedJSON{
			Status:      string(p.Status),
			Reason:      p.Reason,
			FailureKind: p.FailureKind,
		})

	case domain.AssistantTextDelta:
		return json.Marshal(assistantTextDeltaJSON{
			MessageID: string(p.MessageID),
			Text:      p.Text,
		})

	case domain.AssistantReasoningDelta:
		return json.Marshal(assistantReasoningDeltaJSON{
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
			Foreign:    p.Foreign,
			From:       p.From,
			Artifact:   encodeArtifact(p.Artifact),
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

	case domain.RunDirections:
		return json.Marshal(runDirectionsJSON{Text: p.Text})

	case domain.PlanChanged:
		items := make([]planItemJSON, 0, len(p.Items))
		for _, item := range p.Items {
			items = append(items, planItemJSON{
				ID: item.ID, Title: item.Title,
				Status: string(item.Status), Note: item.Note,
			})
		}
		return json.Marshal(planChangedJSON{Items: items})

	case domain.QuestionAsked:
		options := make([]questionOptionJSON, 0, len(p.Options))
		for _, option := range p.Options {
			options = append(options, questionOptionJSON{
				ID: option.ID, Label: option.Label, Detail: option.Detail,
			})
		}
		return json.Marshal(questionAskedJSON{
			QuestionID: string(p.QuestionID),
			CallID:     string(p.CallID),
			Prompt:     p.Prompt,
			Kind:       string(p.Kind),
			Options:    options,
		})

	case domain.QuestionAnswered:
		return json.Marshal(questionAnsweredJSON{
			QuestionID: string(p.QuestionID),
			CallID:     string(p.CallID),
			Status:     string(p.Status),
			Answer:     p.Answer,
			AnsweredBy: decidedByJSON{encodeOrigin(p.AnsweredBy)},
		})

	case domain.ApprovalRequested:
		return json.Marshal(approvalRequestedJSON{
			ApprovalID:  string(p.ApprovalID),
			CallID:      string(p.CallID),
			ToolName:    p.ToolName,
			Arguments:   p.Arguments,
			Summary:     p.Summary,
			Effects:     p.Effects,
			Preview:     p.Preview,
			ReadForeign: p.ReadForeign,
		})

	case domain.SkillActivated:
		return json.Marshal(skillActivatedJSON{
			Name: p.Name, Version: p.Version, Digest: p.Digest,
		})

	case domain.ApprovalResolved:
		return json.Marshal(approvalResolvedJSON{
			ApprovalID: string(p.ApprovalID),
			CallID:     string(p.CallID),
			ToolName:   p.ToolName,
			Status:     string(p.Status),
			Scope:      string(p.Scope),
			DecidedBy:  decidedByJSON{encodeOrigin(p.DecidedBy)},
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
			MessageID:   domain.MessageID(p.MessageID),
			Text:        p.Text,
			Trust:       domain.TrustLevel(p.Trust),
			Origin:      decodeOrigin(p.Origin),
			Attachments: decodeAttachments(p.Attachments),
		}, nil

	case domain.EventRunStateChanged:
		var p runStateChangedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.RunStateChanged{
			Status:      domain.RunStatus(p.Status),
			Reason:      p.Reason,
			FailureKind: p.FailureKind,
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

	case domain.EventAssistantReasoningDelta:
		var p assistantReasoningDeltaJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.AssistantReasoningDelta{
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
			Foreign:    p.Foreign,
			From:       p.From,
			Artifact:   decodeArtifact(p.Artifact),
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

	case domain.EventRunDirections:
		var p runDirectionsJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.RunDirections{Text: p.Text}, nil

	case domain.EventPlanChanged:
		var p planChangedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		items := make([]domain.PlanItem, 0, len(p.Items))
		for _, item := range p.Items {
			items = append(items, domain.PlanItem{
				ID: item.ID, Title: item.Title,
				Status: domain.PlanStatus(item.Status), Note: item.Note,
			})
		}
		return domain.PlanChanged{Items: items}, nil

	case domain.EventQuestionAsked:
		var p questionAskedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		options := make([]domain.QuestionOption, 0, len(p.Options))
		for _, option := range p.Options {
			options = append(options, domain.QuestionOption{
				ID: option.ID, Label: option.Label, Detail: option.Detail,
			})
		}
		return domain.QuestionAsked{
			QuestionID: domain.QuestionID(p.QuestionID),
			CallID:     domain.ToolCallID(p.CallID),
			Prompt:     p.Prompt,
			Kind:       domain.QuestionKind(p.Kind),
			Options:    options,
		}, nil

	case domain.EventQuestionAnswered:
		var p questionAnsweredJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.QuestionAnswered{
			QuestionID: domain.QuestionID(p.QuestionID),
			CallID:     domain.ToolCallID(p.CallID),
			Status:     domain.QuestionStatus(p.Status),
			Answer:     p.Answer,
			AnsweredBy: decodeOrigin(p.AnsweredBy.runOriginJSON),
		}, nil

	case domain.EventApprovalRequested:
		var p approvalRequestedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.ApprovalRequested{
			ApprovalID:  domain.ApprovalID(p.ApprovalID),
			CallID:      domain.ToolCallID(p.CallID),
			ToolName:    p.ToolName,
			Arguments:   p.Arguments,
			Summary:     p.Summary,
			Effects:     p.Effects,
			Preview:     p.Preview,
			ReadForeign: p.ReadForeign,
		}, nil

	case domain.EventSkillActivated:
		var p skillActivatedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("storage: decode %s: %w", kind, err)
		}
		return domain.SkillActivated{
			Name: p.Name, Version: p.Version, Digest: p.Digest,
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
			DecidedBy:  decodeOrigin(p.DecidedBy.runOriginJSON),
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
