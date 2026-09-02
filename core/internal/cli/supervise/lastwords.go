package supervise

import (
	"strings"
	"sync"
)

// lastWords keeps the end of what a part wrote.
//
// So that a part which failed to start can be quoted rather than only
// reported. Its output goes to a log file when somebody is watching — the
// console owns the bottom line of the terminal and a part writing to the same
// stream lands in the middle of what is being typed — and a message saying
// only that something did not start sends whoever read it looking for a file
// nobody told them about.
//
// The end rather than the start: a program that failed says why on its way
// out, and its first lines are it announcing itself.
type lastWords struct {
	mu    sync.Mutex
	kept  []byte
	limit int
}

func (w *lastWords) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.kept = append(w.kept, p...)
	if extra := len(w.kept) - w.limit; extra > 0 {
		w.kept = w.kept[extra:]
	}
	return len(p), nil
}

func (w *lastWords) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return strings.TrimSpace(string(w.kept))
}
