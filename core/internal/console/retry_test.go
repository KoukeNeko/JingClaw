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
