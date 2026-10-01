package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/storage/memory"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
)

// topics is an embedder that knows a few subjects. A text is near whichever
// subjects it mentions any word of, whatever language the word is in, which
// is the property a real model has and a word index does not.
type topics struct {
	failing bool
	asked   atomic.Int32
}

var subjects = [][]string{
	{"control plane", "kubernetes", "masters", "cluster"},
	{"traditional chinese", "語言", "中文", "language"},
	{"gofmt", "code style", "程式碼風格"},
}

func (*topics) Model() string { return "topics-1" }

func (e *topics) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.asked.Add(1)
	if e.failing {
		return nil, errors.New("the embedding server is down")
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		// A small constant in every dimension keeps unrelated texts from
		// being exactly orthogonal, the way real ones never are.
		vector := make([]float32, len(subjects))
		for dim := range vector {
			vector[dim] = 0.05
		}
		lower := strings.ToLower(text)
		for dim, words := range subjects {
			for _, word := range words {
				if strings.Contains(lower, word) {
					vector[dim] = 1
				}
			}
		}
		out[i] = vector
	}
	return out, nil
}

func newSemanticTools(t *testing.T, embedder memorytool.Embedder) (
	*memorytool.Remember, *memorytool.Recall, memorytool.Options,
) {
	t.Helper()
	var counter atomic.Uint64
	options := memorytool.Options{
		Store:        memory.New(),
		WorkspaceRef: workspace,
		NewID:        func() string { return fmt.Sprintf("mem_%d", counter.Add(1)) },
		Clock:        func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Embedder:     embedder,
	}
	return &memorytool.Remember{Options: options}, &memorytool.Recall{Options: options}, options
}

// The reason for any of this: the same thing in other words, or in the other
// language, shares no word with the note and is found anyway.
func TestAMemoryIsFoundByWhatItMeans(t *testing.T) {
	write, read, _ := newSemanticTools(t, &topics{})

	remember(t, write, localTurn(), map[string]any{
		"text": "the production cluster must not have its control plane restarted"})
	remember(t, write, localTurn(), map[string]any{
		"text": "answer in Traditional Chinese as used in Taiwan"})

	for query, want := range map[string]string{
		"can I bounce the kubernetes masters": "control plane",
		"回覆要用什麼語言":                            "Traditional Chinese",
	} {
		found := recall(t, read, localTurn(), map[string]any{"query": query})
		if !strings.Contains(found, want) {
			t.Errorf("%q did not find the note about %q:\n%s", query, want, found)
		}
	}
}

// Nothing near enough is nothing. A store of vectors always has a nearest one,
// and putting it in front of every turn is noise with the authority of a note.
func TestNothingNearEnoughIsNothing(t *testing.T) {
	write, read, _ := newSemanticTools(t, &topics{})

	remember(t, write, localTurn(), map[string]any{
		"text": "the production cluster must not have its control plane restarted"})

	found := recall(t, read, localTurn(), map[string]any{"query": "run gofmt please"})
	if strings.Contains(found, "control plane") {
		t.Errorf("an unrelated question found a note:\n%s", found)
	}
}

// When the embedder is down, search is by words, and nothing fails for it.
func TestAFailingEmbedderLeavesTheWords(t *testing.T) {
	embedder := &topics{failing: true}
	write, read, _ := newSemanticTools(t, embedder)

	remember(t, write, localTurn(), map[string]any{"text": "the deploy script needs sudo"})

	found := recall(t, read, localTurn(), map[string]any{"query": "deploy"})
	if !strings.Contains(found, "needs sudo") {
		t.Errorf("the word search did not survive the embedder failing:\n%s", found)
	}
	if embedder.asked.Load() == 0 {
		t.Error("the embedder was never asked, so this tested nothing")
	}
}

// A memory written while the embedder was down, or before there was one, is
// caught up later.
func TestMissingVectorsAreCaughtUp(t *testing.T) {
	embedder := &topics{failing: true}
	write, read, options := newSemanticTools(t, embedder)

	remember(t, write, localTurn(), map[string]any{
		"text": "the production cluster must not have its control plane restarted"})

	embedder.failing = false
	done, err := options.EmbedMissing(context.Background())
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if done != 1 {
		t.Errorf("caught up %d memories, want 1", done)
	}
	again, err := options.EmbedMissing(context.Background())
	if err != nil || again != 0 {
		t.Errorf("a second catch-up did %d (%v), want nothing", again, err)
	}

	found := recall(t, read, localTurn(), map[string]any{"query": "can I bounce the kubernetes masters"})
	if !strings.Contains(found, "control plane") {
		t.Errorf("a caught-up memory was not found by meaning:\n%s", found)
	}
}

// Meaning does not widen whose memories a turn may read.
func TestMeaningRespectsWhoIsAsking(t *testing.T) {
	write, read, _ := newSemanticTools(t, &topics{})

	remember(t, write, gatewayTurn("111"), map[string]any{
		"text": "the production cluster must not have its control plane restarted", "scope": "person"})

	found := recall(t, read, gatewayTurn("222"), map[string]any{"query": "can I bounce the kubernetes masters"})
	if strings.Contains(found, "control plane") {
		t.Errorf("another person's note was found by meaning:\n%s", found)
	}
}
