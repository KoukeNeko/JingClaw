package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const (
	// defaultMinSimilarity is how alike a memory has to be to a turn to be
	// put in front of it by meaning alone, unasked. Measured, not chosen:
	// with qwen3-embedding:0.6b the closest note to an unrelated turn never
	// passed 0.553. Another model draws that line elsewhere, which is why it
	// is configurable.
	defaultMinSimilarity = 0.56

	// defaultRecallMinSimilarity is the same line for a search somebody
	// asked for. Lower, because whoever asked reads what comes back and
	// judges it: on the same model, the right note for a question in other
	// words or the other language was the nearest one at 0.49 to 0.54, which
	// the stricter line drops.
	defaultRecallMinSimilarity = 0.45

	// meaningWeight is how much a place in the list by meaning counts
	// against the same place in the list by words. More, so that a near
	// miss on a common word does not outrank the note that means the same
	// thing; a memory both lists find still comes first.
	meaningWeight = 1.5

	// defaultEmbedTimeout bounds one embedding. A turn waits on the one for
	// what it said, and a turn that waits on a slow vector instead of
	// starting is a worse outcome than a turn recalled by its words.
	defaultEmbedTimeout = 10 * time.Second

	// rankConstant damps how much the top of either list outweighs the
	// rest when the two are merged. Sixty is what the method was published
	// with, and nothing here is tuned finely enough to want another.
	rankConstant = 60

	// embedBatch is how many memories go in one request when catching up.
	embedBatch = 32

	// maxQueryRunes is how much of what was said is embedded to search
	// with. A turn can be a pasted log many times longer than a model takes,
	// and what it is about is at the start; a memory itself is bounded far
	// below this when it is written.
	maxQueryRunes = 2000
)

// Embedder turns text into vectors. Model names what made them: vectors from
// two models are not comparable, so a stored vector is kept per model.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// search finds memories by their words and, when there is an embedder, by
// what they mean.
//
// The word index finds what was said the way it was written — a name, a
// path, a flag — and misses the same thing in other words or the other
// language. Vectors are the opposite. Each list is ranked on its own and the
// two are merged by rank, so a memory near the top of either comes back.
//
// The meaning half is optional. Without an embedder, or when it fails, this
// is the word search, and the turn is answered either way.
//
// floor is how alike a memory has to be to be found by meaning alone:
// stricter for what is put in front of a turn unasked than for what the
// model asked for.
func (o Options) search(
	ctx context.Context,
	text string,
	query storage.MemoryQuery,
	floor float64,
) ([]domain.Memory, error) {
	byWords, err := o.Store.SearchMemories(ctx, text, query)
	if err != nil {
		return nil, err
	}
	if o.Embedder == nil || strings.TrimSpace(text) == "" {
		return byWords, nil
	}

	vectors, err := o.embed(ctx, []string{clipRunes(text, maxQueryRunes)})
	if err != nil {
		o.Logger().Warn("could not embed a memory search; searching by words only", "error", err)
		return byWords, nil
	}
	byMeaning, err := o.Store.NearestMemories(ctx, o.Embedder.Model(), vectors[0], floor, query)
	if err != nil {
		o.Logger().Warn("could not search memories by meaning", "error", err)
		return byWords, nil
	}

	return mergeByRank(query.Limit, byWords, byMeaning), nil
}

// mergeByRank is weighted reciprocal rank fusion: each memory scores
// weight/(k+rank) summed over the lists it is in, the list by meaning
// weighing meaningWeight. Exact ties keep the order of first appearance.
func mergeByRank(limit int, byWords, byMeaning []domain.Memory) []domain.Memory {
	score := map[domain.MemoryID]float64{}
	var order []domain.Memory
	add := func(list []domain.Memory, weight float64) {
		for rank, memory := range list {
			if _, seen := score[memory.ID]; !seen {
				order = append(order, memory)
			}
			score[memory.ID] += weight / float64(rankConstant+rank+1)
		}
	}
	add(byWords, 1)
	add(byMeaning, meaningWeight)

	merged := order
	sort.SliceStable(merged, func(i, j int) bool { return score[merged[i].ID] > score[merged[j].ID] })
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// remembered gives memories just written their vectors.
//
// After the write and never instead of it: a memory whose embedding failed is
// still a memory, found by its words today and by meaning once EmbedMissing
// catches it up.
func (o Options) remembered(ctx context.Context, written []domain.Memory) {
	if o.Embedder == nil || len(written) == 0 {
		return
	}
	if err := o.storeVectors(ctx, written); err != nil {
		o.Logger().Warn("could not embed what was remembered; it will be caught up later",
			"memories", len(written), "error", err)
	}
}

// EmbedMissing gives a vector to every believed memory that has none from the
// current model, and says how many it did. For a store that predates the
// embedder, a model that changed, or a write whose embedding failed.
func (o Options) EmbedMissing(ctx context.Context) (int, error) {
	if o.Embedder == nil {
		return 0, nil
	}
	done := 0
	for {
		missing, err := o.Store.UnembeddedMemories(ctx, o.Embedder.Model(), embedBatch)
		if err != nil {
			return done, err
		}
		if len(missing) == 0 {
			return done, nil
		}
		if err := o.storeVectors(ctx, missing); err != nil {
			return done, err
		}
		done += len(missing)
	}
}

func (o Options) storeVectors(ctx context.Context, memories []domain.Memory) error {
	texts := make([]string, len(memories))
	for i, memory := range memories {
		texts[i] = memory.Text
	}
	vectors, err := o.embed(ctx, texts)
	if err != nil {
		return err
	}
	for i, memory := range memories {
		if err := o.Store.SetMemoryVector(ctx, memory.ID, o.Embedder.Model(), vectors[i]); err != nil {
			return err
		}
	}
	return nil
}

func (o Options) embed(ctx context.Context, texts []string) ([][]float32, error) {
	timeout := o.EmbedTimeout
	if timeout <= 0 {
		timeout = defaultEmbedTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vectors, err := o.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("asked for %d embeddings, got %d", len(texts), len(vectors))
	}
	return vectors, nil
}

// searchAsked is search for the recall tool and the curator.
func (o Options) searchAsked(ctx context.Context, text string, query storage.MemoryQuery) ([]domain.Memory, error) {
	return o.search(ctx, text, query, o.askedFloor())
}

// searchUnasked is search for the notes put in front of a turn.
func (o Options) searchUnasked(ctx context.Context, text string, query storage.MemoryQuery) ([]domain.Memory, error) {
	return o.search(ctx, text, query, o.unaskedFloor())
}

// unaskedFloor is the line for notes put in front of a turn nobody asked for.
func (o Options) unaskedFloor() float64 {
	if o.MinSimilarity > 0 {
		return o.MinSimilarity
	}
	return defaultMinSimilarity
}

// askedFloor is the line for a search the model or the curator asked for.
func (o Options) askedFloor() float64 {
	if o.RecallMinSimilarity > 0 {
		return o.RecallMinSimilarity
	}
	return defaultRecallMinSimilarity
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
