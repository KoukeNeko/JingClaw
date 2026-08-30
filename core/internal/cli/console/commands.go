package console

import (
	"context"
	"fmt"
	"strings"

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

	case "questions":
		s.showQuestions(ctx)

	case "answer":
		s.answer(ctx, command)

	case "sessions":
		s.showSessions(ctx)

	case "focus":
		s.focus(command.Arg(0))

	case "interrupt":
		s.interrupt(ctx, command)

	case "stop":
		if s.leaving == StopsIt {
			s.say("stopping JingClaw.")
			return true
		}
		s.say("this console did not start it; use jingclaw stop.")
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
