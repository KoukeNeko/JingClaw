package runtime

import (
	"context"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// PruneSession discards events a conversation no longer needs.
//
// The log is the truth and it grows without bound: a session used every day
// accumulates every delta of every answer forever. What makes any of it safe
// to discard is compaction — once turns have been folded into a summary, the
// rebuilt conversation reads the summary and not the turns behind it.
//
// So the cut is never chosen by age or by count. It is the last fold, minus
// whatever the caller wants kept beyond it, and never past a fold that is
// still the most recent one: discarding an event the rebuild still reads makes
// a session unusable rather than smaller.
//
// Returns how many events went.
func (r *Runtime) PruneSession(
	ctx context.Context, sessionID domain.SessionID, keepAfterFold int,
) (int64, error) {
	if keepAfterFold < 0 {
		keepAfterFold = 0
	}

	events, err := r.opts.Store.ListAfter(ctx, sessionID, 0, 0)
	if err != nil {
		return 0, err
	}

	cut := safeCut(events, keepAfterFold)
	if cut <= 0 {
		// Nothing has been folded, so nothing is safe to discard. A session
		// that has never compacted keeps everything, which is correct: every
		// event is still read when the conversation is rebuilt.
		return 0, nil
	}

	removed, err := r.opts.Store.PruneEvents(ctx, sessionID, cut)
	if err != nil {
		return 0, fmt.Errorf("runtime: prune %s: %w", sessionID, err)
	}

	if removed > 0 {
		r.opts.Logger.Info("discarded events a summary replaced",
			"session_id", string(sessionID), "through_seq", uint64(cut), "events", removed)
	}
	return removed, nil
}

// safeCut is the highest sequence that may be discarded.
//
// Everything up to the last fold, less a margin the caller asked to keep. The
// fold event itself stays: it is what tells a rebuild that history was folded
// rather than lost, and a client drawing the conversation shows it.
func safeCut(events []domain.Event, keepAfterFold int) domain.Seq {
	lastFold := -1
	for index, event := range events {
		if _, folded := event.Payload.(domain.ConversationCompacted); folded {
			lastFold = index
		}
	}
	if lastFold <= 0 {
		return 0
	}

	// Up to but not including the fold, then back off by the margin.
	cutIndex := lastFold - 1 - keepAfterFold
	if cutIndex < 0 {
		return 0
	}
	return events[cutIndex].Seq
}
