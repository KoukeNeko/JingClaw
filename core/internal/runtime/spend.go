package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// noticeAfter is how many reads go by before a run is told how many.
//
// Six, because that is past the point where a search is plainly a search and
// well short of the ceiling. Earlier would interrupt looking something up;
// much later and the context it would have saved is already spent.
const noticeAfter = 6

// spendNotice is what a run is told about its own searching, once.
//
// A model cannot see how much of its context it has spent. It asks for a
// grep, reads a file, asks for another, and at no point is there anything to
// tell it that this has become a long search — so a tool that exists to make
// long searches cheap is one it never has a reason to reach for. Four
// different ways of saying "use investigate for this" in the prompt changed
// nothing, and this is why: none of them was news at the moment it mattered.
//
// So this is not another way of saying it. It is the one fact the model does
// not have, delivered when it becomes true, and phrased as a fact.
//
// Returns empty when there is nothing worth saying.
func (r *Runtime) spendNotice(ctx context.Context, run domain.Run, name string) string {
	// A worker is already the thing this would suggest, and cannot delegate
	// again. Telling it to would be telling it to do what it is refused.
	//
	// The check below covers this too, since a worker is not offered the
	// tool — either alone is enough, and removing one leaves the other
	// holding. Both, because they say different things: that one says a
	// worker must not be told, and this one says nobody is told about a tool
	// they do not have.
	if run.Kind == domain.RunWorker {
		return ""
	}
	if !searching[name] {
		return ""
	}
	if !r.toolsFor(run)["investigate"] {
		return ""
	}

	// The call being answered is one of them: this runs before its result is
	// recorded, so the log does not have it yet. Counting only what is
	// already written would make the threshold mean one more than it says
	// and the number in the sentence one less than the truth.
	reads, said, err := r.readsSoFar(ctx, run)
	reads++

	if err != nil || said || reads != noticeAfter {
		// Exactly at the threshold, so this happens once. Counting "at least"
		// would append it to every result from here to the end of the run,
		// which is nagging rather than informing.
		return ""
	}

	return fmt.Sprintf(
		"\n\n(%d searches so far this turn, and everything they returned is now "+
			"part of what you re-read on every turn from here. If finding this "+
			"out will take many more, investigate can do the rest in a context "+
			"of its own and hand back only what it concluded.)", reads)
}

// searching is the tools whose results are a search rather than an answer.
//
// Named rather than derived from a level. Reading a file somebody named is
// not searching, and a run that opened four files it already knew about is
// not one to interrupt.
var searching = map[string]bool{
	"grep":       true,
	"glob_files": true,
	"read_file":  true,
}

// readsSoFar counts this run's searches, and whether it has been told.
func (r *Runtime) readsSoFar(ctx context.Context, run domain.Run) (int, bool, error) {
	events, err := r.opts.Store.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return 0, false, err
	}

	var reads int
	var said bool
	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		done, ok := event.Payload.(domain.ToolCallCompleted)
		if !ok {
			continue
		}
		if searching[done.Name] {
			reads++
		}
		// Read back out of the log rather than remembered in a field, so a
		// run resumed in another process does not say it again.
		if strings.Contains(done.Content, spendMarker) {
			said = true
		}
	}
	return reads, said, nil
}

// spendMarker is what makes the notice findable in the log on a resume.
const spendMarker = "searches so far this turn"

// toolsFor is the tools a run may call, by name.
func (r *Runtime) toolsFor(run domain.Run) map[string]bool {
	offered := make(map[string]bool)
	for _, declared := range r.declarationsFor(run) {
		offered[declared.Name] = true
	}
	return offered
}
