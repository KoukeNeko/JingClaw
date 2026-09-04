package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
)

const theLabel = "[Noted in earlier conversations: NOTED-LABEL]"

// labelWhen answers Recall with the label for turns containing `word`, and
// counts how often it was asked.
type labelWhen struct {
	mu    sync.Mutex
	word  string
	asked int
}

func (l *labelWhen) recall(_ context.Context, _ domain.Run, said string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked++
	if strings.Contains(said, l.word) {
		return theLabel
	}
	return ""
}

// blocksOf returns the text blocks of a message, in order.
func blocksOf(message provider.Message) []provider.TextBlock {
	var blocks []provider.TextBlock
	for _, block := range message.Content {
		if text, ok := block.(provider.TextBlock); ok {
			blocks = append(blocks, text)
		}
	}
	return blocks
}

// What was noted goes in front of the turn being answered, as a label, after
// the line saying when and from whom and before the person's own words — and
// only on that turn. An earlier turn is history, and history is not edited on
// the way out.
func TestWhatWasNotedGoesInFrontOfTheTurnBeingAnswered(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}
	labels := &labelWhen{word: "second"}
	rt := newCompactionHarness(t, store, model, 0, func(o *runtime.Options) {
		o.Recall = labels.recall
	})

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runTurn(t, rt, session.ID, "the first question")
	runTurn(t, rt, session.ID, "the second question")

	turns := model.turnRequests()
	if len(turns) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(turns))
	}

	if text := requestText(turns[0]); strings.Contains(text, theLabel) {
		t.Errorf("the first turn, which had nothing noted, carries the label:\n%s", text)
	}

	last := turns[1].Messages[len(turns[1].Messages)-1]
	blocks := blocksOf(last)
	labelAt, wordsAt := -1, -1
	for i, block := range blocks {
		switch {
		case block.Text == theLabel:
			labelAt = i
			if !block.Annotation {
				t.Error("the label is not marked as written by this machine")
			}
		case strings.Contains(block.Text, "the second question"):
			wordsAt = i
		}
	}
	if labelAt < 0 {
		t.Fatalf("the turn being answered carries no label:\n%+v", blocks)
	}
	if wordsAt < labelAt {
		t.Errorf("the label comes after the person's words")
	}
	if labelAt == 0 {
		t.Errorf("the label displaced the line saying when the turn was sent")
	}

	// And the first turn, now history in the second request, is untouched.
	for _, block := range blocksOf(turns[1].Messages[0]) {
		if block.Text == theLabel {
			t.Error("the label was put on an earlier turn")
		}
	}
}

// AfterRun hears about the turns that were answered: not a turn that failed,
// and not until it is over.
func TestAfterRunIsToldAboutAnsweredTurnsOnly(t *testing.T) {
	store := memory.New()
	model := &compactingProvider{reply: "ok"}

	told := make(chan domain.RunID, 8)
	rt := newCompactionHarness(t, store, model, 0, func(o *runtime.Options) {
		o.AfterRun = func(_ context.Context, run domain.Run) {
			if run.Status != domain.RunCompleted {
				t.Errorf("told about a run in state %s", run.Status)
			}
			told <- run.ID
		}
	})

	session, err := rt.CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first := runTurn(t, rt, session.ID, "one")

	model.failTurns(errors.New("the model is down"))
	failed := runTurn(t, rt, session.ID, "two")
	model.failTurns(nil)

	third := runTurn(t, rt, session.ID, "three")

	heard := map[domain.RunID]bool{}
	for len(heard) < 2 {
		select {
		case id := <-told:
			heard[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("after %d, nobody was told about the answered turns", len(heard))
		}
	}
	if !heard[first] || !heard[third] {
		t.Errorf("told about %v; wanted %s and %s", heard, first, third)
	}
	if heard[failed] {
		t.Errorf("told about the turn that failed, %s", failed)
	}
}
