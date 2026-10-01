package storage

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// EncodeVector is a vector as stored: little-endian float32s, four bytes each.
func EncodeVector(vector []float32) []byte {
	out := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(value))
	}
	return out
}

// DecodeVector reads what EncodeVector wrote.
func DecodeVector(raw []byte) ([]float32, error) {
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("storage: a vector of %d bytes is not float32s", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return out, nil
}

// Cosine is how alike two vectors point, from -1 to 1. Vectors of different
// lengths came from different models and are not alike at all.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}

// Nearest keeps the candidates at least min alike to the vector, closest
// first, at most limit of them (zero means all). Both stores rank with it, so
// they cannot disagree about what is near.
func Nearest(
	vector []float32,
	candidates []domain.Memory,
	vectors [][]float32,
	min float64,
	limit int,
) []domain.Memory {
	type scored struct {
		memory     domain.Memory
		similarity float64
	}
	var kept []scored
	for i, candidate := range candidates {
		if similarity := Cosine(vector, vectors[i]); similarity >= min {
			kept = append(kept, scored{candidate, similarity})
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].similarity > kept[j].similarity })
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}

	out := make([]domain.Memory, len(kept))
	for i, item := range kept {
		out[i] = item.memory
	}
	return out
}
