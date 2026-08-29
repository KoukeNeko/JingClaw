package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/fake"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

func waitForQuestion(t *testing.T, rt *runtime.Runtime, session domain.SessionID) domain.Question {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := rt.PendingQuestions(context.Background(), session)
		if err != nil {
			t.Fatalf("read questions: %v", err)
		}
		if len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the agent never asked anything")
	return domain.Question{}
}

// waitForStatus polls until a run reaches a state, which is not the same as
// waiting for it to finish: a run parked on a question never finishes on its
// own, and that is exactly what is being checked.
func waitForStatus(t *testing.T, store *memory.Store, run domain.RunID, want domain.RunStatus) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var last domain.RunStatus
	for time.Now().Before(deadline) {
		got, err := store.Run(context.Background(), run)
		if err == nil {
			last = got.Status
			if got.Status == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the run is %q, want %q", last, want)
}

const askForStrategy = `{"prompt":"Which migration strategy?","kind":"choice",` +
	`"options":[{"id":"a","label":"keep the schema compatible"},{"id":"b","label":"upgrade in place"}]}`

// A run that asks must stop and say it is waiting for an answer, not for an
// approval: every client offers a different control for the two.
func TestAskingParksTheRunWaitingForAnAnswer(t *testing.T) {
	rt, store := newScriptedRuntime(t, []fake.Turn{
		{Text: "I need to know something.", Tool: "ask_user", Args: askForStrategy},
		{Text: "Right, doing that."},
	})
	ctx := context.Background()

	session, err := rt.CreateSession(ctx, "asking")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runID, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "migrate the database"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	question := waitForQuestion(t, rt, session.ID)
	if question.Prompt != "Which migration strategy?" {
		t.Errorf("the question is %q", question.Prompt)
	}
	if len(question.Options) != 2 {
		t.Errorf("the question carries %d options", len(question.Options))
	}

	waitForStatus(t, store, runID, domain.RunAwaitingInput)
}

// The answer reaches the model as the tool's result, which is what makes this
// different from the person simply sending another turn.
func TestTheAnswerComesBackAsTheToolResult(t *testing.T) {
	rt, store := newScriptedRuntime(t, []fake.Turn{
		{Text: "I need to know something.", Tool: "ask_user", Args: askForStrategy},
		{Text: "Right, doing that."},
	})
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "asking")
	runID, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "migrate the database"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	question := waitForQuestion(t, rt, session.ID)
	if _, err := rt.AnswerQuestion(ctx, question.ID, "b", "test"); err != nil {
		t.Fatalf("answer: %v", err)
	}

	waitForStatus(t, store, runID, domain.RunCompleted)

	events, err := store.ListAfter(ctx, session.ID, 0, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	var result string
	for _, one := range events {
		if completed, ok := one.Payload.(domain.ToolCallCompleted); ok &&
			completed.Name == "ask_user" {
			result = completed.Content
		}
	}
	if !strings.Contains(result, "upgrade in place") {
		t.Errorf("the model was not told what was chosen: %q", result)
	}
}

// A choice must be answered with one of the options. A model that listed
// three and is handed a fourth has been answered with something it has no way
// to interpret.
func TestAChoiceRefusesAnAnswerThatIsNotOnOffer(t *testing.T) {
	rt, _ := newScriptedRuntime(t, []fake.Turn{
		{Text: "I need to know something.", Tool: "ask_user", Args: askForStrategy},
		{Text: "Right."},
	})
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "asking")
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "migrate"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	question := waitForQuestion(t, rt, session.ID)
	if _, err := rt.AnswerQuestion(ctx, question.ID, "c", "test"); err == nil {
		t.Error("an answer that was not on offer was accepted")
	}

	// The labels work too: somebody answering from a chat channel types what
	// they read rather than the id beside it.
	if _, err := rt.AnswerQuestion(ctx, question.ID, "upgrade in place", "test"); err != nil {
		t.Errorf("answering with the label was refused: %v", err)
	}
}

// An empty answer is not an answer. A run resumed with nothing has been told
// the person had no opinion, and that is not what silence means.
func TestAnEmptyAnswerIsRefused(t *testing.T) {
	rt, _ := newScriptedRuntime(t, []fake.Turn{
		{Text: "Asking.", Tool: "ask_user",
			Args: `{"prompt":"Which branch?","kind":"text"}`},
		{Text: "Right."},
	})
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "asking")
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "push it"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	question := waitForQuestion(t, rt, session.ID)
	if _, err := rt.AnswerQuestion(ctx, question.ID, "   ", "test"); !errors.Is(err, runtime.ErrNoAnswer) {
		t.Errorf("an empty answer was accepted: %v", err)
	}
}

// Two clients answering the same prompt at the same moment must not resume
// the run twice.
func TestAQuestionIsAnsweredOnce(t *testing.T) {
	rt, _ := newScriptedRuntime(t, []fake.Turn{
		{Text: "Asking.", Tool: "ask_user",
			Args: `{"prompt":"Which branch?","kind":"text"}`},
		{Text: "Right."},
	})
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "asking")
	if _, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "push it"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	question := waitForQuestion(t, rt, session.ID)
	if _, err := rt.AnswerQuestion(ctx, question.ID, "main", "first"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if _, err := rt.AnswerQuestion(ctx, question.ID, "other", "second"); !errors.Is(err, storage.ErrQuestionAnswered) {
		t.Errorf("the same question was answered twice: %v", err)
	}
}

// A question a model got wrong is refused with a reason it can act on, rather
// than parking the run on something nobody can answer.
func TestAMalformedQuestionIsRefusedRatherThanAsked(t *testing.T) {
	rt, store := newScriptedRuntime(t, []fake.Turn{
		// A choice with one option is not a choice.
		{Text: "Asking.", Tool: "ask_user",
			Args: `{"prompt":"Which?","kind":"choice","options":[{"id":"a","label":"only one"}]}`},
		{Text: "Never mind."},
	})
	ctx := context.Background()

	session, _ := rt.CreateSession(ctx, "asking")
	runID, _, err := rt.SendTurnTo(ctx, session.ID, domain.Turn{Text: "decide"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// It finishes rather than parking: nothing was asked, so nothing is
	// waiting.
	waitForStatus(t, store, runID, domain.RunCompleted)

	pending, err := rt.PendingQuestions(ctx, session.ID)
	if err != nil {
		t.Fatalf("read questions: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a question nobody can answer was asked: %+v", pending)
	}
}
