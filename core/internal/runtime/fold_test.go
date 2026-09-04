package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

// foldWindow is small enough that every turn below overflows it, so each turn
// after the first is preceded by exactly one compaction, and the nth fold is
// the one written at the request for turn n+1.
const foldWindow = 2000

// foldedSession runs `turns` oversized turns through a compacting runtime and
// returns the session, so the tests below read the same log at different
// depths.
func foldedSession(
	t *testing.T, store *memory.Store, model *compactingProvider, turns int,
) domain.SessionID {
	t.Helper()

	rt := newCompactionHarness(t, store, model, foldWindow)
	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for turn := range turns {
		runTurn(t, rt, session.ID, fmt.Sprintf("turn %d ", turn)+strings.Repeat("y", 3000))
	}
	return session.ID
}

// firstText is the first text block of a request: the oldest fold, when the
// conversation opens with one.
func firstText(req provider.Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	for _, block := range req.Messages[0].Content {
		if text, ok := block.(provider.TextBlock); ok {
			return text.Text
		}
	}
	return ""
}

// A fold is written from the turns it covers and from nothing else. Feeding
// the summariser the fold already in front of the conversation would make
// every fold a summary of a summary: each loses a little, and the beginning
// of a long session is retold through every fold after it until nothing of
// what was actually said survives.
func TestAFoldIsWrittenFromTurnsNotFromEarlierFolds(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	foldedSession(t, store, model, 5)

	asked := model.summaryRequests()
	if len(asked) < 2 {
		t.Fatalf("only %d compactions happened; the test needs at least two", len(asked))
	}
	for i, req := range asked[1:] {
		if text := requestText(req); strings.Contains(text, theSummary) {
			t.Errorf("summarisation %d was handed an earlier summary to condense:\n%s", i+2, text)
		}
	}
}

// Every fold that has been written is still sent, in order: the conversation
// opens with each range of the log condensed once, not with the latest
// retelling of all of them.
func TestEveryFoldStandsInFrontOfTheConversation(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	foldedSession(t, store, model, 5)

	turns := model.turnRequests()
	last := requestText(turns[len(turns)-1])

	folds := model.summaryCount()
	if folds < 2 {
		t.Fatalf("only %d folds were written", folds)
	}
	previous := -1
	for n := 1; n <= folds; n++ {
		at := strings.Index(last, nthSummary(n))
		if at < 0 {
			t.Fatalf("the last request does not carry fold %d of %d:\n%s", n, folds, last)
		}
		if at < previous {
			t.Errorf("fold %d comes before fold %d", n, n-1)
		}
		previous = at
	}
}

// The turn being answered reaches the model, every time. Retention runs the
// moment a fold is written, and the fold is written while the turn's own
// message sits after everything it covers; a cut that reached past the fold
// took that message with it, and the model was asked to answer a summary
// with no question after it.
func TestTheTurnBeingAnsweredIsNeverPruned(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	foldedSession(t, store, model, 5)

	for i, req := range model.turnRequests() {
		if own := fmt.Sprintf("turn %d ", i); !strings.Contains(requestText(req), own) {
			t.Errorf("request %d does not carry its own turn %q", i, own)
		}
	}
}

// A fold once written is sent byte for byte on every later turn. The whole
// point of keeping folds immutable is that the front of the conversation stays
// a prefix a provider can go on caching; a compaction that rewrote it would
// invalidate the cache on the very turn it was trying to make cheaper.
func TestAnEarlierFoldIsSentByteForByteOnLaterTurns(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	foldedSession(t, store, model, 5)

	turns := model.turnRequests()
	// The request for turn 2 is the first to open with a fold; every request
	// after it must open with the same one.
	opening := firstText(turns[1])
	if !strings.Contains(opening, nthSummary(1)) {
		t.Fatalf("the second request does not open with the first fold:\n%s", opening)
	}
	for i, req := range turns[2:] {
		if got := firstText(req); got != opening {
			t.Errorf("request %d opens differently from request 2:\n%q\nwas\n%q", i+3, got, opening)
		}
	}
}

// Folds pile up — one per compaction — and each is bounded but their number
// is not. Once enough stand in front of the conversation they are condensed
// into one, which is the one case where a summary is written from summaries,
// and the summariser is told so.
func TestFoldsAreCondensedOnceTheyPileUp(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	// Nine turns: eight folds, of which the seventh finds six standing in
	// front of it and condenses them, and the eighth is ordinary again.
	foldedSession(t, store, model, 9)

	if folds := model.summaryCount(); folds != 8 {
		t.Fatalf("expected 8 folds from 9 turns, got %d", folds)
	}

	asked := model.summaryRequests()
	condensing := requestText(asked[6])
	if !strings.Contains(condensing, "an earlier summary of this session") {
		t.Errorf("the seventh compaction did not say it was condensing earlier folds:\n%s", condensing)
	}
	for n := 1; n <= 6; n++ {
		if !strings.Contains(condensing, nthSummary(n)) {
			t.Errorf("the seventh compaction did not condense fold %d", n)
		}
	}

	turns := model.turnRequests()
	last := requestText(turns[len(turns)-1])
	for n := 1; n <= 6; n++ {
		if strings.Contains(last, nthSummary(n)) {
			t.Errorf("fold %d is still sent after being condensed", n)
		}
	}
	for _, n := range []int{7, 8} {
		if !strings.Contains(last, nthSummary(n)) {
			t.Errorf("the last request does not carry fold %d", n)
		}
	}
}

// Retention runs after every compaction, and it must never take a fold with
// it: each fold is the only thing standing in for its range, and a rebuild
// with one missing has a hole where that part of the session was.
func TestRetentionKeepsEveryFold(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok", numbered: true}
	session := foldedSession(t, store, model, 5)

	written := model.summaryCount()
	if kept := countEvents(t, store, session, "conversation.compacted"); kept != written {
		t.Errorf("%d folds were written and %d survive in the log", written, kept)
	}

	// And the rebuild after a restart reads all of them.
	restarted := newCompactionHarness(t, store, model, foldWindow)
	runTurn(t, restarted, session, "one more "+strings.Repeat("z", 100))

	turns := model.turnRequests()
	last := requestText(turns[len(turns)-1])
	for n := 1; n <= written; n++ {
		if !strings.Contains(last, nthSummary(n)) {
			t.Errorf("after a restart the conversation lost fold %d of %d", n, written)
		}
	}
}
