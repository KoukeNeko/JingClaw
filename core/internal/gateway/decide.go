package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// ApprovalDecision is somebody answering an approval from the room it was
// posted in, by pressing a control on the message rather than by typing.
//
// The principal must be the one the platform authenticated for that press. It
// is never read out of the message, out of the control's own identifier, or
// out of anything else the person pressing could have chosen: that identity is
// the only thing separating an approver from everybody else who can see the
// same button.
type ApprovalDecision struct {
	Principal    Principal
	Conversation ConversationRef
	ApprovalID   domain.ApprovalID
	Allow        bool
}

// DecisionOutcome is what happened, in enough detail for a platform to say
// something true to the person who pressed and nothing at all to anybody else.
type DecisionOutcome string

const (
	// DecisionRecorded means this press decided it.
	DecisionRecorded DecisionOutcome = "recorded"

	// DecisionRefused means this person may not decide here. Deliberately not
	// distinguished from an approval that does not exist when it is reported:
	// telling somebody which of the two it was tells them something about a
	// room they are not trusted in.
	DecisionRefused DecisionOutcome = "refused"

	// DecisionAlready means somebody else got there first. Two approvers can
	// press at the same moment and exactly one of them can win.
	DecisionAlready DecisionOutcome = "already"

	// DecisionUnavailable means this ingress cannot decide anything, because
	// nothing was wired in to do it.
	DecisionUnavailable DecisionOutcome = "unavailable"
)

// Decide answers an approval on behalf of somebody who pressed a control.
//
// Two gates, in this order. The room must name this person as an approver,
// which is a fact about the deployment; and the approval must still be
// pending, which is settled by the store rather than here — a decision is
// recorded by an update that only matches a pending row, so two presses in
// the same instant cannot both take effect.
//
// Nothing about the approval is returned. The caller reports the outcome to
// the person who pressed, and a refusal must not become a way to read what
// was being asked.
func (i *Ingress) Decide(ctx context.Context, decision ApprovalDecision) (DecisionOutcome, error) {
	if i.Decisions == nil {
		return DecisionUnavailable, nil
	}

	binding, err := i.Store.Binding(ctx,
		decision.Conversation.Platform, decision.Conversation.AccountID,
		decision.Conversation.TenantID, decision.Conversation.ChannelID)
	if err != nil {
		if errors.Is(err, ErrBindingNotFound) {
			return DecisionRefused, nil
		}
		return "", fmt.Errorf("gateway: read binding: %w", err)
	}

	if !binding.MayApprove(decision.Principal) {
		return DecisionRefused, nil
	}

	// Who decided, in the form the rest of the system records people: the
	// platform and the account, not a display name that its owner can change
	// to somebody else's between the press and the log line.
	decidedBy := string(decision.Conversation.Platform) + ":" + decision.Principal.ID

	if _, err := i.Decisions.DecideApproval(
		ctx, decision.ApprovalID, decision.Allow, domain.RememberOnce, decidedBy,
	); err != nil {
		if errors.Is(err, domain.ErrApprovalDecided) {
			return DecisionAlready, nil
		}
		return "", fmt.Errorf("gateway: decide approval: %w", err)
	}

	return DecisionRecorded, nil
}
