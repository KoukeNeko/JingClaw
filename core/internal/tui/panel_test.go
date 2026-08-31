package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// The panel is tested without a terminal.
//
// A Bubble Tea model is a function from a message to a model, so what it
// draws for a given sequence of messages can be checked directly. The PTY
// checks beside this one are about the terminal; these are about the screen.

// A list nobody could load says so.
//
// The failure that matters here is silence: a panel that swallowed the error
// would show an empty list, which is what a daemon with no sessions looks
// like, and the difference is whether anything is wrong.
func TestItSaysWhenItCannotReachTheDaemon(t *testing.T) {
	drawn := draw(t, listed{err: context.DeadlineExceeded})

	if !strings.Contains(drawn, "context deadline exceeded") {
		t.Errorf("a panel that could not reach the daemon drew:\n%s", drawn)
	}
}

// No sessions is not the same as a problem.
func TestItSaysWhenThereAreNoSessionsYet(t *testing.T) {
	drawn := draw(t, listed{})

	if !strings.Contains(drawn, "no sessions") {
		t.Errorf("an empty daemon drew:\n%s", drawn)
	}
}

// The list names what a person is choosing between.
func TestItListsTheSessionsToChooseFrom(t *testing.T) {
	drawn := draw(t, listed{sessions: []Summary{
		{ID: "ses_1", Title: "the deploy"},
		{ID: "ses_2", Title: "reading the logs"},
	}})

	for _, expected := range []string{"the deploy", "reading the logs"} {
		if !strings.Contains(drawn, expected) {
			t.Errorf("%q is not in the list:\n%s", expected, drawn)
		}
	}
}

// A session with no title is still reachable.
//
// Sessions get their titles from the first turn, so one that has not had a
// turn yet has none. Drawing a blank row would leave a line nobody can tell
// apart from another blank row.
func TestASessionWithNoTitleIsStillNamed(t *testing.T) {
	drawn := draw(t, listed{sessions: []Summary{{ID: "ses_1"}}})

	if !strings.Contains(drawn, "ses_1") {
		t.Errorf("an untitled session drew nothing to identify it:\n%s", drawn)
	}
}

// Moving down and pressing enter opens the session under the cursor.
func TestItOpensTheSessionUnderTheCursor(t *testing.T) {
	watched := &recordingSessions{}
	model := start(t, watched, listed{sessions: []Summary{
		{ID: "ses_1", Title: "first"},
		{ID: "ses_2", Title: "second"},
	}})

	model = after(model, key("down"))
	model, command := model.Update(key("enter"))
	runCommand(t, command)

	if watched.opened != "ses_2" {
		t.Errorf("enter on the second row opened %q", watched.opened)
	}
}

// A session draws the conversation the daemon assembled.
func TestItDrawsTheConversationItWasGiven(t *testing.T) {
	drawn := drawSession(t, Screen{
		Messages: []Message{
			{Role: domain.RoleUser, Text: "run the tests"},
			{Role: domain.RoleAssistant, Text: "One failed."},
		},
		HeadSeq: 12,
	})

	for _, expected := range []string{"run the tests", "One failed."} {
		if !strings.Contains(drawn, expected) {
			t.Errorf("%q is not on the screen:\n%s", expected, drawn)
		}
	}
}

// The working-out is not drawn as the answer.
//
// It reaches this client and no other, so this is the one place it can be
// shown at all — and the one place it can be shown wrongly, by running it
// together with the reply as if the model had said it.
func TestTheWorkingOutIsMarkedAsSuch(t *testing.T) {
	drawn := drawSession(t, Screen{Messages: []Message{{
		Role:      domain.RoleAssistant,
		Reasoning: "the tests are in a different directory",
		Text:      "One failed.",
	}}})

	thinking := lineOf(t, drawn, "the tests are in a different directory")
	answer := lineOf(t, drawn, "One failed.")

	lines := strings.Split(drawn, "\n")
	if thinking == 0 || !strings.Contains(strings.ToLower(lines[thinking-1]), "thinking") {
		t.Errorf("the working-out is not labelled as such:\n%s", drawn)
	}
	if answer == 0 || strings.Contains(strings.ToLower(lines[answer-1]), "thinking") {
		t.Errorf("the answer is drawn as part of the working-out:\n%s", drawn)
	}
	if answer <= thinking {
		t.Errorf("the answer comes before the working-out that led to it:\n%s", drawn)
	}
}

// lineOf is which line something was drawn on.
func lineOf(t *testing.T, drawn, wanted string) int {
	t.Helper()

	for index, line := range strings.Split(drawn, "\n") {
		if strings.Contains(line, wanted) {
			return index
		}
	}
	t.Fatalf("%q was not drawn at all:\n%s", wanted, drawn)
	return -1
}

// A run still going is said to be, because it is what an interrupt acts on.
func TestItSaysWhenARunIsStillGoing(t *testing.T) {
	drawn := drawSession(t, Screen{ActiveRun: "run_1"})

	if !strings.Contains(strings.ToLower(drawn), "running") {
		t.Errorf("a session with a run in flight drew:\n%s", drawn)
	}
}

// An event arriving is folded onto the screen.
func TestALiveEventReachesTheScreen(t *testing.T) {
	model := opened(t, Screen{HeadSeq: 4})

	model = after(model, arrived{update: Update{Event: &domain.Event{
		Seq:     5,
		Kind:    domain.EventUserMessageAdded,
		Payload: domain.UserMessageAdded{Text: "and now this"},
	}}})

	drawn := view(model)
	if !strings.Contains(drawn, "and now this") {
		t.Errorf("an event that arrived is not on the screen:\n%s", drawn)
	}
	if got := model.(panel).screen.HeadSeq; got != 5 {
		t.Errorf("the cursor stayed at %d after folding seq 5", got)
	}
}

// Folding one event asks for the next one.
//
// A command runs once. A panel that did not ask again would draw the first
// thing that happened after it attached and then sit there, showing a session
// that has moved on — which looks exactly like a session where nothing is
// happening.
func TestItKeepsAskingForWhatHappensNext(t *testing.T) {
	watched := &recordingSessions{updates: make(chan Update, 1)}
	model := start(t, watched, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1"})

	_, command := model.Update(arrived{update: Update{Event: &domain.Event{
		Seq: 5, Kind: domain.EventUserMessageAdded,
		Payload: domain.UserMessageAdded{Text: "one"},
	}}})

	if command == nil {
		t.Fatal("after folding an event the panel stopped following the session")
	}

	watched.updates <- Update{Event: &domain.Event{
		Seq: 6, Kind: domain.EventUserMessageAdded,
		Payload: domain.UserMessageAdded{Text: "two"},
	}}
	next, ok := command().(arrived)
	if !ok {
		t.Fatalf("asking for the next update produced %T", command())
	}
	if next.update.Event.Seq != 6 {
		t.Errorf("the panel re-read seq %d rather than what came next",
			next.update.Event.Seq)
	}
}

// A gap in the log is said out loud.
//
// The panel cannot fold what it never received. Resuming quietly from the new
// oldest sequence would draw a conversation with a hole in it and nothing to
// show there had been one.
func TestItSaysWhenItMissedEvents(t *testing.T) {
	model := opened(t, Screen{HeadSeq: 4})

	model = after(model, arrived{update: Update{OldestSeq: 30}})

	drawn := view(model)
	if !strings.Contains(strings.ToLower(drawn), "missed") {
		t.Errorf("a panel that lost part of the log drew:\n%s", drawn)
	}
}

// Escape goes back to the list rather than out of the panel.
//
// Once a session is open, escape is how a person leaves it. A panel where it
// quit instead would drop somebody to the shell for pressing the key that
// means "back" everywhere else.
func TestEscapeLeavesTheSessionAndNotThePanel(t *testing.T) {
	model := opened(t, Screen{Messages: []Message{
		{Role: domain.RoleUser, Text: "run the tests"},
	}})

	model, command := model.Update(key("esc"))
	if isQuit(command) {
		t.Fatal("escape inside a session quit the panel")
	}
	if drawn := view(model); strings.Contains(drawn, "run the tests") {
		t.Errorf("escape stayed in the session:\n%s", drawn)
	}
}

// helpers

// recordingSessions is a Sessions that remembers what was asked of it.
type recordingSessions struct {
	sessions []Summary
	screen   Screen
	opened   domain.SessionID
	updates  chan Update

	decision    Decision
	answer      Answer
	answered    int
	interrupted domain.RunID

	// asked counts what was sent, because a run id of "" is both what an
	// interrupt of a quiet session would carry and what this field holds
	// when nothing was sent at all.
	asked int

	// artifact is what ReadArtifact hands back, and opener/into are where a
	// check watches the file go.
	artifact string
	opener   *recordingOpener
	into     string

	// refuse is what Decide and Interrupt answer with, so a panel can be
	// checked on what it does when the daemon says no.
	refuse error
}

func (r *recordingSessions) Decide(_ context.Context, decision Decision) error {
	if r.refuse != nil {
		return r.refuse
	}
	r.decision = decision
	return nil
}

func (r *recordingSessions) Answer(_ context.Context, answer Answer) error {
	if r.refuse != nil {
		return r.refuse
	}
	r.answer = answer
	r.answered++
	return nil
}

func (r *recordingSessions) ReadArtifact(_ context.Context, id string) ([]byte, error) {
	if r.refuse != nil {
		return nil, r.refuse
	}
	if r.opener != nil {
		r.opener.wanted = id
	}
	return []byte(r.artifact), nil
}

func (r *recordingSessions) Interrupt(_ context.Context, run domain.RunID) error {
	if r.refuse != nil {
		return r.refuse
	}
	r.interrupted = run
	r.asked++
	return nil
}

func (r *recordingSessions) List(context.Context) ([]Summary, error) {
	return r.sessions, nil
}

func (r *recordingSessions) Open(_ context.Context, id domain.SessionID) (Screen, error) {
	r.opened = id
	return r.screen, nil
}

func (r *recordingSessions) Watch(
	context.Context, domain.SessionID, domain.Seq,
) <-chan Update {
	if r.updates == nil {
		r.updates = make(chan Update)
	}
	return r.updates
}

// start is a panel that has been given a list.
func start(t *testing.T, sessions Sessions, message tea.Msg) tea.Model {
	t.Helper()

	recording, _ := sessions.(*recordingSessions)
	var opener Opener
	into := ""
	if recording != nil && recording.opener != nil {
		opener, into = recording.opener, recording.into
	}

	var model tea.Model = newPanel(context.Background(), sessions, opener, into)
	model = after(model, tea.WindowSizeMsg{Width: 80, Height: 24})
	return after(model, message)
}

// draw is what a panel shows after one message.
func draw(t *testing.T, message tea.Msg) string {
	t.Helper()
	return view(start(t, &recordingSessions{}, message))
}

// opened is a panel showing one session.
func opened(t *testing.T, screen Screen) tea.Model {
	t.Helper()

	model := start(t, &recordingSessions{}, listed{sessions: []Summary{{ID: "ses_1"}}})
	return after(model, showing{id: "ses_1", screen: screen})
}

// drawSession is what a panel shows for one session.
func drawSession(t *testing.T, screen Screen) string {
	t.Helper()
	return view(opened(t, screen))
}

func after(model tea.Model, message tea.Msg) tea.Model {
	next, _ := model.Update(message)
	return next
}

func view(model tea.Model) string {
	return model.View().Content
}

func key(name string) tea.Msg {
	switch name {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: rune(name[0]), Text: name}
}

// runCommand runs a command for its effect, discarding what it returns.
func runCommand(t *testing.T, command tea.Cmd) tea.Msg {
	t.Helper()
	if command == nil {
		return nil
	}
	return command()
}

// errRefused stands for the daemon saying no, for the checks that only care
// that a refusal reaches the screen.
var errRefused = errors.New("the daemon would not have it")

// secondOf is the command half of what Update returns, for the checks that
// only care what it did rather than what it drew.
func secondOf(_ tea.Model, command tea.Cmd) tea.Cmd { return command }

// isQuit says whether a command is the one that ends the panel.
func isQuit(command tea.Cmd) bool {
	if command == nil {
		return false
	}
	_, quitting := command().(tea.QuitMsg)
	return quitting
}
