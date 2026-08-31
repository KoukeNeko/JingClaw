package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// What is waiting is drawn with enough to decide it.
//
// The name of the tool is not enough. A person allowing a command is allowing
// that command, and a panel that showed only "exec_command" would be asking
// them to approve a category.
func TestItDrawsWhatIsWaitingInEnoughDetailToDecide(t *testing.T) {
	drawn := drawSession(t, Screen{Waiting: []Waiting{{
		ID:       "apr_1",
		ToolName: "exec_command",
		Preview:  "rm -rf ./build",
		Effects:  []string{"writes to the workspace"},
	}}})

	for _, expected := range []string{"exec_command", "rm -rf ./build", "writes to the workspace"} {
		if !strings.Contains(drawn, expected) {
			t.Errorf("%q is not on the screen:\n%s", expected, drawn)
		}
	}
}

// A run that read somebody else's words before asking says so.
//
// The one thing the person deciding cannot see for themselves: the request
// looks identical whether the agent arrived at it or a page it read suggested
// it, and only the log knows which.
func TestItSaysWhenTheRunHadReadSomebodyElsesWords(t *testing.T) {
	drawn := drawSession(t, Screen{Waiting: []Waiting{{
		ID: "apr_1", ToolName: "exec_command", ReadForeign: true,
	}}})

	if !strings.Contains(strings.ToLower(drawn), "read") {
		t.Errorf("a request made after reading foreign text drew:\n%s", drawn)
	}
}

// And one that did not is not marked, or the mark means nothing.
func TestItDoesNotWarnAboutARunThatReadNothing(t *testing.T) {
	drawn := drawSession(t, Screen{Waiting: []Waiting{{
		ID: "apr_1", ToolName: "exec_command",
	}}})

	if strings.Contains(strings.ToLower(drawn), "somebody else") {
		t.Errorf("a request made off the agent's own reasoning was marked:\n%s", drawn)
	}
}

// Allowing sends a decision, and says which.
func TestAllowingSendsAnAllow(t *testing.T) {
	decided := &recordingSessions{}
	model := waiting(t, decided, "apr_1")

	runCommand(t, secondOf(model.Update(key("a"))))

	if decided.decision.ID != "apr_1" {
		t.Fatalf("nothing was decided; got %q", decided.decision.ID)
	}
	if decided.decision.Status != domain.ApprovalAllowed {
		t.Errorf("pressing allow sent %q", decided.decision.Status)
	}
	if decided.decision.Scope != domain.RememberOnce {
		t.Errorf("a plain allow carried scope %q", decided.decision.Scope)
	}
}

// Refusing sends a deny rather than nothing.
//
// The distinction the wire format was changed to keep: a client that could
// not say "deny" would refuse by staying quiet, and a run blocked forever
// looks the same as a run nobody has got to yet.
func TestRefusingSendsADeny(t *testing.T) {
	decided := &recordingSessions{}
	model := waiting(t, decided, "apr_1")

	runCommand(t, secondOf(model.Update(key("d"))))

	if decided.decision.Status != domain.ApprovalDenied {
		t.Errorf("pressing deny sent %q", decided.decision.Status)
	}
}

// Allowing for the session is a different decision from allowing once.
func TestAllowingForTheSessionSaysSo(t *testing.T) {
	decided := &recordingSessions{}
	model := waiting(t, decided, "apr_1")

	runCommand(t, secondOf(model.Update(key("A"))))

	if decided.decision.Scope != domain.RememberSession {
		t.Errorf("allowing for the session carried scope %q", decided.decision.Scope)
	}
}

// Nothing waiting means nothing is decided.
//
// Otherwise a stray keypress on a quiet session sends a decision about
// whatever was last on the screen.
func TestAKeypressWithNothingWaitingDecidesNothing(t *testing.T) {
	decided := &recordingSessions{}
	model := start(t, decided, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1"})

	runCommand(t, secondOf(model.Update(key("a"))))

	if decided.decision.ID != "" {
		t.Errorf("a keypress with nothing waiting decided %q", decided.decision.ID)
	}
}

// The request being decided is the one on the screen.
//
// The oldest, which is what the panel draws. A panel that drew one and
// decided another would allow a call somebody never read, and the person
// pressing the key would have no way to notice.
func TestItDecidesTheRequestItIsShowing(t *testing.T) {
	decided := &recordingSessions{}
	model := start(t, decided, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{Waiting: []Waiting{
		{ID: "apr_first", ToolName: "exec_command", Preview: "the one on screen"},
		{ID: "apr_second", ToolName: "write_file"},
	}}})

	if drawn := view(model); !strings.Contains(drawn, "the one on screen") {
		t.Fatalf("the oldest request is not the one drawn:\n%s", drawn)
	}

	runCommand(t, secondOf(model.Update(key("a"))))

	if decided.decision.ID != "apr_first" {
		t.Errorf("the panel decided %q while showing the first", decided.decision.ID)
	}
}

// Interrupting names the run in flight.
func TestInterruptingStopsTheRunThatIsGoing(t *testing.T) {
	stopped := &recordingSessions{}
	model := start(t, stopped, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{ActiveRun: "run_7"}})

	runCommand(t, secondOf(model.Update(key("i"))))

	if stopped.interrupted != "run_7" {
		t.Errorf("interrupt stopped %q", stopped.interrupted)
	}
}

// And does nothing when nothing is going.
func TestInterruptingAQuietSessionStopsNothing(t *testing.T) {
	stopped := &recordingSessions{}
	model := start(t, stopped, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1"})

	runCommand(t, secondOf(model.Update(key("i"))))

	if stopped.asked != 0 {
		t.Errorf("interrupt on a quiet session sent %d requests", stopped.asked)
	}
}

// A decision the daemon refused is said, not swallowed.
//
// The failure this prevents: a panel that reported success on a refusal would
// leave somebody believing they had unblocked a run that is still waiting.
func TestARefusedDecisionIsSaidOutLoud(t *testing.T) {
	refusing := &recordingSessions{refuse: context.DeadlineExceeded}
	var model tea.Model = waiting(t, refusing, "apr_1")

	model, command := model.Update(key("a"))
	model = after(model, runCommand(t, command))

	if drawn := view(model); !strings.Contains(drawn, "context deadline exceeded") {
		t.Errorf("a refused decision drew:\n%s", drawn)
	}
}

// waiting is a panel showing a session with one approval outstanding.
func waiting(t *testing.T, sessions *recordingSessions, id domain.ApprovalID) tea.Model {
	t.Helper()

	model := start(t, sessions, listed{sessions: []Summary{{ID: "ses_1"}}})
	model = after(model, showing{id: "ses_1", screen: Screen{
		ActiveRun: "run_1",
		Waiting:   []Waiting{{ID: id, ToolName: "exec_command"}},
	}})
	return model
}
