package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

// buttonPrefix marks the controls this adapter owns.
//
// The identifier of a button is not a secret and not a claim: Discord hands it
// back exactly as it was sent, from whoever pressed it. It therefore carries a
// locator and nothing else — no user, no role, no permission, no arguments.
// Everything that decides whether a press counts is looked up here from what
// the platform authenticated.
const buttonPrefix = "jc.approval"

const (
	buttonAllow = "allow"
	buttonDeny  = "deny"
)

// Decider is what this adapter needs in order to act on a press.
//
// Separate from Sink, which starts work. Being able to ask the agent for
// something and being able to permit what it asks are different powers, and an
// adapter given one and not the other cannot do the other's job.
type Decider interface {
	Decide(ctx context.Context, decision jcgateway.ApprovalDecision) (jcgateway.DecisionOutcome, error)
}

// approvalButtons is the row attached to an approval a room may answer.
func approvalButtons(approvalID string) discord.LayoutComponent {
	return discord.NewActionRow(
		discord.NewSuccessButton("Approve", buttonID(approvalID, buttonAllow)),
		discord.NewDangerButton("Deny", buttonID(approvalID, buttonDeny)),
	)
}

func buttonID(approvalID, action string) string {
	return buttonPrefix + ":" + approvalID + ":" + action
}

// parseButtonID reads back what buttonID wrote.
//
// Anything that does not parse belongs to somebody else and is left alone: a
// bot may carry other buttons, and swallowing their presses would break them
// while looking like nothing happened.
func parseButtonID(customID string) (approvalID, action string, ok bool) {
	parts := strings.Split(customID, ":")
	if len(parts) != 3 || parts[0] != buttonPrefix {
		return "", "", false
	}
	if parts[1] == "" || (parts[2] != buttonAllow && parts[2] != buttonDeny) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// approvalIDOf reports which approval a dispatch is about, so the buttons can
// name it. Only an approval carries one.
func approvalIDOf(dispatch jcgateway.Dispatch) (string, bool) {
	if dispatch.Kind != jcgateway.DispatchApproval {
		return "", false
	}

	var payload jcgateway.ApprovalPayload
	if err := json.Unmarshal([]byte(dispatch.Payload), &payload); err != nil {
		return "", false
	}
	if payload.Route != jcgateway.ApprovalByPress || payload.ApprovalID == "" {
		return "", false
	}
	return payload.ApprovalID, true
}

// onComponent handles a button press.
//
// Every path answers Discord, and answers it first. An interaction that is not
// responded to within a few seconds is reported to the person as having failed,
// whatever the daemon went on to do about it — so the reply is sent before any
// of the work, and it is ephemeral in every case: a room does not need to be
// told who tried to approve something and was not allowed to.
func (a *Adapter) onComponent(event *events.ComponentInteractionCreate) {
	approvalID, action, ok := parseButtonID(event.Data.CustomID())
	if !ok {
		return
	}

	if a.decider == nil {
		a.replyQuietly(event, "Approving from here is not available.")
		return
	}

	principal, ok := a.pressPrincipal(event)
	if !ok {
		a.replyQuietly(event, "Discord did not say who pressed that.")
		return
	}

	decision := jcgateway.ApprovalDecision{
		Principal:    principal,
		Conversation: a.pressConversation(event),
		ApprovalID:   domain.ApprovalID(approvalID),
		Allow:        action == buttonAllow,
	}

	outcome, err := a.decider.Decide(context.WithoutCancel(context.Background()), decision)
	if err != nil {
		a.config.Logger.Error("could not record a decision from discord",
			"approval_id", approvalID, "error", err)
		a.replyQuietly(event, "That could not be recorded. Try again.")
		return
	}

	a.replyQuietly(event, outcomeText(outcome, action))

	// The buttons go only once a decision is actually recorded. Removing them
	// on a refusal would let anybody who can see the message take the controls
	// away from the people who can use them.
	if outcome == jcgateway.DecisionRecorded || outcome == jcgateway.DecisionAlready {
		a.settleButtons(event, outcome, action)
	}
}

// outcomeText says what happened without saying what was being asked.
//
// A refusal reads the same whether the approval exists or not. Telling
// somebody which it was tells them something about a room they are not
// trusted in.
func outcomeText(outcome jcgateway.DecisionOutcome, action string) string {
	switch outcome {
	case jcgateway.DecisionRecorded:
		if action == buttonAllow {
			return "Approved."
		}
		return "Denied."
	case jcgateway.DecisionAlready:
		return "Somebody already decided this one."
	case jcgateway.DecisionUnavailable:
		return "Approving from here is not available."
	default:
		return "You cannot decide this here."
	}
}

// replyQuietly answers the person who pressed and nobody else.
func (a *Adapter) replyQuietly(event *events.ComponentInteractionCreate, text string) {
	err := event.CreateMessage(discord.MessageCreate{
		Content:         text,
		Flags:           discord.MessageFlagEphemeral,
		AllowedMentions: &discord.AllowedMentions{},
	})
	if err != nil {
		a.config.Logger.Warn("could not answer a discord button press", "error", err)
	}
}

// settleButtons takes the controls off a message that has been decided.
//
// Cosmetic, and deliberately so: two approvers pressing in the same instant
// are separated by the store, which records a decision only against something
// still pending. This is what stops the second one from having to find that
// out by pressing.
func (a *Adapter) settleButtons(
	event *events.ComponentInteractionCreate,
	outcome jcgateway.DecisionOutcome,
	action string,
) {
	message := event.Message
	settled := message.Content + "\n" + settledLine(outcome, action, event)

	_, err := a.client.Rest.UpdateMessage(message.ChannelID, message.ID,
		discord.MessageUpdate{
			Content:         &settled,
			Components:      &[]discord.LayoutComponent{},
			AllowedMentions: &discord.AllowedMentions{},
		})
	if err != nil {
		a.config.Logger.Warn("could not clear approval buttons",
			"message_id", message.ID.String(), "error", err)
	}
}

func settledLine(
	outcome jcgateway.DecisionOutcome,
	action string,
	event *events.ComponentInteractionCreate,
) string {
	if outcome == jcgateway.DecisionAlready {
		return "— already decided."
	}

	verb := "Denied"
	if action == buttonAllow {
		verb = "Approved"
	}
	return fmt.Sprintf("— %s by <@%s>.", verb, event.User().ID.String())
}

// pressPrincipal reads who pressed, from what the platform said rather than
// from anything in the message.
//
// In a guild the member is the invoking user; in a direct message there is no
// member and the user stands alone. Roles travel as claims, the same shape a
// typed message produces, so one allowlist can be written against either.
func (a *Adapter) pressPrincipal(event *events.ComponentInteractionCreate) (jcgateway.Principal, bool) {
	user := event.User()
	if user.ID == 0 {
		return jcgateway.Principal{}, false
	}

	principal := jcgateway.Principal{
		Platform:    jcgateway.PlatformDiscord,
		AccountID:   a.config.AccountID,
		TenantID:    tenantOf(event.GuildID()),
		ID:          user.ID.String(),
		DisplayName: user.Username,
		IsBot:       user.Bot,
	}

	if member := event.Member(); member != nil {
		for _, role := range member.RoleIDs {
			principal.Claims = append(principal.Claims, jcgateway.Claim{
				Namespace: "discord.role",
				Value:     role.String(),
			})
		}
	}

	return principal, true
}

func (a *Adapter) pressConversation(event *events.ComponentInteractionCreate) jcgateway.ConversationRef {
	return jcgateway.ConversationRef{
		Platform:  jcgateway.PlatformDiscord,
		AccountID: a.config.AccountID,
		TenantID:  tenantOf(event.GuildID()),
		ChannelID: event.Channel().ID().String(),
	}
}

func tenantOf(guildID *snowflake.ID) string {
	if guildID == nil {
		return ""
	}
	return guildID.String()
}
