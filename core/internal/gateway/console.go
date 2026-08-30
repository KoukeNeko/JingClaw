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

// DecidingRuntime is the extra reach needed to answer an approval or a
// question, rather than only to start work.
//
// Separate from Runtime, and optional, because Runtime is deliberately narrow:
// an ingress serving ordinary channels may start work and nothing else. Adding
// approvals to that interface would hand every channel the ability to resolve
// them. Kept apart, an ingress without this cannot decide anything, and the
// widening is visible at the point somebody wires it in.
//
// Holding it is not the same as being allowed to use it. Two separate gates
// decide that, and they identify the decider differently: a console channel,
// where the room itself is the credential and a typed command is enough; and
// a named approver in a shared room, who is identified by the platform when
// they press a button and never by what they type.
type DecidingRuntime interface {
	PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error)

	DecideApproval(
		ctx context.Context,
		id domain.ApprovalID,
		allow bool,
		scope domain.RememberScope,
		decidedBy domain.RunOrigin,
	) (domain.Approval, error)

	PendingQuestions(ctx context.Context, session domain.SessionID) ([]domain.Question, error)

	AnswerQuestion(
		ctx context.Context,
		id domain.QuestionID,
		answer string,
		answeredBy domain.RunOrigin,
	) (domain.Question, error)
}

// whoTyped is who to record for something a console channel was told to do.
//
// A console binding is a room that is itself the credential: a typed command
// is enough, and the platform is not asked who typed it. So the principal is
// taken when the platform happened to supply one, and otherwise the room is
// what is recorded — the room being what was authorised.
//
// Not the platform's name on its own, which was what this did before. "This
// was decided by discord" says only that it happened somewhere on Discord,
// which of every channel and every account is true.
func whoTyped(message InboundMessage) domain.RunOrigin {
	if message.Principal.ID != "" {
		return domain.FromAPlatformAccount(
			string(message.Principal.Platform),
			message.Principal.ID,
			message.Principal.DisplayName,
		)
	}
	return domain.FromAChannel(
		string(message.Conversation.Platform),
		message.Conversation.ChannelID,
	)
}

// consoleCommand is something typed in a console channel that is an
// instruction to this program rather than a message for the agent.
type consoleCommand struct {
	verb string
	arg  string

	// rest is everything after the argument, for the one command whose
	// answer is a sentence rather than an id.
	rest string
}

// parseConsoleCommand recognises the few things a console channel can be told.
//
// Deliberately a short, closed list matched on the whole message. Anything
// else is a question for the agent, and a channel where an ordinary sentence
// might be swallowed as a command is one nobody can talk in.
func parseConsoleCommand(text string) (consoleCommand, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return consoleCommand{}, false
	}

	// One command takes words rather than a single argument, because an
	// answer is a sentence. It is matched before the length check that keeps
	// every other command from swallowing an ordinary message.
	if strings.EqualFold(fields[0], "answer") && len(fields) >= 3 {
		return consoleCommand{
			verb: "answer",
			arg:  fields[1],
			rest: strings.Join(fields[2:], " "),
		}, true
	}

	if len(fields) > 2 {
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
	case "questions", "asked":
		return consoleCommand{verb: "questions"}, arg == ""
	case "artifact", "file":
		return consoleCommand{verb: "artifact", arg: arg}, arg != ""
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

	// Handing something over needs the artifact store, not the approval
	// machinery, so it is answered before the check for the latter.
	if command.verb == "artifact" {
		return i.sendArtifact(ctx, message, session, command.arg)
	}

	if i.Decisions == nil {
		return i.say(ctx, message, session,
			"This daemon was not started with console commands available.")
	}

	// Only this channel's own conversation. A console decides for the work it
	// can see, not for a run somebody started at the machine: the point of the
	// channel is remote control of its own conversations, and reaching past
	// them would make every bound channel a way to approve anything.
	if command.verb == "questions" || command.verb == "answer" {
		asked, err := i.Decisions.PendingQuestions(ctx, session)
		if err != nil {
			return err
		}
		if command.verb == "questions" {
			return i.say(ctx, message, session, renderAsked(asked))
		}
		return i.answer(ctx, message, session, asked, command)
	}

	waiting, err := i.Decisions.PendingApprovals(ctx, session)
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

// answer settles a question from the channel that was asked it.
func (i *Ingress) answer(
	ctx context.Context,
	message InboundMessage,
	session domain.SessionID,
	asked []domain.Question,
	command consoleCommand,
) error {
	var found *domain.Question
	for index, question := range asked {
		if strings.EqualFold(string(question.ID), command.arg) {
			found = &asked[index]
			break
		}
	}
	if found == nil {
		return i.say(ctx, message, session, fmt.Sprintf(
			"Nothing here is waiting on %s. Type `questions` to see what is.", command.arg))
	}

	// Recorded as the platform account that typed it, not as the operator
	// running the daemon. Who answered is part of what the log is for.
	answeredBy := whoTyped(message)

	answered, err := i.Decisions.AnswerQuestion(ctx, found.ID, command.rest, answeredBy)
	if err != nil {
		return i.say(ctx, message, session, fmt.Sprintf("That did not work: %v", err))
	}

	return i.say(ctx, message, session,
		fmt.Sprintf("Answered %s: %s", answered.ID, answered.Answer))
}

// renderAsked lists what the agent is waiting to be told.
func renderAsked(asked []domain.Question) string {
	if len(asked) == 0 {
		return "Nothing here is waiting on an answer."
	}

	var out strings.Builder
	for _, question := range asked {
		fmt.Fprintf(&out, "%s — %s\n", question.ID, question.Prompt)
		for _, option := range question.Options {
			fmt.Fprintf(&out, "  %s: %s\n", option.ID, option.Label)
		}
	}
	out.WriteString("\nReply `answer <id> <your answer>`.")
	return out.String()
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
	decidedBy := whoTyped(message)

	if _, err := i.Decisions.DecideApproval(ctx, found.ID, allow, domain.RememberOnce, decidedBy); err != nil {
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

// MaxConsoleFileBytes bounds what a console will hand over in one go.
//
// Well under what a platform accepts, because this travels through the
// dispatch queue and a queue is a poor place to keep megabytes.
const MaxConsoleFileBytes = 2 << 20

// sendArtifact hands over something a run stored, because somebody asked for
// it by name.
//
// Pull rather than push. A run that produces a large result tells the channel
// it exists and stops there; the bytes cross only when a person names the one
// they want. Attaching everything a run produced would put whole build logs
// and fetched pages into a room on the agent's initiative.
func (i *Ingress) sendArtifact(
	ctx context.Context,
	message InboundMessage,
	session domain.SessionID,
	id string,
) error {
	if i.Artifacts == nil {
		return i.say(ctx, message, session, "This daemon keeps no artifacts.")
	}

	reader, ok := i.Artifacts.(ArtifactReader)
	if !ok {
		return i.say(ctx, message, session, "This daemon cannot read artifacts back.")
	}

	ref, err := reader.Stat(id)
	if err != nil {
		return i.say(ctx, message, session, fmt.Sprintf(
			"There is nothing stored as %s.", id))
	}
	if ref.Size > MaxConsoleFileBytes {
		return i.say(ctx, message, session, fmt.Sprintf(
			"%s is %d bytes, over the %d this will hand over. Read it on the machine.",
			id, ref.Size, MaxConsoleFileBytes))
	}

	content, _, err := reader.ReadRange(id, 0, MaxConsoleFileBytes)
	if err != nil {
		return i.say(ctx, message, session, fmt.Sprintf("%s could not be read: %v", id, err))
	}

	return i.attach(ctx, message, session, MessageFile{
		Name:      artifactFilename(id, ref.MediaType),
		Content:   content,
		MediaType: ref.MediaType,
	})
}

// artifactFilename gives the attachment a name somebody can open.
//
// The digest is unreadable but unique, and the extension is what decides
// whether a platform shows the contents or offers a download.
func artifactFilename(id, mediaType string) string {
	name := strings.TrimPrefix(id, "sha256-")
	if len(name) > 12 {
		name = name[:12]
	}
	return name + extensionFor(mediaType)
}

func extensionFor(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "image/png"):
		return ".png"
	case strings.HasPrefix(mediaType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mediaType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mediaType, "application/json"):
		return ".json"
	default:
		return ".txt"
	}
}

// attach queues a file for the conversation.
func (i *Ingress) attach(
	ctx context.Context,
	message InboundMessage,
	session domain.SessionID,
	file MessageFile,
) error {
	if i.NewDispatchID == nil {
		return errors.New("gateway: this ingress cannot post on its own behalf")
	}

	encoded, err := json.Marshal(MessagePayload{Final: true, File: &file})
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
	out.WriteString("`pending` lists what is waiting, `approve <id>` and `deny <id>` decide it.\n")
	out.WriteString("`questions` lists what the agent has asked, `answer <id> <your answer>` tells it.\n")
	out.WriteString("`artifact <id>` hands over something a run stored.\n\n")

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
