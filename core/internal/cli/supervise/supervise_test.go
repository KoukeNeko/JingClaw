package supervise

import (
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/console"

	"errors"
	"strings"
	"testing"
)

// jingclaw stop signals the daemon, so the daemon exiting is the point of the
// command rather than a failure of it.
func TestACleanExitIsNotAnError(t *testing.T) {
	if err := stopped("daemon", nil); err != nil {
		t.Errorf("a clean exit was reported as an error: %v", err)
	}
}

func TestACrashIsStillAnError(t *testing.T) {
	err := stopped("gateway", errors.New("exit status 2"))
	if err == nil {
		t.Fatal("a crash was reported as a clean stop")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("the message does not say which part: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("the message does not carry the reason: %v", err)
	}
}

// Attaching to something already running does not offer to stop it.
//
// The rule this file states about the parts it starts applies to the console
// too: found already running, they are somebody else's — a service, or
// another terminal — and a console that told this person quitting would stop
// them would be offering to end a session they did not start.
func TestAttachingToSomethingAlreadyRunningLeavesItRunning(t *testing.T) {
	if leaving := howLeavingWorks(true); leaving != console.LeavesItRunning {
		t.Errorf("attaching to a running deployment offered to stop it")
	}
	if leaving := howLeavingWorks(false); leaving != console.StopsIt {
		t.Errorf("a deployment this command started is not stopped on the way out")
	}
}

// Without a terminal there is no console to attach.
//
// The failure this prevents is the one the console reports itself, one layer
// too late: piped or under a service, standard input is not a terminal, and
// trying anyway turns "watching it" into an error about raw mode.
func TestWithoutATerminalThereIsNothingToAttach(t *testing.T) {
	if canAttach(false) {
		t.Error("a console was offered where there is no terminal to draw on")
	}
	if !canAttach(true) {
		t.Error("a terminal was refused a console")
	}
}

// A rebuild that did not reach what is running is said out loud.
//
// The trap it closes: the wrapper script builds, finds a deployment already
// up, and attaches to it. Everything on screen says the new code is running
// and the old process is still answering, so a fix that landed twenty minutes
// after that process started is simply absent — and the way anybody finds out
// is by concluding the fix does not work.
func TestARebuildThatDidNotReachWhatIsRunningIsSaid(t *testing.T) {
	started := time.Date(2026, 8, 31, 20, 23, 0, 0, time.UTC)

	if !buildIsNewerThanWhatIsRunning(started.Add(18*time.Minute), started) {
		t.Error("a build made after the running deployment started went unmentioned")
	}
	if buildIsNewerThanWhatIsRunning(started.Add(-time.Hour), started) {
		t.Error("a deployment running newer code than the binary was called stale")
	}

	// Equal is not newer. A deployment started by this very build would
	// otherwise be reported as out of date every time.
	if buildIsNewerThanWhatIsRunning(started, started) {
		t.Error("a deployment started from this build was called stale")
	}
}

// And nothing is claimed when neither time can be read.
//
// Guessing here would be worse than silence: a warning that appears whenever
// a file cannot be stat'd is one people learn to ignore, including the times
// it is right.
func TestNothingIsClaimedWhenTheTimesAreUnknown(t *testing.T) {
	if buildIsNewerThanWhatIsRunning(time.Time{}, time.Now()) {
		t.Error("an unknown build time was reported as newer")
	}
	if buildIsNewerThanWhatIsRunning(time.Now(), time.Time{}) {
		t.Error("an unknown start time was compared against anyway")
	}
}
