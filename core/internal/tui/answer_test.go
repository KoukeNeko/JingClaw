package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// A run that stopped to ask says what it asked.
func TestItDrawsTheQuestionARunStoppedOn(t *testing.T) {
	drawn := drawSession(t, Screen{Asked: []Asked{{
		ID:     "qst_1",
		Prompt: "which branch should this go on",
		Kind:   domain.QuestionText,
	}}})

	if !strings.Contains(drawn, "which branch should this go on") {
		t.Errorf("the question is not on the screen:\n%s", drawn)
	}
}

// A question with options lists them.
//
// A model that offered a fixed set and a panel that asked for free text
// disagree about what an answer is, and the person typing finds out when the
// run rejects theirs.
func TestItListsTheOptionsAQuestionOffers(t *testing.T) {
	drawn := drawSession(t, Screen{Asked: []Asked{{
		ID: "qst_1", Prompt: "which one", Kind: domain.QuestionChoice,
		Options: []Option{
			{ID: "a", Label: "rebase onto main"},
			{ID: "b", Label: "merge it"},
		},
	}}})

	for _, expected := range []string{"rebase onto main", "merge it"} {
		if !strings.Contains(drawn, expected) {
			t.Errorf("%q is not among the options drawn:\n%s", expected, drawn)
		}
	}
}

// The line to type at is there only while something is waiting for it.
//
// The whole reason this is not a composer. A panel with a permanent input
// invites a turn to be typed into it, and a turn typed here would be a run
// with no origin.
func TestThereIsNowhereToTypeUntilSomethingAsks(t *testing.T) {
	quiet := drawSession(t, Screen{Messages: []Message{
		{Role: domain.RoleUser, Text: "run the tests"},
	}})
	if strings.Contains(quiet, "\n"+answerPrompt) {
		t.Errorf("a session with nothing waiting offered a line to type at:\n%s", quiet)
	}

	asking := drawSession(t, Screen{Asked: []Asked{{ID: "qst_1", Prompt: "which one"}}})
	if !strings.Contains(asking, "\n"+answerPrompt) {
		t.Errorf("a run waiting on an answer offered nowhere to type it:\n%s", asking)
	}
}

// Typing goes into the answer, and enter sends it.
func TestAnAnswerIsTypedAndSent(t *testing.T) {
	asked := &recordingSessions{}
	model := asking(t, asked)

	for _, letter := range "main" {
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}

	if drawn := view(model); !strings.Contains(drawn, "main") {
		t.Errorf("what was typed is not on the screen:\n%s", drawn)
	}

	runCommand(t, secondOf(model.Update(key("enter"))))

	if asked.answer.ID != "qst_1" {
		t.Fatalf("nothing was answered; got %q", asked.answer.ID)
	}
	if asked.answer.Text != "main" {
		t.Errorf("the answer sent was %q", asked.answer.Text)
	}
}

// An empty answer is not sent.
//
// A run waiting on a person is unblocked by an answer, and "" is one it
// cannot act on. Sending it turns a run parked on a question into a run that
// carried on with nothing.
func TestAnEmptyAnswerIsNotSent(t *testing.T) {
	asked := &recordingSessions{}
	model := asking(t, asked)

	runCommand(t, secondOf(model.Update(key("enter"))))

	if asked.answered != 0 {
		t.Errorf("an empty answer was sent %d time(s)", asked.answered)
	}
}

// Backspace takes a letter back.
func TestTypingCanBeTakenBack(t *testing.T) {
	asked := &recordingSessions{}
	model := asking(t, asked)

	for _, letter := range "mainxy" {
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}
	model = after(model, tea.KeyPressMsg{Code: tea.KeyBackspace})
	model = after(model, tea.KeyPressMsg{Code: tea.KeyBackspace})

	runCommand(t, secondOf(model.Update(key("enter"))))

	if asked.answer.Text != "main" {
		t.Errorf("after taking two letters back the answer was %q", asked.answer.Text)
	}
}

// While a question is waiting, letters are letters.
//
// The keys that decide are single letters, and a person typing an answer
// containing one of them must not allow a call by writing it. The decision
// keys are not offered while there is a line to type at.
func TestTypingAnAnswerDoesNotDecideAnything(t *testing.T) {
	both := &recordingSessions{}
	model := start(t, both, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{
		Waiting: []Waiting{{ID: "apr_1", ToolName: "exec_command"}},
		Asked:   []Asked{{ID: "qst_1", Prompt: "which one"}},
	}})

	for _, letter := range "add" {
		runCommand(t, secondOf(model.Update(tea.KeyPressMsg{Code: letter, Text: string(letter)})))
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}

	if both.decision.ID != "" {
		t.Errorf("typing an answer decided %q", both.decision.ID)
	}
}

// A key with nothing behind it types nothing.
//
// An arrow or a function key has a name and no text. Appending the name would
// put the word "up" into somebody's answer for pressing a key that moves a
// cursor they do not have.
func TestAKeyWithNoTextTypesNothing(t *testing.T) {
	asked := &recordingSessions{}
	model := asking(t, asked)

	for _, letter := range "ma" {
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}
	model = after(model, tea.KeyPressMsg{Code: tea.KeyUp})
	model = after(model, tea.KeyPressMsg{Code: tea.KeyRight})
	for _, letter := range "in" {
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}

	runCommand(t, secondOf(model.Update(key("enter"))))

	if asked.answer.Text != "main" {
		t.Errorf("pressing arrows while typing gave %q", asked.answer.Text)
	}
}

// A question asked while somebody is watching reaches the screen.
//
// The screens above are handed a question already on them, which checks the
// drawing and not the folding. A run that stops to ask after the panel is
// open is the case that matters: nobody is going to reopen the session to
// find out that it did.
func TestAQuestionAskedWhileWatchingReachesTheScreen(t *testing.T) {
	model := opened(t, Screen{HeadSeq: 4})

	model = after(model, arrived{update: Update{Event: &domain.Event{
		Seq:  5,
		Kind: domain.EventQuestionAsked,
		Payload: domain.QuestionAsked{
			QuestionID: "qst_1",
			Prompt:     "which branch should this go on",
			Kind:       domain.QuestionChoice,
			Options: []domain.QuestionOption{
				{ID: "a", Label: "rebase onto main"},
			},
		},
	}}})

	drawn := view(model)
	if !strings.Contains(drawn, "which branch should this go on") {
		t.Errorf("a question asked while watching is not on the screen:\n%s", drawn)
	}
	if !strings.Contains(drawn, "rebase onto main") {
		t.Errorf("the options it offered did not survive the fold:\n%s", drawn)
	}
	if !strings.Contains(drawn, "\n"+answerPrompt) {
		t.Errorf("it asked and there is nowhere to answer:\n%s", drawn)
	}
}

// A question that has been answered stops waiting.
//
// Otherwise the line stays on the screen after the run has moved on, and the
// next thing typed is sent against a question nobody is waiting on.
func TestAnAnsweredQuestionStopsWaiting(t *testing.T) {
	model := opened(t, Screen{Asked: []Asked{{ID: "qst_1", Prompt: "which branch"}}})

	model = after(model, arrived{update: Update{Event: &domain.Event{
		Seq:     6,
		Kind:    domain.EventQuestionAnswered,
		Payload: domain.QuestionAnswered{QuestionID: "qst_1", Answer: "main"},
	}}})

	if drawn := view(model); strings.Contains(drawn, "which branch") {
		t.Errorf("an answered question is still being asked:\n%s", drawn)
	}
}

// Escape still leaves, rather than being taken as text.
func TestEscapeStillLeavesWhileAnswering(t *testing.T) {
	model := asking(t, &recordingSessions{})

	model, command := model.Update(key("esc"))
	if isQuit(command) {
		t.Fatal("escape while answering quit the panel")
	}
	// The question, not the prompt: the list marks its cursor with the same
	// two characters, so looking for those would find the list's own.
	if drawn := view(model); strings.Contains(drawn, "which branch") {
		t.Errorf("escape stayed in the session:\n%s", drawn)
	}
}

// An answer the daemon refused is said, not swallowed.
func TestARefusedAnswerIsSaidOutLoud(t *testing.T) {
	refusing := &recordingSessions{refuse: errRefused}
	model := asking(t, refusing)

	model = after(model, tea.KeyPressMsg{Code: 'x', Text: "x"})
	model, command := model.Update(key("enter"))
	model = after(model, runCommand(t, command))

	if drawn := view(model); !strings.Contains(drawn, errRefused.Error()) {
		t.Errorf("a refused answer drew:\n%s", drawn)
	}
}

// An answer that was sent clears the line.
//
// Otherwise the next question opens with the previous answer already in it,
// and enter sends it before anybody has read what is being asked.
func TestTheLineIsClearedOnceAnAnswerIsSent(t *testing.T) {
	asked := &recordingSessions{}
	model := asking(t, asked)

	for _, letter := range "main" {
		model = after(model, tea.KeyPressMsg{Code: letter, Text: string(letter)})
	}
	model = after(model, key("enter"))

	if drawn := view(model); strings.Contains(drawn, answerPrompt+"main") {
		t.Errorf("the answer stayed on the line after being sent:\n%s", drawn)
	}
}

// asking is a panel showing a session with a question outstanding.
func asking(t *testing.T, sessions *recordingSessions) tea.Model {
	t.Helper()

	model := start(t, sessions, listed{sessions: []Summary{{ID: "ses_1"}}})
	return after(model, showing{id: "ses_1", screen: Screen{
		Asked: []Asked{{ID: "qst_1", Prompt: "which branch", Kind: domain.QuestionText}},
	}})
}
