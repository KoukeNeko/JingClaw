package runtime_test

import (
	"context"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
)

// Nothing is discarded until a summary has replaced it.
//
// The conversation sent to the model is rebuilt from this log. Discarding an
// event the rebuild still reads makes a session unusable rather than smaller,
// and the failure appears one turn later as the agent having forgotten.
func TestNothingIsPrunedBeforeAnythingIsFolded(t *testing.T) {
	rt, store, _ := newToolHarness(t, [][]provider.Event{
		{provider.TextDelta{Text: "one"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})

	session, err := rt.CreateSession(context.Background(), "unfolded")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	runID, _, err := rt.SendTurn(context.Background(), session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := rt.Wait(context.Background(), runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	before, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	removed, err := rt.PruneSession(context.Background(), session.ID, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 0 {
		t.Errorf("discarded %d events from a session that never compacted", removed)
	}

	after, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("the log went from %d events to %d", len(before), len(after))
	}
}

// Once a fold exists, what it replaced can go — and the fold itself stays,
// because it is what tells a rebuild that history was summarised rather than
// lost.
func TestOnlyWhatAFoldReplacedIsDiscarded(t *testing.T) {
	rt, store, _ := newToolHarness(t, [][]provider.Event{
		{provider.TextDelta{Text: "one"}, provider.Completed{StopReason: domain.StopEndTurn}},
	})

	session, err := rt.CreateSession(context.Background(), "folded")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	runID, _, err := rt.SendTurn(context.Background(), session.ID, "hello", domain.RunOrigin{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := rt.Wait(context.Background(), runID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// A fold, recorded the way compaction records one.
	events, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	through := events[len(events)-1].Seq

	if _, err := store.Append(context.Background(), domain.Event{
		ID:        "evt_fold",
		SessionID: session.ID,
		Kind:      domain.EventConversationCompacted,
		Payload: domain.ConversationCompacted{
			Summary: "what was said", ThroughSeq: through, MessagesFolded: 2,
		},
	}); err != nil {
		t.Fatalf("append fold: %v", err)
	}

	removed, err := rt.PruneSession(context.Background(), session.ID, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("nothing was discarded despite a fold replacing it")
	}

	kept, err := store.ListAfter(context.Background(), session.ID, 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var foldSurvived bool
	for _, event := range kept {
		if _, ok := event.Payload.(domain.ConversationCompacted); ok {
			foldSurvived = true
		}
	}
	if !foldSurvived {
		t.Error("the fold itself was discarded, so nothing says history was summarised")
	}

	// And the sequence numbering does not restart: a sequence names an event
	// for the life of a session, and reusing one gives two events one name.
	oldest, err := store.Oldest(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("oldest: %v", err)
	}
	if oldest <= 1 {
		t.Errorf("the oldest kept event is %d, so numbering restarted", oldest)
	}
}
