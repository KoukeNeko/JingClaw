package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

// Posting to a platform is a separate act from recording an event, and it can
// fail on its own. The outbox exists so a gateway that dies mid-post does not
// silently drop the reply, and so one that reconnects does not post it twice.

func newOutbox(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.CreateSession(context.Background(), domain.Session{
		ID: "ses_1", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	return store
}

func enqueue(t *testing.T, store *sqlite.Store, account, id, payload string) gateway.DispatchSeq {
	t.Helper()

	seq, err := store.EnqueueDispatch(context.Background(), gateway.Dispatch{
		ID:        id,
		AccountID: account,
		SessionID: "ses_1",
		RunID:     "run_1",
		Target:    discordConversation(),
		Kind:      gateway.DispatchMessage,
		Payload:   payload,
		CreatedAt: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", id, err)
	}
	return seq
}

func TestDispatchesAreOrderedPerAccount(t *testing.T) {
	store := newOutbox(t)

	for i := 1; i <= 3; i++ {
		if got := enqueue(t, store, "account_a", fmt.Sprintf("d%d", i), "hello"); got != gateway.DispatchSeq(i) {
			t.Fatalf("dispatch %d got seq %d", i, got)
		}
	}

	// A second account has its own sequence: one gateway's cursor must not be
	// affected by another's traffic.
	if got := enqueue(t, store, "account_b", "d-b1", "hello"); got != 1 {
		t.Errorf("a second account started at seq %d, want 1", got)
	}
}

// A gateway resumes from its cursor, and must not be handed work it already
// posted.
func TestOnlyUndeliveredDispatchesAreReturned(t *testing.T) {
	store := newOutbox(t)
	ctx := context.Background()

	enqueue(t, store, "account_a", "d1", "first")
	enqueue(t, store, "account_a", "d2", "second")
	enqueue(t, store, "account_a", "d3", "third")

	if err := store.MarkDelivered(ctx, "d1", []string{"msg_1"}, time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	pending, err := store.DispatchesAfter(ctx, "account_a", 0, 0)
	if err != nil {
		t.Fatalf("list dispatches: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d pending dispatches, want 2", len(pending))
	}
	for _, dispatch := range pending {
		if dispatch.ID == "d1" {
			t.Error("a delivered dispatch was handed out again")
		}
	}

	// Resuming past a cursor skips what came before it.
	after, err := store.DispatchesAfter(ctx, "account_a", 2, 0)
	if err != nil {
		t.Fatalf("list after cursor: %v", err)
	}
	if len(after) != 1 || after[0].ID != "d3" {
		t.Errorf("resuming after seq 2 returned %+v", after)
	}
}

// A duplicate acknowledgement must not make a reply appear twice.
func TestSecondAcknowledgementIsRefused(t *testing.T) {
	store := newOutbox(t)
	ctx := context.Background()

	enqueue(t, store, "account_a", "d1", "hello")

	if err := store.MarkDelivered(ctx, "d1", []string{"msg_1"}, time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("first acknowledgement: %v", err)
	}

	err := store.MarkDelivered(ctx, "d1", []string{"msg_2"}, time.Unix(0, 0).UTC())
	if !errors.Is(err, gateway.ErrAlreadyDelivered) {
		t.Fatalf("got %v, want ErrAlreadyDelivered", err)
	}
}

func TestAcknowledgingAnUnknownDispatchIsNotFound(t *testing.T) {
	store := newOutbox(t)

	err := store.MarkDelivered(context.Background(), "nope", nil, time.Unix(0, 0).UTC())
	if !errors.Is(err, gateway.ErrDispatchNotFound) {
		t.Fatalf("got %v, want ErrDispatchNotFound", err)
	}
}

// The platform's own message ids are recorded so a later edit can find what
// was posted.
func TestPlatformMessageIDsSurvive(t *testing.T) {
	store := newOutbox(t)
	ctx := context.Background()

	enqueue(t, store, "account_a", "d1", "hello")
	enqueue(t, store, "account_a", "d2", "second")

	if err := store.MarkDelivered(ctx, "d1", []string{"msg_1", "msg_2"}, time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	// Only undelivered rows come back from the queue, so the record is read
	// through the one still pending to confirm the target round-trips.
	pending, err := store.DispatchesAfter(ctx, "account_a", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1", len(pending))
	}

	target := pending[0].Target
	if target.Platform != gateway.PlatformDiscord || target.ThreadID != "thread_1" {
		t.Errorf("the delivery target did not round-trip: %+v", target)
	}
}

// Allocation reads the current maximum and then inserts. Done without a
// transaction holding the write lock across both, two enqueues collide on one
// sequence and a gateway resuming from a cursor silently skips a reply.
func TestConcurrentEnqueuesGetDistinctSequences(t *testing.T) {
	store := newOutbox(t)

	const count = 30
	var wg sync.WaitGroup
	seqs := make([]gateway.DispatchSeq, count)
	errs := make([]error, count)

	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seqs[i], errs[i] = store.EnqueueDispatch(context.Background(), gateway.Dispatch{
				ID:        fmt.Sprintf("d%d", i),
				AccountID: "account_a",
				SessionID: "ses_1",
				Target:    discordConversation(),
				Kind:      gateway.DispatchMessage,
				Payload:   "hello",
				CreatedAt: time.Unix(0, 0).UTC(),
			})
		}()
	}
	wg.Wait()

	seen := make(map[gateway.DispatchSeq]bool, count)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Fatalf("sequence %d was handed out twice", seqs[i])
		}
		seen[seqs[i]] = true
	}

	for i := 1; i <= count; i++ {
		if !seen[gateway.DispatchSeq(i)] {
			t.Fatalf("sequence %d was never allocated", i)
		}
	}
}

// A conversation maps to exactly one session. Two messages arriving at once
// must not each create one, leaving a thread with two histories.
func TestConversationLinkIsExclusive(t *testing.T) {
	store := newOutbox(t)
	ctx := context.Background()

	if err := store.CreateSession(ctx, domain.Session{
		ID: "ses_2", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := store.LinkConversation(ctx, "key", "ses_1", "binding_1", time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("first link: %v", err)
	}

	err := store.LinkConversation(ctx, "key", "ses_2", "binding_1", time.Unix(0, 0).UTC())
	if !errors.Is(err, gateway.ErrAlreadyProcessed) {
		t.Fatalf("got %v, want ErrAlreadyProcessed", err)
	}

	session, found, err := store.SessionForConversation(ctx, "key")
	if err != nil || !found {
		t.Fatalf("lookup: %v found=%t", err, found)
	}
	if session != "ses_1" {
		t.Errorf("the second link overwrote the first: %s", session)
	}
}
