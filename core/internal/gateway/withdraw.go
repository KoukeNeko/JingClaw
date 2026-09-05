package gateway

import (
	"context"
	"errors"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// WithdrawnReason is what a run taken back out of its line is cancelled with.
const WithdrawnReason = "withdrawn"

// ErrWithdrawUnavailable says nothing here can reach the line.
var ErrWithdrawUnavailable = errors.New("gateway: withdrawing is not available")

// Withdrawer is the reach needed to take a waiting message out of its line:
// see what a session has in it, and stop one thing.
type Withdrawer interface {
	Runs(ctx context.Context, session domain.SessionID) ([]domain.Run, error)
	InterruptRun(ctx context.Context, id domain.RunID, reason string) (domain.RunStatus, error)
}

// Withdrawal is somebody taking back a message that has not been answered.
type Withdrawal struct {
	// Principal is who asked. Only the person who sent a message may take it
	// back, and the platform is what says who pressed.
	Principal Principal

	// InboundKey is the key the message was claimed under when it arrived,
	// which is how the session it went to is found.
	InboundKey string

	// MessageID is the platform's id for the message, which is how its run is
	// told apart from the others waiting in the same session.
	MessageID string
}

// Withdraw takes a waiting message out of its line.
//
// It reports whether anything was taken back. Nothing is not an error: the
// message may have started being answered, or never have been one the agent
// took, and either way the right response is silence.
func (i *Ingress) Withdraw(ctx context.Context, withdrawal Withdrawal) (bool, error) {
	if i.Withdrawals == nil {
		return false, ErrWithdrawUnavailable
	}

	session, found, err := i.Store.InboundSession(ctx, withdrawal.InboundKey)
	if err != nil || !found {
		return false, err
	}

	runs, err := i.Withdrawals.Runs(ctx, session)
	if err != nil {
		return false, err
	}

	for _, run := range runs {
		if run.Status != domain.RunQueued || !startedBy(run, withdrawal.MessageID) {
			continue
		}
		if !sentBy(run, withdrawal.Principal) {
			// Somebody else's message. Pressing the mark on it means nothing,
			// and saying so in the room would be answering a gesture that was
			// not addressed to anybody.
			i.log().Info("somebody other than the sender tried to take a message back",
				"run_id", string(run.ID), "principal", withdrawal.Principal.ID)
			return false, nil
		}

		status, err := i.Withdrawals.InterruptRun(ctx, run.ID, WithdrawnReason)
		if err != nil {
			return false, err
		}
		return status == domain.RunCancelled, nil
	}

	return false, nil
}

// startedBy reports whether a run was started by a platform message.
func startedBy(run domain.Run, messageID string) bool {
	for _, target := range run.DeliveryTargets {
		conversation, ok := ConversationFromTarget(target)
		if ok && conversation.SourceMessageID == messageID {
			return true
		}
	}
	return false
}

// sentBy reports whether a run's message came from a principal.
func sentBy(run domain.Run, principal Principal) bool {
	sender := run.Origin.Principal
	if sender == nil {
		return false
	}
	return sender.Platform == string(principal.Platform) &&
		sender.AccountID == principal.AccountID &&
		sender.TenantID == principal.TenantID &&
		sender.PrincipalID == principal.ID
}
