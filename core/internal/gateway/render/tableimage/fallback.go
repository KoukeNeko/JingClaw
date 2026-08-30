package tableimage

import (
	"image"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// fallback draws each rune with the first typeface that has it.
//
// font.Face is deliberately small — a glyph, its bounds, its advance — and
// every one of those says whether the rune exists. So a face made of several
// faces needs nothing the interface does not already offer, which is why this
// is thirty lines rather than a text stack.
//
// What it is not is shaping. One rune becomes one glyph here, so Arabic
// contextual forms, Indic clusters and emoji joined by ZWJ are all outside
// what this can do. That is the boundary at which this would have to become
// go-text/typesetting, and a table of Latin, CJK and status marks is nowhere
// near it.
type fallback struct {
	faces []font.Face

	// chosen remembers which face answered for a rune. A table repeats the
	// same few hundred runes down a column, and asking six typefaces about
	// each of them every time is the difference between drawing a table and
	// parsing fonts.
	mu     sync.Mutex
	chosen map[rune]font.Face
}

var _ font.Face = (*fallback)(nil)

// chain makes one face of several. The first is the primary: it decides the
// metrics, and it is what an unknown rune falls back to.
func chain(faces ...font.Face) font.Face {
	kept := make([]font.Face, 0, len(faces))
	for _, face := range faces {
		if face != nil {
			kept = append(kept, face)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return &fallback{faces: kept, chosen: make(map[rune]font.Face)}
}

// faceFor is the first face that has the rune, or the primary.
//
// The primary, not nothing: a rune nobody has still needs an answer, and the
// answer is the .notdef box the primary draws. What stops that reaching a
// picture is the substitution in drawable, which runs first and knows to ask
// this same question.
func (f *fallback) faceFor(r rune) font.Face {
	f.mu.Lock()
	defer f.mu.Unlock()

	if known, ok := f.chosen[r]; ok {
		return known
	}

	found := f.faces[0]
	for _, face := range f.faces {
		if _, ok := face.GlyphAdvance(r); ok {
			found = face
			break
		}
	}
	f.chosen[r] = found
	return found
}

func (f *fallback) Glyph(dot fixed.Point26_6, r rune) (
	image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool,
) {
	return f.faceFor(r).Glyph(dot, r)
}

func (f *fallback) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	return f.faceFor(r).GlyphBounds(r)
}

func (f *fallback) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	return f.faceFor(r).GlyphAdvance(r)
}

// Kern is zero across a boundary between typefaces.
//
// Two faces have no shared opinion about the space between their glyphs, and
// one's kerning table says nothing about the other's shapes. Zero is the
// honest answer; asking either face would be asking about a pair it has never
// seen.
func (f *fallback) Kern(a, b rune) fixed.Int26_6 {
	first := f.faceFor(a)
	if first != f.faceFor(b) {
		return 0
	}
	return first.Kern(a, b)
}

// Metrics is an envelope big enough for every face in the chain.
//
// The primary's alone would be wrong the moment a fallback is taller: a row
// sized to a Latin face and filled with CJK gets clipped, and the clipping
// looks like a rendering bug rather than a missing measurement.
func (f *fallback) Metrics() font.Metrics {
	widest := f.faces[0].Metrics()
	for _, face := range f.faces[1:] {
		other := face.Metrics()
		widest.Height = max(widest.Height, other.Height)
		widest.Ascent = max(widest.Ascent, other.Ascent)
		widest.Descent = max(widest.Descent, other.Descent)
		widest.XHeight = max(widest.XHeight, other.XHeight)
		widest.CapHeight = max(widest.CapHeight, other.CapHeight)
	}
	return widest
}

// Close releases every face in the chain.
func (f *fallback) Close() error {
	for _, face := range f.faces {
		_ = face.Close()
	}
	return nil
}
