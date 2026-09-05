package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/console"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// run does what was typed. It reports whether the console should close.
//
// Every command goes through the same path, including the ones that seem too
// small to need it. A console that answered some of them itself would be a
// second place where what is allowed is decided, and the two would drift.
func (s *session) run(ctx context.Context, line string) bool {
	command, ok, err := console.Parse(line)
	if err != nil {
		s.say(err.Error())
		return false
	}
	if !ok {
		return false
	}

	switch command.Verb {
	case "help":
		s.say(console.Help())

	case "quit":
		// What actually happens, which is not the same in both places a
		// console is opened from. Saying "it keeps running" where it does not
		// is worse than saying nothing.
		if s.leaving == StopsIt {
			s.say("stopping JingClaw.")
		} else {
			s.say("leaving the console; JingClaw keeps running.")
		}
		return true

	case "approvals":
		s.showApprovals(ctx)

	case "approve", "deny":
		s.decide(ctx, command)

	case "show":
		s.showOneApproval(ctx, command.Arg(0))

	case "open":
		s.openStoredOutput(ctx, command.Arg(0))

	case "questions":
		s.showQuestions(ctx)

	case "answer":
		s.answer(ctx, command)

	case "sessions":
		s.showSessions(ctx)

	case "processes":
		s.showProcesses(ctx)

	case "focus":
		s.focus(command.Arg(0))

	case "queue":
		s.showQueue(ctx)

	case "interrupt":
		s.interrupt(ctx, command)

	case "stop":
		if s.leaving == StopsIt {
			s.say("stopping JingClaw.")
			return true
		}
		s.say("this console did not start it; use jingclaw stop.")

	default:
		// A verb the table has and this switch does not. Said rather than
		// ignored, because the parser accepted it: a command that is listed
		// in help, completes, and then does nothing is worse than one that
		// does not exist, and the person typing it has no way to tell which
		// they are looking at.
		s.say("`" + command.Verb + "` is listed and not wired up; this is a bug.")
	}

	return false
}

// meta names this console on every request, so what it decides is recorded as
// having been decided here rather than by whatever else talks to the daemon.
func meta() *controlv1.RequestMeta {
	return &controlv1.RequestMeta{ClientId: clientName}
}

func (s *session) showSessions(ctx context.Context) {
	listed, err := s.daemon.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		s.say("could not list the sessions: " + err.Error())
		return
	}

	sessions := listed.Msg.GetSessions()
	if len(sessions) == 0 {
		s.say("no sessions yet.")
		return
	}

	for _, session := range sessions {
		title := session.GetTitle()
		if title == "" {
			title = "(untitled)"
		}
		s.say(fmt.Sprintf("  %-24s  %s", session.GetId(), title))
	}
}

func (s *session) showApprovals(ctx context.Context) {
	waiting, err := s.everyApproval(ctx)
	if err != nil {
		s.say("could not read what is waiting: " + err.Error())
		return
	}
	if len(waiting) == 0 {
		s.say("nothing is waiting for a decision.")
		return
	}

	for _, approval := range waiting {
		// The arguments as they were asked for, never a summary of them.
		// Deciding whether to run something means deciding about that thing.
		s.say(fmt.Sprintf("  %s  %s  %s",
			approval.GetId(), approval.GetToolName(),
			console.Clip(approval.GetArguments())))
		if effects := approval.GetEffects(); len(effects) > 0 {
			s.say("      " + strings.Join(effects, ", "))
		}
		if approval.GetReadForeign() {
			s.say("      this run read text from outside this machine")
		}
	}
}

// showOneApproval prints the whole of one waiting call.
//
// The listing clips, because a list of full commands is a list nobody reads.
// This is where the clipping stops: deciding whether to run something means
// deciding about that thing, and a decision made against the first seventy
// characters of it is a decision about a prefix.
func (s *session) showOneApproval(ctx context.Context, id string) {
	if id == "" {
		s.say("which one? " + console.Usage("show"))
		return
	}

	waiting, err := s.everyApproval(ctx)
	if err != nil {
		s.say("could not read what is waiting: " + err.Error())
		return
	}

	for _, approval := range waiting {
		if approval.GetId() != id {
			continue
		}
		s.sayApprovalInFull(approval)
		return
	}
	s.say("nothing is waiting under that id; " + console.Usage("approvals"))
}

// sayApprovalInFull is one call, unclipped.
func (s *session) sayApprovalInFull(approval *controlv1.Approval) {
	s.say(approval.GetId() + "  " + approval.GetToolName())
	if summary := approval.GetSummary(); summary != "" {
		s.say("  " + summary)
	}

	// The arguments as well as the rendering. The rendering is what a person
	// reads; the arguments are what will actually run, and a decision made
	// against a rendering that disagreed with them would be a decision about
	// something else.
	if preview := approval.GetPreview(); preview != "" {
		s.sayEachLine(preview)
	}
	s.say("  arguments:")
	s.sayEachLine(approval.GetArguments())

	for _, effect := range approval.GetEffects() {
		s.say("  · " + effect)
	}
	if approval.GetReadForeign() {
		// The one thing the person deciding cannot see for themselves: the
		// request looks the same whether the agent arrived at it or a page it
		// read suggested it, and only the log knows.
		s.say("  this run read text from outside this machine before asking")
	}
}

// sayEachLine prints something multi-line as lines of the log.
//
// One call per line rather than one call with newlines in it, because the
// screen owns the bottom line and redraws it around whatever it prints.
func (s *session) sayEachLine(text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		s.say("  " + line)
	}
}

// openStoredOutput writes stored output out and hands it to the machine.
//
// Not drawn in the log. A terminal is a poor image viewer and a worse PDF
// reader, and what somebody wants when a build fails is the log in the thing
// they read logs in.
func (s *session) openStoredOutput(ctx context.Context, id string) {
	stored := s.lastStored()
	if id != "" && !strings.HasPrefix(stored.id, id) {
		s.say("this console has only seen " + shortOrNothing(stored.id) +
			" go past; open it with `open`, or read it with `agent artifact`.")
		return
	}
	if stored.id == "" {
		s.say("no stored output has gone past yet.")
		return
	}

	extension, openable := console.ExtensionFor(stored.mediaType)
	if !openable {
		// Refused rather than opened as something else. An artifact is
		// whatever a tool produced, which includes whatever a page the run
		// read suggested it produce, and handing that to the machine's
		// default program for it is running somebody else's file.
		s.say(fmt.Sprintf("%s is a %s, which this console will not hand to the machine.",
			stored.id, orUnknown(stored.mediaType)))
		return
	}

	data, err := s.readArtifact(ctx, stored.id)
	if err != nil {
		s.say("could not read it: " + err.Error())
		return
	}

	path, err := console.WriteForOpening(s.into, stored.id, extension, data)
	if err != nil {
		s.say(err.Error())
		return
	}
	if err := s.opener.Open(ctx, path); err != nil {
		s.say("could not open it: " + err.Error())
		return
	}
	s.say("opened " + path)
}

// readArtifact reads stored output back whole.
//
// Whole because what it is for is handing to another program, and half a
// build log is a file that opens and says the wrong thing.
func (s *session) readArtifact(ctx context.Context, id string) ([]byte, error) {
	if s.artifacts == nil {
		return nil, errors.New("this console cannot reach stored output")
	}

	stream, err := s.artifacts.ReadArtifact(ctx, connect.NewRequest(
		&controlv1.ReadArtifactRequest{Id: id, Limit: maxArtifactBytes}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var read []byte
	for stream.Receive() {
		read = append(read, stream.Msg().GetChunk()...)
		if len(read) > maxArtifactBytes {
			// Said rather than truncated. A log cut off at an arbitrary point
			// opens and says the wrong thing, and nothing on the screen would
			// show that the end is missing.
			return nil, fmt.Errorf("it is larger than this console will write out (%d bytes)",
				maxArtifactBytes)
		}
	}
	return read, stream.Err()
}

// maxArtifactBytes bounds what the console will write out to be opened.
//
// Generous, because the thing this exists for is a build log nobody wants
// truncated, and bounded because "whatever a tool produced" is not a size.
const maxArtifactBytes = 64 << 20

func orUnknown(mediaType string) string {
	if mediaType == "" {
		return "kind of file this console was not told"
	}
	return mediaType
}

func shortOrNothing(id string) string {
	if id == "" {
		return "nothing"
	}
	return id
}

// everyApproval gathers what is waiting across every session.
//
// The daemon answers per session, and the console is not looking at one.
func (s *session) everyApproval(ctx context.Context) ([]*controlv1.Approval, error) {
	sessions, err := s.daemon.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		return nil, err
	}

	var waiting []*controlv1.Approval
	for _, session := range sessions.Msg.GetSessions() {
		if focused := s.focusedOn(); focused != "" && domain.SessionID(session.GetId()) != focused {
			continue
		}
		listed, err := s.daemon.ListApprovals(ctx, connect.NewRequest(
			&controlv1.ListApprovalsRequest{SessionId: session.GetId()}))
		if err != nil {
			return nil, err
		}
		waiting = append(waiting, listed.Msg.GetApprovals()...)
	}
	return waiting, nil
}

func (s *session) decide(ctx context.Context, command console.Command) {
	id := command.Arg(0)
	if id == "" {
		s.say(command.Verb + " needs the id of something waiting; type approvals to see them.")
		return
	}

	decision := controlv1.ApprovalDecision_APPROVAL_DECISION_DENY
	if command.Verb == "approve" {
		decision = controlv1.ApprovalDecision_APPROVAL_DECISION_ALLOW
	}

	if _, err := s.daemon.DecideApproval(ctx, connect.NewRequest(&controlv1.DecideApprovalRequest{
		Meta:       meta(),
		ApprovalId: id,
		Decision:   decision,
		Remember:   controlv1.RememberScope_REMEMBER_SCOPE_ONCE,
	})); err != nil {
		s.say("could not decide it: " + err.Error())
		return
	}
	s.say(command.Verb + "d " + id)
}

func (s *session) showQuestions(ctx context.Context) {
	sessions, err := s.daemon.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		s.say("could not list the sessions: " + err.Error())
		return
	}

	found := 0
	for _, session := range sessions.Msg.GetSessions() {
		if focused := s.focusedOn(); focused != "" && domain.SessionID(session.GetId()) != focused {
			continue
		}
		listed, err := s.daemon.ListQuestions(ctx, connect.NewRequest(
			&controlv1.ListQuestionsRequest{SessionId: session.GetId()}))
		if err != nil {
			s.say("could not read the questions: " + err.Error())
			return
		}
		for _, question := range listed.Msg.GetQuestions() {
			found++
			s.say(fmt.Sprintf("  %s  %s", question.GetId(), console.Clip(question.GetPrompt())))
			for _, option := range question.GetOptions() {
				s.say(fmt.Sprintf("      %s  %s", option.GetId(), option.GetLabel()))
			}
		}
	}

	if found == 0 {
		s.say("nothing is waiting on an answer.")
	}
}

func (s *session) answer(ctx context.Context, command console.Command) {
	id, text := command.Arg(0), command.Rest()
	if id == "" || text == "" {
		s.say("answer needs a question and an answer: answer <id> <text>")
		return
	}

	if _, err := s.daemon.AnswerQuestion(ctx, connect.NewRequest(&controlv1.AnswerQuestionRequest{
		Meta:       meta(),
		QuestionId: id,
		Answer:     text,
	})); err != nil {
		s.say("could not answer it: " + err.Error())
		return
	}
	s.say("answered " + id)
}

// showQueue lists the messages waiting their turn, with the run id that
// interrupt takes to pull one out of line.
func (s *session) showQueue(ctx context.Context) {
	sessions, err := s.daemon.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		s.say("could not list the sessions: " + err.Error())
		return
	}

	found := 0
	for _, session := range sessions.Msg.GetSessions() {
		if focused := s.focusedOn(); focused != "" && domain.SessionID(session.GetId()) != focused {
			continue
		}
		listed, err := s.daemon.ListRuns(ctx, connect.NewRequest(
			&controlv1.ListRunsRequest{SessionId: session.GetId()}))
		if err != nil {
			s.say("could not read the line: " + err.Error())
			return
		}
		for _, run := range listed.Msg.GetRuns() {
			if run.GetStatus() != controlv1.RunStatus_RUN_STATUS_QUEUED {
				continue
			}
			found++
			s.say(fmt.Sprintf("  %s  %s  waiting %s  %s",
				run.GetId(), session.GetId(),
				time.Since(run.GetCreatedAt().AsTime()).Round(time.Second),
				sentFrom(run.GetOrigin())))
		}
	}

	if found == 0 {
		s.say("nothing is waiting in line.")
	}
}

// sentFrom says where a run's message came from, for a listing.
func sentFrom(origin *controlv1.RunOrigin) string {
	if principal := origin.GetPrincipal(); principal != nil {
		who := principal.GetDisplayName()
		if who == "" {
			who = principal.GetPrincipalId()
		}
		return "from " + who + " on " + principal.GetPlatform()
	}
	if origin.GetClientId() != "" {
		return "from " + origin.GetClientId()
	}
	return ""
}

func (s *session) interrupt(ctx context.Context, command console.Command) {
	id := command.Arg(0)
	if id == "" {
		s.say("interrupt needs the run to stop.")
		return
	}

	if _, err := s.daemon.InterruptRun(ctx, connect.NewRequest(&controlv1.InterruptRunRequest{
		Meta:   meta(),
		RunId:  id,
		Reason: "asked to stop at the console",
	})); err != nil {
		s.say("could not interrupt it: " + err.Error())
		return
	}
	s.say("asked " + id + " to stop")
}

// focus narrows the log to one session, or widens it again.
//
// Only what arrives after it. Lines already printed are part of the
// terminal's scrollback and belong to whoever is scrolling it.
func (s *session) focus(id string) {
	s.mu.Lock()
	s.focused = domain.SessionID(id)
	s.mu.Unlock()

	if id == "" {
		s.say("showing every session again.")
		return
	}
	s.say("showing " + id + " only; type focus with nothing to see them all.")
}

func (s *session) focusedOn() domain.SessionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.focused
}

// showProcesses lists what the agent has left running.
//
// Across every session, or one when the console is focused: a program started
// an hour ago in a conversation nobody is looking at is exactly the one worth
// being told about.
func (s *session) showProcesses(ctx context.Context) {
	sessions, err := s.daemon.ListSessions(ctx, connect.NewRequest(&controlv1.ListSessionsRequest{}))
	if err != nil {
		s.say("could not list the sessions: " + err.Error())
		return
	}

	found := 0
	for _, session := range sessions.Msg.GetSessions() {
		if focused := s.focusedOn(); focused != "" && domain.SessionID(session.GetId()) != focused {
			continue
		}

		listed, err := s.daemon.ListProcesses(ctx, connect.NewRequest(
			&controlv1.ListProcessesRequest{SessionId: session.GetId()}))
		if err != nil {
			s.say("could not read what is running: " + err.Error())
			return
		}

		for _, one := range listed.Msg.GetProcesses() {
			found++
			state := "running"
			if !one.GetRunning() {
				state = fmt.Sprintf("exit %d", one.GetExitCode())
			}
			s.say(fmt.Sprintf("  %s  pid %-7d %-10s %s",
				one.GetId(), one.GetPid(), state,
				console.Clip(one.GetProgram()+" "+strings.Join(one.GetArgs(), " "))))
		}
	}

	if found == 0 {
		s.say("nothing is running.")
	}
}
