package tableimage

import (
	"strings"
	"unicode"

	"golang.org/x/image/font"
)

// drawable replaces what the typeface cannot draw.
//
// A face asked for a glyph it does not have still reports an advance and
// still draws something: the .notdef box. So a cell that said "✅ 有販售"
// arrives as a box followed by the words, and the box is worse than nothing —
// it looks like a broken program rather than a missing character.
//
// This is the only place text is changed, and it runs before anything is
// measured. Substituting later would mean the columns were sized for one
// string and the picture drawn with another.
func drawable(face font.Face, text string) string {
	if text == "" {
		return ""
	}

	var out strings.Builder
	out.Grow(len(text))

	// Whether the last thing written was a space standing in for something
	// undrawable. It decides whether the next space is worth writing: "🎉 ok"
	// would otherwise become two spaces and then the word, having had one.
	var placeholder bool

	for _, r := range text {
		// Characters that were never meant to be seen: the variation selector
		// after an emoji, the joiner between two of them, a skin tone. They
		// occupy no width in the text they came from, so replacing one with a
		// space would put a gap where the original had nothing — "⚠️" is two
		// runes and would arrive as a mark followed by a hole.
		if invisible(r) {
			continue
		}

		if _, ok := face.GlyphAdvance(r); ok {
			if unicode.IsSpace(r) && placeholder {
				continue
			}
			out.WriteRune(r)
			placeholder = false
			continue
		}

		if instead, known := substitutes[r]; known {
			// Only if the substitute is itself drawable. A mapping written
			// against one typeface is not a promise about the next one.
			if _, ok := face.GlyphAdvance(instead); ok {
				out.WriteRune(instead)
				placeholder = false
				continue
			}
		}

		// Nothing to put here. A space rather than a deletion, so a mark that
		// stood between two words does not join them — but only one, however
		// many undrawable runes were in a row.
		if !placeholder {
			out.WriteRune(' ')
			placeholder = true
		}
	}

	// Space that only ever stood in for something missing is not spacing. A
	// table cell has no meaningful leading or trailing whitespace anyway: the
	// markdown it came from was trimmed before it got here.
	return strings.TrimSpace(out.String())
}

// invisible reports whether a rune is there to modify its neighbour rather
// than to be drawn.
func invisible(r rune) bool {
	switch {
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors, supplement
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tone modifiers
		return true
	case r == 0x200D: // zero width joiner
		return true
	}
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r)
}

// substitutes are the marks that no typeface here can draw, in a form one of
// them can.
//
// Shorter than it was, because the chain reaches further than one face did: ✓
// ✗ ⚠ → are drawn as themselves now, from whichever typeface has them. What
// is left is emoji, and emoji genuinely cannot be drawn — the colour formats
// they live in are bitmap and layered-vector tables this rasterizer does not
// read, and the one font on a Mac that has ✅ is one of them.
//
// ○ and × rather than ticks, and not as a compromise: they are the
// conventional yes and no in Chinese and Japanese writing, which is the
// writing these tables are usually in.
//
// Deliberately short. Anything not here becomes a space, which reads as
// absence instead of as damage.
var substitutes = map[rune]rune{
	'✅': '○', // white heavy check mark
	'✔': '○',
	'☑': '○',
	'🟢': '○',
	'⭕': '○',

	'❌': '×', // cross mark
	'✖': '×',
	'✗': '×',
	'☒': '×',
	'🔴': '×',

	'⚠': '!', // warning
	'❗': '!',
	'❓': '?',

	'🟡': '△', // partly, which is the third of the three CJK marks
	'🟠': '△',

	'•': '·',
}
