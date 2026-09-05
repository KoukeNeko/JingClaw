package gateway_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// line is a session's runs as a withdrawer sees them, and what it was asked
// to stop.
type line struct {
	runs        []domain.Run
	interrupted []domain.RunID
}

func (l *line) Runs(context.Context, domain.SessionID) ([]domain.Run, error) {
	return l.runs, nil
}

func (l *line) InterruptRun(_ context.Context, id domain.RunID, _ string) (domain.RunStatus, error) {
	l.interrupted = append(l.interrupted, id)
	return domain.RunCancelled, nil
}

// waitingRun is a run a platform message started that has not begun.
func waitingRun(id string, messageID string, sender gateway.Principal) domain.Run {
	conversation := discordConversation()
	conversation.SourceMessageID = messageID
	return domain.Run{
		ID:              domain.RunID(id),
		SessionID:       "ses_1",
		Status:          domain.RunQueued,
		Origin:          sender.Origin(),
		DeliveryTargets: []domain.DeliveryTarget{conversation.DeliveryTarget()},
	}
}

// newLine accepts a message so it is claimed, then puts a run for it in line.
func newLine(t *testing.T, run domain.Run) (*gateway.Ingress, *line) {
	t.Helper()
	ingress, store, _, _ := newIngress(t)
	bindChannel(t, store, "user_1")

	source, _ := gateway.ConversationFromTarget(run.DeliveryTargets[0])
	if _, err := ingress.Accept(context.Background(),
		message(source.SourceMessageID, "second thoughts", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	waiting := &line{runs: []domain.Run{run}}
	ingress.Withdrawals = waiting
	return ingress, waiting
}

func withdrawal(by string, messageID string) gateway.Withdrawal {
	return gateway.Withdrawal{
		Principal:  discordPrincipal(by),
		InboundKey: messageID,
		MessageID:  messageID,
	}
}

// The person who sent a waiting message can take it back.
func TestTheSenderCanTakeAWaitingMessageBack(t *testing.T) {
	ingress, waiting := newLine(t, waitingRun("run_2", "m2", discordPrincipal("user_1")))

	withdrawn, err := ingress.Withdraw(context.Background(), withdrawal("user_1", "m2"))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if !withdrawn {
		t.Fatal("the sender's own waiting message was not taken back")
	}
	if len(waiting.interrupted) != 1 || waiting.interrupted[0] != "run_2" {
		t.Errorf("stopped %v, want run_2", waiting.interrupted)
	}
}

// Nobody else can: pressing the mark on somebody else's message does nothing.
func TestOnlyTheSenderCanTakeItBack(t *testing.T) {
	ingress, waiting := newLine(t, waitingRun("run_2", "m2", discordPrincipal("user_1")))

	withdrawn, err := ingress.Withdraw(context.Background(), withdrawal("user_2", "m2"))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if withdrawn || len(waiting.interrupted) != 0 {
		t.Errorf("somebody else took a message back: withdrawn=%v stopped=%v", withdrawn, waiting.interrupted)
	}
}

// A message already being answered is not taken back by this gesture.
// Stopping an answer is a different thing, said differently.
func TestAMessageBeingAnsweredIsNotTakenBack(t *testing.T) {
	run := waitingRun("run_2", "m2", discordPrincipal("user_1"))
	run.Status = domain.RunRunning
	ingress, waiting := newLine(t, run)

	withdrawn, err := ingress.Withdraw(context.Background(), withdrawal("user_1", "m2"))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if withdrawn || len(waiting.interrupted) != 0 {
		t.Errorf("a running answer was stopped by the waiting mark: withdrawn=%v stopped=%v", withdrawn, waiting.interrupted)
	}
}

// A message the agent never took has nothing to take back.
func TestAMessageTheAgentNeverTookHasNothingToWithdraw(t *testing.T) {
	ingress, waiting := newLine(t, waitingRun("run_2", "m2", discordPrincipal("user_1")))

	withdrawn, err := ingress.Withdraw(context.Background(), withdrawal("user_1", "never-sent"))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if withdrawn || len(waiting.interrupted) != 0 {
		t.Errorf("an unknown message withdrew something: withdrawn=%v stopped=%v", withdrawn, waiting.interrupted)
	}
}

// An ingress with no reach into the line says so rather than pretending.
func TestWithdrawingNeedsTheLine(t *testing.T) {
	ingress, _, _, _ := newIngress(t)

	_, err := ingress.Withdraw(context.Background(), withdrawal("user_1", "m2"))
	if !errors.Is(err, gateway.ErrWithdrawUnavailable) {
		t.Fatalf("got %v, want %v", err, gateway.ErrWithdrawUnavailable)
	}
}
