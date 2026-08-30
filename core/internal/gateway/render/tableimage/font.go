// Package tableimage draws a table as a picture.
//
// For a chat platform that renders a code block in whatever font it happens
// to have. An aligned table only stays aligned if the thing drawing it agrees
// about how wide each glyph is, and on Discord that is decided by a fallback
// font nobody here chose — so a table of Chinese and Latin text arrives bent
// however carefully its columns were counted.
//
// A picture is the layout, rather than a description of one somebody else
// draws. What it costs is real and is not hidden: the text in it cannot be
// selected, searched, or read by a screen reader.
package tableimage

import (
	"fmt"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// needed is what a table has to be able to draw before this stops looking.
//
// Chinese first, because a table without it is the thing this package exists
// to avoid, and because a typeface that has it nearly always has Latin too.
// Then the marks a status column uses, which on most systems live in a
// different file entirely.
var needed = struct{ script, marks []rune }{
	script: []rune{'漢', '的', 'の', 'A'},
	marks:  []rune{'✓', '✗', '⚠', '○', '×', '△', '→', '—'},
}

// mostFaces bounds the chain.
//
// Every face in it is asked about every unseen rune, and each one was a file
// read and parsed. Three is enough for the two things above plus something
// unexpected; a machine that needs more than three typefaces to draw a table
// of Latin, Chinese and ticks has something stranger wrong with it.
const mostFaces = 3

// Fonts is a loaded typeface at the two weights a table uses.
type Fonts struct {
	Header font.Face
	Body   font.Face
}

// Close releases both faces.
func (f Fonts) Close() error {
	if f.Header != nil {
		_ = f.Header.Close()
	}
	if f.Body != nil {
		_ = f.Body.Close()
	}
	return nil
}

// Load finds typefaces that between them can draw a table.
//
// Chosen by what each one actually contains rather than by what it is called.
// A filename says which typeface somebody packaged, not which characters it
// has, and this package was for a long time picking a face with Chinese and
// no tick and then drawing boxes where the ticks went.
//
// Several, because no single one is enough. On macOS the Chinese typefaces
// have no ✓ ✗ ⚠ at all and the interface typeface has no Chinese; they are
// different files and always were.
func Load(paths []string) (Fonts, error) {
	if len(paths) == 0 {
		paths = searchOrder()
	}

	var header, body []font.Face
	release := func() {
		for _, face := range append(header, body...) {
			_ = face.Close()
		}
	}

	// What is still missing. A typeface earns a place in the chain by
	// covering something nothing before it did — so a machine with one
	// typeface that has everything loads one, and nothing is read twice for
	// the sake of a list.
	missing := append(append([]rune{}, needed.script...), needed.marks...)
	haveScript := false

	for _, path := range paths {
		if len(header) >= mostFaces || len(missing) == 0 {
			break
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed, err := parse(raw)
		if err != nil {
			continue
		}

		first, err := faceFrom(parsed)
		if err != nil {
			continue
		}

		// The first face has to be one with the script: it decides the
		// metrics, and a chain led by a Latin typeface lays out Chinese rows
		// at Latin heights.
		covers, remaining := coverage(first, missing)
		if !haveScript && !covers(needed.script) {
			_ = first.Close()
			continue
		}
		if haveScript && len(remaining) == len(missing) {
			_ = first.Close()
			continue
		}

		second, err := faceFrom(parsed)
		if err != nil {
			_ = first.Close()
			continue
		}

		header = append(header, first)
		body = append(body, second)
		missing = remaining
		haveScript = true
	}

	if len(header) == 0 {
		release()
		return Fonts{}, fmt.Errorf(
			"tableimage: none of the %d typefaces on this machine has Chinese glyphs", len(paths))
	}

	return Fonts{Header: chain(header...), Body: chain(body...)}, nil
}

// coverage says whether a face has a set of runes, and what is left over.
func coverage(face font.Face, missing []rune) (func([]rune) bool, []rune) {
	has := func(r rune) bool {
		_, ok := face.GlyphAdvance(r)
		return ok
	}

	remaining := make([]rune, 0, len(missing))
	for _, r := range missing {
		if !has(r) {
			remaining = append(remaining, r)
		}
	}

	return func(wanted []rune) bool {
		for _, r := range wanted {
			if !has(r) {
				return false
			}
		}
		return true
	}, remaining
}

// faceFrom is one face at the size a table is drawn.
//
// Two are made from each typeface because a Face carries a glyph cache and is
// not safe to use from two places at once, and a table measures its header
// while it draws its body.
func faceFrom(parsed *sfnt.Font) (font.Face, error) {
	return opentype.NewFace(parsed, &opentype.FaceOptions{
		Size: pointSize, DPI: density, Hinting: font.HintingFull,
	})
}

// parse reads a font file, which may hold one typeface or several.
//
// The system fonts that carry Chinese are nearly all collections — one file
// holding a whole family — and reading one as a single font simply fails.
func parse(raw []byte) (*sfnt.Font, error) {
	if collection, err := sfnt.ParseCollection(raw); err == nil && collection.NumFonts() > 0 {
		return collection.Font(0)
	}
	return sfnt.Parse(raw)
}
