package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

func aStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func aSession(t *testing.T, store *Store, id domain.SessionID) {
	t.Helper()
	if err := store.CreateSession(context.Background(), domain.Session{
		ID: id, Title: string(id), CreatedAt: time.Unix(0, 0),
	}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func anEvent(t *testing.T, store *Store, id domain.SessionID, text string) domain.Seq {
	t.Helper()
	return anEventAt(t, store, id, text, time.Unix(0, 0))
}

func anEventAt(
	t *testing.T, store *Store, id domain.SessionID, text string, at time.Time,
) domain.Seq {
	t.Helper()
	seq, err := store.Append(context.Background(), domain.Event{
		ID:         domain.EventID("evt_" + text),
		SessionID:  id,
		OccurredAt: at,
		Kind:       domain.EventUserMessageAdded,
		Payload:    domain.UserMessageAdded{Text: text},
	})
	if err != nil {
		t.Fatalf("append %s: %v", text, err)
	}
	return seq
}

// Two sessions both at seq 3 make "I have read up to 3" mean nothing, which
// is the question the whole log's position exists to answer.
func TestTheWholeLogHasOneOrderAcrossSessions(t *testing.T) {
	store := aStore(t)
	aSession(t, store, "a")
	aSession(t, store, "b")

	// Interleaved on purpose: this is the case a per-session number cannot
	// describe.
	anEvent(t, store, "a", "one")
	anEvent(t, store, "b", "two")
	anEvent(t, store, "a", "three")
	anEvent(t, store, "b", "four")

	everything, err := store.ListAllAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(everything) != 4 {
		t.Fatalf("read %d events, want 4", len(everything))
	}

	for index, event := range everything {
		if want := domain.Seq(index + 1); event.GlobalSeq != want {
			t.Errorf("event %d is at %d, want %d", index, event.GlobalSeq, want)
		}
	}

	said := []string{"one", "two", "three", "four"}
	for index, event := range everything {
		if got := event.Payload.(domain.UserMessageAdded).Text; got != said[index] {
			t.Errorf("position %d holds %q, want %q", index+1, got, said[index])
		}
	}
}

// Resuming means starting after what you have, from every session at once.
func TestResumingFromAPositionSkipsExactlyWhatWasRead(t *testing.T) {
	store := aStore(t)
	aSession(t, store, "a")
	aSession(t, store, "b")
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		session := domain.SessionID("a")
		if len(text)%2 == 0 {
			session = "b"
		}
		anEvent(t, store, session, text)
	}

	rest, err := store.ListAllAfter(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rest) != 3 {
		t.Fatalf("resuming after 2 gave %d events, want 3", len(rest))
	}
	if rest[0].GlobalSeq != 3 {
		t.Errorf("resumed at %d, want 3", rest[0].GlobalSeq)
	}
}

// A per-session number and the whole log's number have to agree about the
// order within one session, or a client watching both sees them differently.
func TestTheTwoOrdersAgreeWithinASession(t *testing.T) {
	store := aStore(t)
	aSession(t, store, "a")
	aSession(t, store, "b")
	for index := range 6 {
		session := domain.SessionID("a")
		if index%2 == 1 {
			session = "b"
		}
		anEvent(t, store, session, string(rune('a'+index)))
	}

	own, err := store.ListAfter(context.Background(), "a", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for index := 1; index < len(own); index++ {
		if own[index].GlobalSeq <= own[index-1].GlobalSeq {
			t.Errorf("seq %d is at %d, after seq %d at %d",
				own[index].Seq, own[index].GlobalSeq,
				own[index-1].Seq, own[index-1].GlobalSeq)
		}
	}
}

// Pruning leaves gaps, which is fine. What is not fine is a client resuming
// into one and being told nothing happened.
func TestPruningRaisesTheWatermark(t *testing.T) {
	ctx := context.Background()
	store := aStore(t)
	aSession(t, store, "a")
	aSession(t, store, "b")

	anEvent(t, store, "a", "one")
	anEvent(t, store, "b", "two")
	anEvent(t, store, "a", "three")
	anEvent(t, store, "b", "four")

	if through, err := store.LogPrunedThrough(ctx); err != nil || through != 0 {
		t.Fatalf("a log that has lost nothing says %d (err %v), want 0", through, err)
	}

	// Session a's events sit at 1 and 3.
	if _, err := store.PruneEvents(ctx, "a", 2); err != nil {
		t.Fatalf("prune: %v", err)
	}

	through, err := store.LogPrunedThrough(ctx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if through != 3 {
		t.Errorf("the watermark is %d, want 3 — the highest position discarded", through)
	}
}

// Sessions are pruned one at a time and in no order, so a later prune can be
// of older events. Lowering the mark would claim back events that are gone.
func TestTheWatermarkOnlyRises(t *testing.T) {
	ctx := context.Background()
	store := aStore(t)
	aSession(t, store, "a")
	aSession(t, store, "b")

	anEvent(t, store, "b", "one")   // 1
	anEvent(t, store, "a", "two")   // 2
	anEvent(t, store, "a", "three") // 3
	anEvent(t, store, "b", "four")  // 4

	if _, err := store.PruneEvents(ctx, "a", 2); err != nil { // discards 2 and 3
		t.Fatalf("prune a: %v", err)
	}
	if _, err := store.PruneEvents(ctx, "b", 1); err != nil { // discards 1
		t.Fatalf("prune b: %v", err)
	}

	through, err := store.LogPrunedThrough(ctx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if through != 3 {
		t.Errorf("the watermark fell to %d; it must stay at 3", through)
	}
}

func TestTheHeadIsTheLastThingAppended(t *testing.T) {
	ctx := context.Background()
	store := aStore(t)
	aSession(t, store, "a")

	if head, err := store.LogHead(ctx); err != nil || head != 0 {
		t.Fatalf("an empty log's head is %d (err %v), want 0", head, err)
	}

	anEvent(t, store, "a", "one")
	anEvent(t, store, "a", "two")

	head, err := store.LogHead(ctx)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 2 {
		t.Errorf("the head is %d, want 2", head)
	}
}

// The reason a position exists at all, rather than ordering the log by time.
//
// A wall clock goes backwards on an NTP correction, so an event appended later
// can carry an earlier time. Ordered by time it sorts in front of what a
// client has already read, and the client — asking only for what comes after
// its cursor — never sees it. Ordered by position, it is simply next.
func TestAnEventWhoseClockWentBackwardsIsStillNext(t *testing.T) {
	store := aStore(t)
	aSession(t, store, "a")

	anEventAt(t, store, "a", "first", time.Unix(2000, 0))
	anEventAt(t, store, "a", "second", time.Unix(3000, 0))
	// The clock is corrected, and the next event carries a time before both.
	anEventAt(t, store, "a", "third", time.Unix(1000, 0))

	everything, err := store.ListAllAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	said := make([]string, 0, len(everything))
	for _, event := range everything {
		said = append(said, event.Payload.(domain.UserMessageAdded).Text)
	}
	if len(said) != 3 || said[0] != "first" || said[1] != "second" || said[2] != "third" {
		t.Fatalf("the log reads %v, want [first second third] — the order it was written", said)
	}

	// And a client that had read the first two is handed the third, rather
	// than never seeing it because its timestamp sorts behind them.
	rest, err := store.ListAllAfter(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("resuming after 2 gave %d events, want 1", len(rest))
	}
	if got := rest[0].Payload.(domain.UserMessageAdded).Text; got != "third" {
		t.Errorf("resuming gave %q, want third", got)
	}
}
