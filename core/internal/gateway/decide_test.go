package gateway_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// bindApprovers binds the test channel with people who may ask and, separately,
// people who may permit.
func bindApprovers(t *testing.T, h *harness, allowed, approvers []string) {
	t.Helper()

	conversation := discordConversation()
	if err := h.store.UpsertBinding(context.Background(), gateway.Binding{
		ID:                  "binding_1",
		Platform:            conversation.Platform,
		AccountID:           conversation.AccountID,
		TenantID:            conversation.TenantID,
		ChannelID:           conversation.ChannelID,
		WorkspaceID:         "ws_1",
		PermissionProfile:   "gateway",
		AllowedPrincipals:   allowed,
		ApprovingPrincipals: approvers,
		CreatedAt:           time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func press(who string, id domain.ApprovalID, allow bool) gateway.ApprovalDecision {
	return gateway.ApprovalDecision{
		Principal:    discordPrincipal(who),
		Conversation: discordConversation(),
		ApprovalID:   id,
		Allow:        allow,
	}
}

// pausedHarness is a run scripted to ask for something that stops for a
// person, so there is a real pending approval to press at.
func pausedHarness(t *testing.T) *harness {
	t.Helper()

	arguments, err := json.Marshal(map[string]any{"path": "notes.md", "content": "written"})
	if err != nil {
		t.Fatal(err)
	}

	return newSummaryHarness(t, [][]provider.Event{
		{
			provider.ToolCallRequested{ID: "call_1", Name: "write_file", Args: arguments},
			provider.Completed{StopReason: domain.StopToolUse},
		},
		{
			provider.TextDelta{Text: "Written."},
			provider.Completed{StopReason: domain.StopEndTurn},
		},
	})
}

// waitingApproval starts the run and returns what it is waiting on.
func waitingApproval(t *testing.T, h *harness) domain.ApprovalID {
	t.Helper()

	if _, err := h.ingress.Accept(context.Background(),
		message("m1", "write notes.md", discordPrincipal("user_1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}

	dispatch := waitForDispatch(t, h, gateway.DispatchApproval)

	var payload gateway.ApprovalPayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.ApprovalID == "" {
		t.Fatal("the approval carries no id")
	}
	return domain.ApprovalID(payload.ApprovalID)
}

// Being allowed to ask the agent for something is not being allowed to permit
// it. A room where everybody who can talk can also approve is a deployment
// somebody should have had to write down.
func TestAskingIsNotApproving(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, nil)

	approval := waitingApproval(t, h)

	outcome, err := h.ingress.Decide(context.Background(), press("user_1", approval, true))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if outcome != gateway.DecisionRefused {
		t.Errorf("the person who asked was allowed to approve: %q", outcome)
	}
}

func TestANamedApproverDecides(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, []string{"user_2"})

	approval := waitingApproval(t, h)

	outcome, err := h.ingress.Decide(context.Background(), press("user_2", approval, true))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if outcome != gateway.DecisionRecorded {
		t.Errorf("a named approver was refused: %q", outcome)
	}
}

// Everybody in the room can see the same button. What separates an approver
// from everybody else is the identity the platform attaches to the press.
func TestSomebodyElseInTheRoomCannotDecide(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, []string{"user_2"})

	approval := waitingApproval(t, h)

	outcome, err := h.ingress.Decide(context.Background(), press("user_9", approval, true))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if outcome != gateway.DecisionRefused {
		t.Errorf("a stranger decided it: %q", outcome)
	}
}

// A refusal reads the same whether the approval exists or not. Telling
// somebody which it was tells them something about a room they are not
// trusted in.
func TestARefusalDoesNotRevealWhetherItExists(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, []string{"user_2"})

	real := waitingApproval(t, h)

	forReal, err := h.ingress.Decide(context.Background(), press("user_9", real, true))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	invented, err := h.ingress.Decide(context.Background(),
		press("user_9", domain.ApprovalID("apr_nothing"), true))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	if forReal != invented {
		t.Errorf("a stranger can tell a real approval (%q) from an invented one (%q)",
			forReal, invented)
	}
}

// Two approvers can press in the same instant and exactly one can win. The
// store settles it, not the order the presses arrive in.
func TestOnlyOnePressDecides(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, []string{"user_2", "user_3"})

	approval := waitingApproval(t, h)

	first, err := h.ingress.Decide(context.Background(), press("user_2", approval, true))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := h.ingress.Decide(context.Background(), press("user_3", approval, false))
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first != gateway.DecisionRecorded {
		t.Errorf("the first press did not decide it: %q", first)
	}
	if second != gateway.DecisionAlready {
		t.Errorf("the second press was reported as %q, want already decided", second)
	}
}

// A channel nobody bound cannot be a way to decide something.
func TestAPressFromAnUnboundChannelIsRefused(t *testing.T) {
	h := pausedHarness(t)
	bindApprovers(t, h, []string{"user_1"}, []string{"user_2"})

	approval := waitingApproval(t, h)

	elsewhere := press("user_2", approval, true)
	elsewhere.Conversation.ChannelID = "channel_nobody_bound"

	outcome, err := h.ingress.Decide(context.Background(), elsewhere)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if outcome != gateway.DecisionRefused {
		t.Errorf("an unbound channel decided something: %q", outcome)
	}
}

// A bot pressing its own button is a loop, and a bot account is not a person
// who can be held to a decision.
func TestABotCannotApprove(t *testing.T) {
	binding := gateway.Binding{ApprovingPrincipals: []string{"bot_1"}}

	robot := gateway.Principal{ID: "bot_1", IsBot: true}
	if binding.MayApprove(robot) {
		t.Error("a bot listed as an approver was allowed to approve")
	}

	person := gateway.Principal{ID: "bot_1"}
	if !binding.MayApprove(person) {
		t.Error("the same id from a person was refused, so the test proves nothing")
	}
}

// Roles are how a deployment names a group without listing it. They travel as
// opaque claims, the same shape a typed message produces.
func TestAnApproverRoleIsEnough(t *testing.T) {
	binding := gateway.Binding{
		ApprovingClaims: []gateway.Claim{{Namespace: "discord.role", Value: "role_ops"}},
	}

	member := gateway.Principal{
		ID:     "user_5",
		Claims: []gateway.Claim{{Namespace: "discord.role", Value: "role_ops"}},
	}
	if !binding.MayApprove(member) {
		t.Error("somebody holding the named role was refused")
	}

	outsider := gateway.Principal{
		ID:     "user_6",
		Claims: []gateway.Claim{{Namespace: "discord.role", Value: "role_everyone"}},
	}
	if binding.MayApprove(outsider) {
		t.Error("a different role was accepted")
	}
}

// An empty list is nobody, never everybody. This is the failure that turns a
// missing setting into an open door.
func TestNoApproversMeansNobody(t *testing.T) {
	binding := gateway.Binding{AllowedPrincipals: []string{"user_1"}}

	if binding.MayApprove(gateway.Principal{ID: "user_1"}) {
		t.Error("a binding with no approvers let somebody approve")
	}
}
