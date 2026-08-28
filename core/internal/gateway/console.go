package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// ConsoleProfileName is the profile a binding names to become a console.
const ConsoleProfileName = "console"

// ConsoleRuntime is the extra reach a console binding has.
//
// Separate from Runtime, and optional, because Runtime is deliberately narrow:
// an ingress serving ordinary channels may start work and nothing else. Adding
// approvals to that interface would hand every channel the ability to resolve
// them. Kept apart, an ingress without this cannot decide anything, and the
// widening is visible at the point somebody wires it in.
type ConsoleRuntime interface {
	PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error)

	DecideApproval(
		ctx context.Context,
		id domain.ApprovalID,
		allow bool,
		scope domain.RememberScope,
		decidedBy string,
	) (domain.Approval, error)
}

// consoleCommand is something typed in a console channel that is an
// instruction to this program rather than a message for the agent.
type consoleCommand struct {
	verb string
	arg  string
}

// parseConsoleCommand recognises the few things a console channel can be told.
//
// Deliberately a short, closed list matched on the whole message. Anything
// else is a question for the agent, and a channel where an ordinary sentence
// might be swallowed as a command is one nobody can talk in.
func parseConsoleCommand(text string) (consoleCommand, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || len(fields) > 2 {
		return consoleCommand{}, false
	}

	verb := strings.ToLower(fields[0])
	var arg string
	if len(fields) == 2 {
		arg = fields[1]
	}

	switch verb {
	case "approve", "allow", "yes":
		return consoleCommand{verb: "approve", arg: arg}, arg != ""
	case "deny", "reject", "no":
		return consoleCommand{verb: "deny", arg: arg}, arg != ""
	case "pending", "approvals":
		return consoleCommand{verb: "pending"}, arg == ""
	case "help", "motd":
		return consoleCommand{verb: "help"}, arg == ""
	}
	return consoleCommand{}, false
}

// handleConsole runs a command typed in a console channel.
//
// The reply is queued rather than returned, because the person typing is
// watching a channel and not a terminal.
func (i *Ingress) handleConsole(
	ctx context.Context,
	message InboundMessage,
	binding Binding,
	session domain.SessionID,
	command consoleCommand,
) error {
	if command.verb == "help" {
		return i.say(ctx, message, session, consoleMOTD(binding))
	}

	if i.Console == nil {
		return i.say(ctx, message, session,
			"This daemon was not started with console commands available.")
	}

	// Only this channel's own conversation. A console decides for the work it
	// can see, not for a run somebody started at the machine: the point of the
	// channel is remote control of its own conversations, and reaching past
	// them would make every bound channel a way to approve anything.
	waiting, err := i.Console.PendingApprovals(ctx, session)
	if err != nil {
		return err
	}

	switch command.verb {
	case "pending":
		return i.say(ctx, message, session, renderPending(waiting))

	case "approve", "deny":
		return i.decide(ctx, message, binding, session, waiting, command)
	}

	return nil
}

func (i *Ingress) decide(
	ctx context.Context,
	message InboundMessage,
	binding Binding,
	session domain.SessionID,
	waiting []domain.Approval,
	command consoleCommand,
) error {
	var found *domain.Approval
	for index, approval := range waiting {
		if string(approval.ID) == command.arg {
			found = &waiting[index]
			break
		}
	}
	if found == nil {
		// Said plainly rather than as a failure. The usual cause is an
		// approval that has already been decided, or one belonging to another
		// conversation.
		return i.say(ctx, message, session, fmt.Sprintf(
			"Nothing here is waiting on %s. Type `pending` to see what is.", command.arg))
	}

	allow := command.verb == "approve"

	// Recorded as the platform account that typed it, not as the operator
	// running the daemon. Who decided is part of what the log is for.
	decidedBy := message.Principal.ID
	if decidedBy == "" {
		decidedBy = string(binding.Platform)
	}

	if _, err := i.Console.DecideApproval(ctx, found.ID, allow, domain.RememberOnce, decidedBy); err != nil {
		return err
	}

	verb := "Denied"
	if allow {
		verb = "Approved"
	}
	return i.say(ctx, message, session, fmt.Sprintf("%s `%s`.", verb, found.ToolName))
}

func renderPending(waiting []domain.Approval) string {
	if len(waiting) == 0 {
		return "Nothing is waiting."
	}

	var out strings.Builder
	out.WriteString("Waiting:\n")
	for _, approval := range waiting {
		fmt.Fprintf(&out, "- `%s` — %s\n", approval.ID, approval.ToolName)
	}
	out.WriteString("\nReply `approve <id>` or `deny <id>`.")
	return out.String()
}

// say queues a message from this program, as opposed to from the agent.
//
// It carries no session or run, because it belongs to neither: it is this
// program answering somebody who typed at it, and putting it in a session's
// history would make the agent read its own console as conversation.
func (i *Ingress) say(
	ctx context.Context,
	message InboundMessage,
	session domain.SessionID,
	text string,
) error {
	if i.NewDispatchID == nil {
		// Refused rather than panicked. An ingress wired without this can
		// still accept work; it simply cannot speak on its own behalf, and a
		// named error says which of those it is.
		return errors.New("gateway: this ingress cannot post on its own behalf")
	}

	encoded, err := json.Marshal(MessagePayload{Text: text, Final: true})
	if err != nil {
		return err
	}

	ref := message.Conversation
	_, err = i.Store.EnqueueDispatch(ctx, Dispatch{
		ID:        i.NewDispatchID(),
		AccountID: ref.AccountID,
		SessionID: session,
		Target:    ref,
		Kind:      DispatchMessage,
		Payload:   string(encoded),
		CreatedAt: i.now(),
	})
	return err
}

// consoleMOTD says what this channel can and cannot do.
//
// Posted where the rule applies rather than left in a configuration file on a
// machine nobody in the channel is sitting at. A boundary somebody has to go
// and look up is one they will assume the shape of instead.
func consoleMOTD(binding Binding) string {
	var out strings.Builder

	out.WriteString("**This channel is a console.**\n")
	out.WriteString("It can read and search the workspace, fetch web pages, and read what I remember.\n")
	out.WriteString("Changes to files and to memory stop and ask, and you can answer here: ")
	out.WriteString("`pending` lists what is waiting, `approve <id>` and `deny <id>` decide it.\n\n")

	out.WriteString("**It cannot run programs.** That needs somebody at the machine.\n")
	out.WriteString("Channel permissions decide who can see and type here, which is what makes the rest of ")
	out.WriteString("this safe. What they cannot decide is whether an account still belongs to its owner, ")
	out.WriteString("and a stolen one would hold both the request and the approval. ")
	out.WriteString("So running programs stays where somebody has to be present — which is also the only ")
	out.WriteString("place that can tell.\n")

	if len(binding.AllowedPrincipals) > 0 || len(binding.AllowedClaims) > 0 {
		out.WriteString("\nOnly listed accounts and roles are answered here.")
	}
	return out.String()
}
