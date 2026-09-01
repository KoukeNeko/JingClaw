package console_test

import (
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/console"
)

// The same failure is said once.
//
// The console used to retry with no delay and print the failure every time,
// which filled a terminal with one sentence in seconds and hid whatever
// somebody had been reading.
func TestAFailureThatKeepsHappeningIsSaidOnce(t *testing.T) {
	var retries console.Retries

	said, _ := retries.Failed("connection refused")
	if said == "" {
		t.Fatal("the first failure was not reported at all")
	}

	for attempt := range 20 {
		if said, _ := retries.Failed("connection refused"); said != "" {
			t.Fatalf("the same failure was said again on attempt %d: %q", attempt+2, said)
		}
	}
}

// A different failure is a different fact.
func TestADifferentFailureIsSaid(t *testing.T) {
	var retries console.Retries

	retries.Failed("connection refused")
	if said, _ := retries.Failed("no such host"); said != "no such host" {
		t.Errorf("a new failure came back as %q", said)
	}
}

// It waits longer each time, up to a point.
//
// A daemon that is coming back should be found within a few seconds of
// coming back, so the wait stops growing rather than doubling into minutes.
func TestItBacksOffAndThenStops(t *testing.T) {
	var retries console.Retries

	_, first := retries.Failed("connection refused")
	if first <= 0 {
		t.Fatalf("the first retry waits %v, so it is a spin", first)
	}

	previous := first
	longest := first
	for range 20 {
		_, wait := retries.Failed("connection refused")
		if wait < previous {
			t.Errorf("the wait went backwards: %v then %v", previous, wait)
		}
		previous = wait
		longest = wait
	}

	if longest > 10*time.Second {
		t.Errorf("it backs off to %v, which is long enough to look broken", longest)
	}
	if longest <= first {
		t.Errorf("it never backed off at all: %v then %v", first, longest)
	}
}

// A stream that worked resets both.
//
// Otherwise the next outage is silent, having said that sentence once an
// hour ago, and is still waited on at the longest interval.
func TestAStreamThatWorkedStartsOver(t *testing.T) {
	var retries console.Retries

	for range 10 {
		retries.Failed("connection refused")
	}
	retries.Worked()

	said, wait := retries.Failed("connection refused")
	if said == "" {
		t.Error("the outage after a working stream was not reported")
	}
	if wait > time.Second {
		t.Errorf("the first retry after a working stream waits %v", wait)
	}
}

// A console attaching shows what just happened, not everything that ever did.
//
// The log is every event since the deployment was first started. Drawing it
// from the beginning scrolls days past somebody who opened a console to see
// what is happening now, and the thing they wanted is the part that went by
// while the terminal was still catching up.
func TestAttachingDrawsTheTailAndNotTheWholeLog(t *testing.T) {
	// A log with more in it than anybody wants to read.
	const head = 5000

	from := console.AttachFrom(head)
	if from == 0 {
		t.Fatal("a console attaching to a long log starts at the beginning of it")
	}
	if from >= head {
		t.Errorf("it starts at %d with the head at %d, so nothing is drawn", from, head)
	}
	if drawn := head - from; drawn > 200 {
		t.Errorf("it draws the last %d events, which is a wall rather than a tail", drawn)
	}

	// A log shorter than the tail is drawn whole. There is nothing to skip,
	// and starting partway through a short log hides the beginning of the
	// only conversation there is.
	if from := console.AttachFrom(10); from != 0 {
		t.Errorf("a log of 10 events is drawn from %d rather than from the start", from)
	}

	// A deployment with no events yet has nothing to skip either.
	if from := console.AttachFrom(0); from != 0 {
		t.Errorf("an empty log is drawn from %d", from)
	}
}
