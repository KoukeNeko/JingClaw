package supervise

import (
	"context"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/console"

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
	said := &lastWords{limit: 200}
	fmt.Fprintln(said, "error: the database is held by something else")

	err := stopped("gateway", said)
	if err == nil {
		t.Fatal("a crash was reported as a clean stop")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("the message does not say which part: %v", err)
	}
	// What it said, not that it said something. The exit status alone is
	// what sent somebody looking for a log nobody told them about.
	if !strings.Contains(err.Error(), "held by something else") {
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

// Stopping waits for it to have stopped.
//
// A signal is a request, and it returns before the process has acted on it.
// The wrapper script runs `stop` and then starts again in the same line, so
// returning early means the second half looks at a process that is still
// dying, attaches to it, and reports that the build it just made has not
// reached the deployment — advice to run exactly what was just run.
func TestStoppingWaitsForItToHaveStopped(t *testing.T) {
	breaths := 0
	stillThere := func(int) bool {
		breaths++
		return breaths < 4
	}

	if !gone(99, stillThere, time.Second, func(time.Duration) {}) {
		t.Error("it gave up while the process was on its way out")
	}
	if breaths < 4 {
		t.Errorf("it asked %d times, so it did not wait at all", breaths)
	}
}

// And gives up rather than hanging, when it does not go.
//
// A stop that never returns is worse than one that reports the truth: the
// terminal is held by a command that is waiting for something that is not
// going to happen, and there is nothing on screen to say so.
func TestStoppingGivesUpOnSomethingThatWillNotGo(t *testing.T) {
	waited := time.Duration(0)
	stubborn := func(int) bool { return true }

	if gone(99, stubborn, 200*time.Millisecond, func(d time.Duration) { waited += d }) {
		t.Error("a process that never went was reported as stopped")
	}
	if waited > time.Second {
		t.Errorf("it waited %v on something it was told to give up on after 200ms", waited)
	}
}

// One already gone is gone, without waiting at all.
func TestSomethingAlreadyStoppedIsNotWaitedFor(t *testing.T) {
	waited := time.Duration(0)

	if !gone(99, func(int) bool { return false }, time.Second,
		func(d time.Duration) { waited += d }) {
		t.Error("a process that was already gone was reported as still running")
	}
	if waited != 0 {
		t.Errorf("it waited %v for something that had already gone", waited)
	}
}

// Two times an hour apart on different days are not both "4:59AM".
//
// The note exists to be read at a glance, and a clock time with no day on it
// cannot say whether the thing running started this morning or on Friday.
func TestATimeSaysWhichDayWhenItIsNotToday(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 43, 0, 0, time.Local)
	earlier := time.Date(2026, 9, 2, 4, 59, 0, 0, time.Local)
	yesterday := time.Date(2026, 9, 1, 4, 59, 0, 0, time.Local)

	if said := clockOf(earlier, now); strings.Contains(said, "Sep") {
		t.Errorf("a time from today is dated: %q", said)
	}
	if said := clockOf(yesterday, now); !strings.Contains(said, "Sep") {
		t.Errorf("a time from another day is not dated: %q", said)
	}
}

// A part that failed to start says why.
//
// The daemon refuses to run against a model the provider does not serve, and
// says so on the way out. Under a console its output goes to a log file so it
// cannot land in the middle of what somebody is typing, and what reached the
// terminal was "the daemon did not start" — sixty seconds later, with the
// reason sitting in a file nobody was told to look at.
func TestAPartThatFailedToStartSaysWhy(t *testing.T) {
	said := &lastWords{limit: 400}
	fmt.Fprintln(said, `error: provider ollama does not serve model "nemotron-3-ultra:cloud"`)

	gone := make(chan struct{})
	close(gone)

	err := waitForReady(context.Background(), gone, said, func() (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("a part that exited without publishing was reported as ready")
	}
	if !strings.Contains(err.Error(), "does not serve model") {
		t.Errorf("the failure does not say why: %v", err)
	}
}

// And does not wait out the timeout to say it.
//
// Sixty seconds of a blank terminal is what somebody presses Ctrl-C through,
// and then they have no message at all.
func TestItDoesNotWaitOutTheTimeoutForAPartThatHasGone(t *testing.T) {
	gone := make(chan struct{})
	close(gone)

	started := time.Now()
	if err := waitForReady(context.Background(), gone, &lastWords{limit: 10},
		func() (bool, error) { return false, nil }); err == nil {
		t.Fatal("a part that exited was reported as ready")
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("it waited %v for something that had already gone", waited)
	}
}

// A part that comes up is ready, and nothing is said about it.
func TestAPartThatPublishesIsReady(t *testing.T) {
	published := false
	err := waitForReady(context.Background(), make(chan struct{}), &lastWords{limit: 10},
		func() (bool, error) {
			was := published
			published = true
			return was, nil
		})
	if err != nil {
		t.Errorf("a part that published was reported as failed: %v", err)
	}
}

// What a part said last is what is kept.
//
// The end rather than the start: a program that failed says why on its way
// out, and its first lines are it announcing itself.
func TestWhatIsKeptIsWhatWasSaidLast(t *testing.T) {
	said := &lastWords{limit: 20}

	fmt.Fprint(said, strings.Repeat("announcing itself\n", 40))
	fmt.Fprint(said, "the actual reason")

	kept := said.String()
	if !strings.Contains(kept, "the actual reason") {
		t.Errorf("the end was dropped: %q", kept)
	}
	if strings.Contains(kept, "announcing itself") {
		t.Errorf("the beginning was kept instead: %q", kept)
	}
	if len(kept) > 20 {
		t.Errorf("it kept %d bytes against a limit of 20", len(kept))
	}
}
