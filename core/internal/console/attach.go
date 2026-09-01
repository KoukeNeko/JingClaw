package console

// tail is how much of the log a console draws when it attaches.
//
// Enough to see what just happened and what is still going, and not so much
// that the thing somebody opened a console for scrolls past while the
// terminal catches up. The log is every event since the deployment was first
// started, which is days of them.
const tail = 60

// AttachFrom is the cursor a console should subscribe from, given where the
// log currently ends.
//
// Zero for a log shorter than the tail: there is nothing to skip, and
// starting partway through a short log hides the beginning of the only
// conversation there is.
func AttachFrom(head uint64) uint64 {
	if head <= tail {
		return 0
	}
	return head - tail
}
