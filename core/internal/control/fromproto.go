package control

import (
	"fmt"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// EventFromProto reads an event off the wire.
//
// The inverse of eventToProto, beside it so the two can be read together and
// tested against each other. Exported because a client watching the log has
// to turn what it is sent back into something the rest of the program
// understands, and reimplementing this mapping somewhere else is how the two
// copies come to disagree.
//
// An unhandled kind is an error rather than an event with no payload: a
// client that quietly ignores what it does not recognise is one where adding
// an event kind makes it invisible.
func EventFromProto(in *controlv1.Event) (domain.Event, error) {
	out := domain.Event{
		ID:        domain.EventID(in.GetId()),
		SessionID: domain.SessionID(in.GetSessionId()),
		RunID:     domain.RunID(in.GetRunId()),
		Seq:       domain.Seq(in.GetSeq()),
		GlobalSeq: domain.Seq(in.GetGlobalSeq()),
	}
	if at := in.GetOccurredAt(); at != nil {
		out.OccurredAt = at.AsTime()
	}

	switch p := in.GetPayload().(type) {
	case *controlv1.Event_UserMessageAdded:
		said := p.UserMessageAdded
		out.Kind = domain.EventUserMessageAdded
		out.Payload = domain.UserMessageAdded{
			MessageID:   domain.MessageID(said.GetMessageId()),
			Text:        said.GetText(),
			Trust:       trustFromProto(said.GetTrust()),
			Origin:      originFromProto(said.GetOrigin()),
			Attachments: attachmentsFromProto(said.GetAttachments()),
		}

	case *controlv1.Event_RunStateChanged:
		changed := p.RunStateChanged
		out.Kind = domain.EventRunStateChanged
		out.Payload = domain.RunStateChanged{
			Status:      runStatusFromProto(changed.GetStatus()),
			Reason:      changed.GetReason(),
			FailureKind: changed.GetFailureKind(),
		}

	case *controlv1.Event_AssistantTextDelta:
		delta := p.AssistantTextDelta
		out.Kind = domain.EventAssistantTextDelta
		out.Payload = domain.AssistantTextDelta{
			MessageID: domain.MessageID(delta.GetMessageId()),
			Text:      delta.GetText(),
		}

	case *controlv1.Event_AssistantReasoningDelta:
		delta := p.AssistantReasoningDelta
		out.Kind = domain.EventAssistantReasoningDelta
		out.Payload = domain.AssistantReasoningDelta{
			MessageID: domain.MessageID(delta.GetMessageId()),
			Text:      delta.GetText(),
		}

	case *controlv1.Event_AssistantMessageCompleted:
		done := p.AssistantMessageCompleted
		out.Kind = domain.EventAssistantMessageCompleted
		out.Payload = domain.AssistantMessageCompleted{
			MessageID:  domain.MessageID(done.GetMessageId()),
			StopReason: stopReasonFromProto(done.GetStopReason()),
		}

	case *controlv1.Event_ToolCallRequested:
		asked := p.ToolCallRequested
		out.Kind = domain.EventToolCallRequested
		out.Payload = domain.ToolCallRequested{
			CallID:    domain.ToolCallID(asked.GetCallId()),
			Name:      asked.GetName(),
			Arguments: asked.GetArguments(),
		}

	case *controlv1.Event_ToolCallCompleted:
		done := p.ToolCallCompleted
		out.Kind = domain.EventToolCallCompleted
		out.Payload = domain.ToolCallCompleted{
			CallID:     domain.ToolCallID(done.GetCallId()),
			Name:       done.GetName(),
			Summary:    done.GetSummary(),
			Content:    done.GetContent(),
			IsError:    done.GetIsError(),
			Truncated:  done.GetTruncated(),
			Foreign:    done.GetForeign(),
			Artifact:   artifactFromProto(done.GetArtifact()),
			DurationMS: done.GetDurationMs(),
		}

	case *controlv1.Event_PlanChanged:
		out.Kind = domain.EventPlanChanged
		out.Payload = domain.PlanChanged{Items: planItemsFromProto(p.PlanChanged.GetItems())}

	case *controlv1.Event_QuestionAsked:
		asked := p.QuestionAsked
		out.Kind = domain.EventQuestionAsked
		out.Payload = domain.QuestionAsked{
			QuestionID: domain.QuestionID(asked.GetQuestionId()),
			CallID:     domain.ToolCallID(asked.GetCallId()),
			Prompt:     asked.GetPrompt(),
			Kind:       questionKindFromProto(asked.GetKind()),
			Options:    questionOptionsFromProto(asked.GetOptions()),
		}

	case *controlv1.Event_QuestionAnswered:
		answered := p.QuestionAnswered
		out.Kind = domain.EventQuestionAnswered
		out.Payload = domain.QuestionAnswered{
			QuestionID: domain.QuestionID(answered.GetQuestionId()),
			CallID:     domain.ToolCallID(answered.GetCallId()),
			Status:     questionStatusFromProto(answered.GetStatus()),
			Answer:     answered.GetAnswer(),
			AnsweredBy: originFromProto(answered.GetAnsweredBy()),
		}

	case *controlv1.Event_ApprovalRequested:
		asked := p.ApprovalRequested
		out.Kind = domain.EventApprovalRequested
		out.Payload = domain.ApprovalRequested{
			ApprovalID:  domain.ApprovalID(asked.GetApprovalId()),
			CallID:      domain.ToolCallID(asked.GetCallId()),
			ToolName:    asked.GetToolName(),
			Arguments:   asked.GetArguments(),
			Summary:     asked.GetSummary(),
			Effects:     asked.GetEffects(),
			Preview:     asked.GetPreview(),
			ReadForeign: asked.GetReadForeign(),
		}

	case *controlv1.Event_ApprovalResolved:
		resolved := p.ApprovalResolved
		out.Kind = domain.EventApprovalResolved
		out.Payload = domain.ApprovalResolved{
			ApprovalID: domain.ApprovalID(resolved.GetApprovalId()),
			CallID:     domain.ToolCallID(resolved.GetCallId()),
			ToolName:   resolved.GetToolName(),
			Status:     approvalStatusFromProto(resolved.GetStatus()),
			Scope:      rememberScopeFromProto(resolved.GetScope()),
			DecidedBy:  originFromProto(resolved.GetDecidedBy()),
		}

	case *controlv1.Event_UsageChanged:
		out.Kind = domain.EventUsageChanged
		out.Payload = domain.UsageChanged{Usage: usageFromProto(p.UsageChanged.GetUsage())}

	case *controlv1.Event_RunDirections:
		out.Kind = domain.EventRunDirections
		out.Payload = domain.RunDirections{Text: p.RunDirections.GetText()}

	case *controlv1.Event_ConversationCompacted:
		folded := p.ConversationCompacted
		out.Kind = domain.EventConversationCompacted
		out.Payload = domain.ConversationCompacted{
			Summary:        folded.GetSummary(),
			ThroughSeq:     domain.Seq(folded.GetThroughSeq()),
			MessagesFolded: int(folded.GetMessagesFolded()),
			TokensBefore:   folded.GetTokensBefore(),
			TokensAfter:    folded.GetTokensAfter(),
		}

	default:
		return domain.Event{}, fmt.Errorf("control: unhandled event payload %T", in.GetPayload())
	}

	return out, nil
}

// The inverse of each mapping beside it. Every pair is checked both ways by
// TestEveryValueSurvivesTheWire, so a value added to one side and not the
// other fails rather than silently arriving as the zero one.

func trustFromProto(t controlv1.TrustLevel) domain.TrustLevel {
	switch t {
	case controlv1.TrustLevel_TRUST_LEVEL_UNTRUSTED:
		return domain.TrustUntrusted
	case controlv1.TrustLevel_TRUST_LEVEL_USER:
		return domain.TrustUser
	case controlv1.TrustLevel_TRUST_LEVEL_WORKSPACE:
		return domain.TrustWorkspace
	case controlv1.TrustLevel_TRUST_LEVEL_SYSTEM:
		return domain.TrustSystem
	default:
		return ""
	}
}

func runStatusFromProto(s controlv1.RunStatus) domain.RunStatus {
	switch s {
	case controlv1.RunStatus_RUN_STATUS_QUEUED:
		return domain.RunQueued
	case controlv1.RunStatus_RUN_STATUS_RUNNING:
		return domain.RunRunning
	case controlv1.RunStatus_RUN_STATUS_AWAITING_APPROVAL:
		return domain.RunAwaitingApproval
	case controlv1.RunStatus_RUN_STATUS_AWAITING_INPUT:
		return domain.RunAwaitingInput
	case controlv1.RunStatus_RUN_STATUS_CANCELLING:
		return domain.RunCancelling
	case controlv1.RunStatus_RUN_STATUS_COMPLETED:
		return domain.RunCompleted
	case controlv1.RunStatus_RUN_STATUS_FAILED:
		return domain.RunFailed
	case controlv1.RunStatus_RUN_STATUS_CANCELLED:
		return domain.RunCancelled
	default:
		return ""
	}
}

func stopReasonFromProto(reason controlv1.StopReason) domain.StopReason {
	switch reason {
	case controlv1.StopReason_STOP_REASON_END_TURN:
		return domain.StopEndTurn
	case controlv1.StopReason_STOP_REASON_MAX_TOKENS:
		return domain.StopMaxTokens
	case controlv1.StopReason_STOP_REASON_CONTENT_FILTER:
		return domain.StopContentFilter
	case controlv1.StopReason_STOP_REASON_CANCELLED:
		return domain.StopCancelled
	case controlv1.StopReason_STOP_REASON_ERROR:
		return domain.StopError
	case controlv1.StopReason_STOP_REASON_TOOL_USE:
		return domain.StopToolUse
	default:
		return ""
	}
}

func questionKindFromProto(kind controlv1.QuestionKind) domain.QuestionKind {
	switch kind {
	case controlv1.QuestionKind_QUESTION_KIND_CHOICE:
		return domain.QuestionChoice
	case controlv1.QuestionKind_QUESTION_KIND_TEXT:
		return domain.QuestionText
	default:
		return ""
	}
}

func questionStatusFromProto(status controlv1.QuestionStatus) domain.QuestionStatus {
	switch status {
	case controlv1.QuestionStatus_QUESTION_STATUS_PENDING:
		return domain.AnswerPending
	case controlv1.QuestionStatus_QUESTION_STATUS_ANSWERED:
		return domain.AnswerGiven
	case controlv1.QuestionStatus_QUESTION_STATUS_CANCELLED:
		return domain.AnswerAbandoned
	default:
		return ""
	}
}

func approvalStatusFromProto(status controlv1.ApprovalStatus) domain.ApprovalStatus {
	switch status {
	case controlv1.ApprovalStatus_APPROVAL_STATUS_PENDING:
		return domain.ApprovalPending
	case controlv1.ApprovalStatus_APPROVAL_STATUS_ALLOWED:
		return domain.ApprovalAllowed
	case controlv1.ApprovalStatus_APPROVAL_STATUS_DENIED:
		return domain.ApprovalDenied
	default:
		return ""
	}
}

func planStatusFromProto(status controlv1.PlanStatus) domain.PlanStatus {
	switch status {
	case controlv1.PlanStatus_PLAN_STATUS_PENDING:
		return domain.PlanPending
	case controlv1.PlanStatus_PLAN_STATUS_IN_PROGRESS:
		return domain.PlanInProgress
	case controlv1.PlanStatus_PLAN_STATUS_COMPLETED:
		return domain.PlanCompleted
	case controlv1.PlanStatus_PLAN_STATUS_ABANDONED:
		return domain.PlanAbandoned
	default:
		return ""
	}
}

func originFromProto(o *controlv1.RunOrigin) domain.RunOrigin {
	if o == nil {
		return domain.RunOrigin{}
	}

	out := domain.RunOrigin{ClientID: o.GetClientId()}
	switch o.GetKind() {
	case controlv1.RunOriginKind_RUN_ORIGIN_KIND_LOCAL_CLIENT:
		out.Kind = domain.OriginLocalClient
	case controlv1.RunOriginKind_RUN_ORIGIN_KIND_GATEWAY:
		out.Kind = domain.OriginGateway
	}

	// Nil rather than an empty one: the difference between "a platform named
	// somebody" and "nobody was named" is the whole reason this is a pointer.
	if p := o.GetPrincipal(); p != nil {
		out.Principal = &domain.ExternalPrincipal{
			Platform:    p.GetPlatform(),
			AccountID:   p.GetAccountId(),
			TenantID:    p.GetTenantId(),
			PrincipalID: p.GetPrincipalId(),
			DisplayName: p.GetDisplayName(),
		}
	}
	return out
}

func attachmentsFromProto(in []*controlv1.MessageAttachment) []domain.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.Attachment, 0, len(in))
	for _, attachment := range in {
		out = append(out, domain.Attachment{
			ArtifactID: attachment.GetArtifactId(),
			Name:       attachment.GetName(),
			MediaType:  attachment.GetMediaType(),
			Size:       attachment.GetSize(),
		})
	}
	return out
}

func artifactFromProto(in *controlv1.Artifact) *domain.Artifact {
	if in == nil {
		return nil
	}
	return &domain.Artifact{
		ID:        in.GetId(),
		Size:      in.GetSize(),
		MediaType: in.GetMediaType(),
	}
}

func planItemsFromProto(in []*controlv1.PlanItem) []domain.PlanItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.PlanItem, 0, len(in))
	for _, item := range in {
		out = append(out, domain.PlanItem{
			ID:     item.GetId(),
			Title:  item.GetTitle(),
			Status: planStatusFromProto(item.GetStatus()),
			Note:   item.GetNote(),
		})
	}
	return out
}

func questionOptionsFromProto(in []*controlv1.QuestionOption) []domain.QuestionOption {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.QuestionOption, 0, len(in))
	for _, option := range in {
		out = append(out, domain.QuestionOption{
			ID:     option.GetId(),
			Label:  option.GetLabel(),
			Detail: option.GetDetail(),
		})
	}
	return out
}

func usageFromProto(in *controlv1.Usage) domain.Usage {
	if in == nil {
		return domain.Usage{}
	}
	return domain.Usage{
		InputTokens:       in.GetInputTokens(),
		CachedInputTokens: in.GetCachedInputTokens(),
		OutputTokens:      in.GetOutputTokens(),
		ReasoningTokens:   in.GetReasoningTokens(),
	}
}
