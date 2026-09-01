package console

import (
	"time"
)

// The pacing of a stream that keeps failing.
//
// A console whose daemon went away used to retry with no delay at all,
// printing the same line as fast as the machine could produce it. What that
// looks like is a terminal filling with one sentence, which hides the last
// thing anybody was reading and says nothing the first line did not.
const (
	// firstWait is short enough that a daemon restarting is not noticed.
	firstWait = 500 * time.Millisecond

	// longestWait is where backing off stops. A daemon that is coming back
	// should be found within a few seconds of coming back.
	longestWait = 5 * time.Second
)

// Retries paces reconnection and keeps it from saying the same thing twice.
type Retries struct {
	said string
	wait time.Duration
}

// Failed records a stream ending badly and says what to do about it: what to
// tell the person, if anything, and how long to wait before trying again.
//
// The same failure is reported once. A daemon that is down stays down, and
// the second copy of the sentence carries no information the first did not —
// but a different failure is a different fact and is said.
func (r *Retries) Failed(because string) (say string, wait time.Duration) {
	if r.wait == 0 {
		r.wait = firstWait
	} else if r.wait < longestWait {
		r.wait *= 2
		r.wait = min(r.wait, longestWait)
	}

	if because == r.said {
		return "", r.wait
	}
	r.said = because
	return because, r.wait
}

// Worked records a stream that ran, so the next failure is said again and
// waited on from the start.
//
// Without this a console that reconnected would stay silent about the next
// outage, having already said that sentence once — and would still be
// backing off five seconds at a time an hour later.
func (r *Retries) Worked() {
	r.said = ""
	r.wait = 0
}
