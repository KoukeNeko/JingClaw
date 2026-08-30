package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// askArguments is what the ask_user tool was called with.
//
// Parsed here rather than in the tool, because here is where the call is
// settled: the tool's Execute is never reached for this one, since its result
// is a person's answer rather than anything a program computes.
type askArguments struct {
	Prompt  string
	Kind    domain.QuestionKind
	Options []domain.QuestionOption
}

// parseAsk reads and checks a call's arguments.
func parseAsk(arguments json.RawMessage) (askArguments, *tool.Error) {
	var wire struct {
		Prompt  string `json:"prompt"`
		Kind    string `json:"kind"`
		Options []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Detail string `json:"detail"`
		} `json:"options"`
	}
	if err := json.Unmarshal(arguments, &wire); err != nil {
		return askArguments{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	prompt := strings.TrimSpace(wire.Prompt)
	if prompt == "" {
		return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
			"Say what you want to know.", "there is no question")
	}

	kind := domain.QuestionKind(wire.Kind)
	if kind == "" {
		// A question with options is a choice whatever it was called, and one
		// without is text. Inferred rather than refused: getting this wrong
		// is a model being imprecise about a thing that is visible from the
		// arguments themselves.
		kind = domain.QuestionText
		if len(wire.Options) > 0 {
			kind = domain.QuestionChoice
		}
	}
	if !kind.IsValid() {
		return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
			`Use "choice" or "text".`, "%q is not a kind of question", wire.Kind)
	}

	options := make([]domain.QuestionOption, 0, len(wire.Options))
	seen := map[string]bool{}
	for _, one := range wire.Options {
		id := strings.TrimSpace(one.ID)
		label := strings.TrimSpace(one.Label)
		if id == "" || label == "" {
			return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
				"Every option needs an id and a label.", "an option is incomplete")
		}
		if seen[strings.ToLower(id)] {
			// Two options with one id cannot be told apart by the answer, so
			// whichever the person picked, the model would be told the other.
			return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
				"Give each option its own id.", "two options share the id %q", id)
		}
		seen[strings.ToLower(id)] = true
		options = append(options, domain.QuestionOption{ID: id, Label: label, Detail: one.Detail})
	}

	if kind == domain.QuestionChoice && len(options) < 2 {
		return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
			"Offer at least two options, or ask for text instead.",
			"a choice with %d options is not a choice", len(options))
	}
	if kind == domain.QuestionText && len(options) > 0 {
		return askArguments{}, tool.Errorf(tool.CodeInvalidArguments,
			`Use kind "choice" when you are offering options.`,
			"a text question was given options nobody can pick")
	}

	return askArguments{Prompt: prompt, Kind: kind, Options: options}, nil
}

// AskName is the tool that stops a run to ask a person something.
//
// Named here rather than only in the tool package because the runtime has to
// recognise it: this is the one tool whose result is not produced by running
// it, and settling the call means finding an answer somebody typed.
const AskName = "ask_user"

// ErrNoAnswer is returned for an answer that says nothing.
//
// An empty answer is refused rather than stored. A run resumed with nothing
// has been told the person had no opinion, and that is not what silence
// means: silence means nobody has answered yet.
var ErrNoAnswer = errors.New("runtime: an answer cannot be empty")

// PendingQuestions is what the agent has asked and nobody has answered.
func (r *Runtime) PendingQuestions(
	ctx context.Context,
	session domain.SessionID,
) ([]domain.Question, error) {
	return r.opts.Store.PendingQuestions(ctx, session)
}

// askQuestion records a question and announces it.
//
// The record goes to storage before the event, for the same reason an
// approval does: a client that reacts to the event by listing what is waiting
// must find it there.
func (r *Runtime) askQuestion(
	ctx context.Context,
	run domain.Run,
	call tool.Call,
	asked askArguments,
) error {
	question := domain.Question{
		ID:         domain.QuestionID(r.opts.NewQuestionID()),
		SessionID:  run.SessionID,
		RunID:      run.ID,
		ToolCallID: domain.ToolCallID(call.ID),
		Prompt:     asked.Prompt,
		Kind:       asked.Kind,
		Options:    asked.Options,
		Status:     domain.AnswerPending,
		CreatedAt:  r.opts.Now(),
	}

	if err := r.opts.Store.CreateQuestion(ctx, question); err != nil {
		return err
	}

	return r.append(ctx, run.SessionID, run.ID, domain.EventQuestionAsked,
		domain.QuestionAsked{
			QuestionID: question.ID,
			CallID:     question.ToolCallID,
			Prompt:     question.Prompt,
			Kind:       question.Kind,
			Options:    question.Options,
		})
}

// AnswerQuestion records what a person said and resumes the run.
//
// Settled in storage first, and storage refuses to settle it twice. That is
// what stops two clients answering the same prompt from resuming the run
// twice.
func (r *Runtime) AnswerQuestion(
	ctx context.Context,
	id domain.QuestionID,
	answer string,
	answeredBy domain.RunOrigin,
) (domain.Question, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return domain.Question{}, ErrNoAnswer
	}

	// Read before writing so a choice can be checked against what was
	// offered. A model that listed three options and is handed a fourth has
	// been answered with something it has no way to interpret.
	asked, err := r.opts.Store.Question(ctx, id)
	if err != nil {
		return domain.Question{}, err
	}
	if asked.Kind == domain.QuestionChoice {
		if err := checkChoice(asked, answer); err != nil {
			return domain.Question{}, err
		}
	}

	question, err := r.opts.Store.AnswerQuestion(
		ctx, id, domain.AnswerGiven, answer, answeredBy, r.opts.Now())
	if err != nil {
		return domain.Question{}, err
	}

	if err := r.append(ctx, question.SessionID, question.RunID, domain.EventQuestionAnswered,
		domain.QuestionAnswered{
			QuestionID: question.ID,
			CallID:     question.ToolCallID,
			Status:     question.Status,
			Answer:     question.Answer,
			AnsweredBy: answeredBy,
		}); err != nil {
		return domain.Question{}, err
	}

	if err := r.resume(ctx, question.RunID); err != nil {
		return domain.Question{}, err
	}

	return question, nil
}

// checkChoice refuses an answer that is not one of the options.
//
// Compared against the ids and the labels both, because a person answering
// from a chat channel types what they read rather than the id beside it.
func checkChoice(asked domain.Question, answer string) error {
	for _, option := range asked.Options {
		if strings.EqualFold(option.ID, answer) || strings.EqualFold(option.Label, answer) {
			return nil
		}
	}

	offered := make([]string, 0, len(asked.Options))
	for _, option := range asked.Options {
		offered = append(offered, option.ID)
	}
	return fmt.Errorf("runtime: %q is not one of the options (%s)",
		answer, strings.Join(offered, ", "))
}

// settleAsk resolves an ask_user call: the answer if there is one, otherwise a
// question and a pause.
//
// The one tool whose result is not produced by running it. Everything else
// here computes an answer; this one waits for a person to type one, which is
// why the runtime has to know the tool by name.
func (r *Runtime) settleAsk(
	ctx context.Context,
	run domain.Run,
	call tool.Call,
) (tool.Result, parked, error) {
	existing, err := r.opts.Store.QuestionForCall(ctx, run.ID, domain.ToolCallID(call.ID))
	switch {
	case err == nil && existing.Status == domain.AnswerGiven:
		return answeredResult(existing), notParked, nil
	case err == nil && existing.Status == domain.AnswerAbandoned:
		return tool.Result{
			Content: "Nobody answered, and the run this question belonged to ended.",
			IsError: true,
		}, notParked, nil
	case err == nil && existing.IsPending():
		// Already waiting; do not ask the same thing twice.
		return tool.Result{}, parkedForAnswer, nil
	case err != nil && !errors.Is(err, storage.ErrQuestionNotFound):
		return tool.Result{}, notParked, err
	}

	asked, argErr := parseAsk(call.Arguments)
	if argErr != nil {
		return argErr.Result(), notParked, nil
	}

	if err := r.askQuestion(ctx, run, call, asked); err != nil {
		return tool.Result{}, notParked, err
	}
	return tool.Result{}, parkedForAnswer, nil
}

// answeredResult is what the model is told when the answer arrives.
func answeredResult(question domain.Question) tool.Result {
	if question.Kind != domain.QuestionChoice {
		return tool.Result{
			Content: fmt.Sprintf("They answered: %s", question.Answer),
			Summary: "answered",
		}
	}

	// A chosen option is reported by label as well as by id: the id is what
	// the model matches on, and the label is what makes the log readable.
	for _, option := range question.Options {
		if strings.EqualFold(option.ID, question.Answer) ||
			strings.EqualFold(option.Label, question.Answer) {
			return tool.Result{
				Content: fmt.Sprintf("They chose %s: %s", option.ID, option.Label),
				Summary: "chose " + option.ID,
			}
		}
	}
	return tool.Result{
		Content: fmt.Sprintf("They answered: %s", question.Answer),
		Summary: "answered",
	}
}
