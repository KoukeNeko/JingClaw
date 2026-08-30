package tableimage

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/image/font"
)

// TestNothingIsDrawnThatTheTypefaceCannotDraw is the check that matters.
//
// A face asked for a glyph it does not have reports an advance anyway and
// draws the .notdef box, so a missing character does not look missing — it
// looks like a broken program. This was reported from a real table: every
// status cell arrived as a box.
func TestNothingIsDrawnThatTheTypefaceCannotDraw(t *testing.T) {
	fonts := loaded(t)

	for _, said := range []string{
		"✅ 有販售",
		"❌ 目前未上架",
		"⚠️ 部分地區",
		"🎉🎊🥳 nothing but emoji",
		"plain ascii",
		"日本語と中文",
	} {
		got := drawable(fonts.Body, said)
		for _, r := range got {
			if _, ok := fonts.Body.GlyphAdvance(r); !ok {
				t.Errorf("%q kept %q (U+%04X), which the typeface cannot draw; got %q",
					said, r, r, got)
			}
		}
	}
}

// TestTheMarksATableUsesSurviveAsSomethingReadable keeps the fix from being
// "delete everything".
//
// A status column whose cells all became empty would draw cleanly and say
// nothing, which is a worse table than a crooked one.
func TestTheMarksATableUsesSurviveAsSomethingReadable(t *testing.T) {
	fonts := loaded(t)

	for _, one := range []struct {
		said string
		want string
	}{
		{"✅ 有販售", "○ 有販售"},
		{"❌ 目前未上架", "× 目前未上架"},
		{"✔ yes", "○ yes"},
		{"✖ no", "× no"},
	} {
		if got := drawable(fonts.Body, one.said); got != one.want {
			t.Errorf("drawable(%q) = %q, want %q", one.said, got, one.want)
		}
	}
}

// TestWordsAreNeverLost is the other half: substituting must not eat text.
func TestWordsAreNeverLost(t *testing.T) {
	fonts := loaded(t)

	const said = "✅ 已於 2026 年 5 月上架，16,800 日圓（享 10% Bic Points）"
	got := drawable(fonts.Body, said)

	for _, keep := range []string{"已於", "2026", "16,800", "日圓", "10%", "Bic Points"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was lost: %q", keep, got)
		}
	}
}

// TestAnEmojiWithNoSubstituteLeavesASpaceRatherThanABox says what happens to
// everything not in the small map: it reads as absence, not as damage.
func TestAnEmojiWithNoSubstituteLeavesASpaceRatherThanABox(t *testing.T) {
	fonts := loaded(t)

	got := drawable(fonts.Body, "🎉 done")
	if strings.HasPrefix(got, "🎉") {
		t.Fatalf("the emoji survived: %q", got)
	}
	if !strings.HasSuffix(got, "done") {
		t.Errorf("the words did not: %q", got)
	}
}

// TestWhatIsMeasuredIsWhatIsDrawn is the invariant the substitution has to
// keep.
//
// Columns are sized from the text and then the picture is painted from it. If
// the two ever see different strings the table is laid out for one and drawn
// with another, which is the bug this whole package exists to avoid.
func TestWhatIsMeasuredIsWhatIsDrawn(t *testing.T) {
	fonts := loaded(t)

	table := Table{
		Header: []string{"調查維度", "✅ 販售情況"},
		Rows:   [][]string{{"Google Fitbit Air", "✅ 有販售"}, {"Curry 特別版", "❌ 未上架"}},
	}

	cleaned := drawableTable(table, fonts)

	// Idempotent: running it again changes nothing, which is what says the
	// output is drawable rather than merely different.
	again := drawableTable(cleaned, fonts)
	if !sameTable(cleaned, again) {
		t.Errorf("cleaning is not settled: %+v then %+v", cleaned, again)
	}

	// And the width the layout computes is the width of what will be painted.
	for _, row := range cleaned.Rows {
		for _, text := range row {
			if measure(fonts.Body, text) != widthOfRunes(fonts.Body, text) {
				t.Errorf("%q measures differently than the sum of its glyphs", text)
			}
		}
	}
}

func widthOfRunes(face font.Face, text string) int {
	var total int
	var previous rune
	for index, r := range text {
		advance, _ := face.GlyphAdvance(r)
		total += advance.Ceil()
		if index > 0 {
			total += face.Kern(previous, r).Ceil()
		}
		previous = r
	}
	return total
}

func sameTable(a, b Table) bool {
	if len(a.Header) != len(b.Header) || len(a.Rows) != len(b.Rows) {
		return false
	}
	for i := range a.Header {
		if a.Header[i] != b.Header[i] {
			return false
		}
	}
	for i := range a.Rows {
		if len(a.Rows[i]) != len(b.Rows[i]) {
			return false
		}
		for j := range a.Rows[i] {
			if a.Rows[i][j] != b.Rows[i][j] {
				return false
			}
		}
	}
	return true
}

// TestDrawItselfSubstitutes closes the gap the test above leaves.
//
// Checking drawableTable directly proves the function works and says nothing
// about whether Draw calls it — and Draw is the only way anybody reaches this
// package. Removing the call left every test here passing.
//
// Two pictures, byte for byte. A table written with the mark the typeface
// lacks must come out as the same image as one written with the substitute,
// because by the time anything is measured they are the same table.
func TestDrawItselfSubstitutes(t *testing.T) {
	fonts := loaded(t)

	withEmoji, err := Draw(Table{
		Header: []string{"狀態", "說明"},
		Rows:   [][]string{{"✅ 有販售", "在架上"}, {"❌ 未上架", "沒有"}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	withMarks, err := Draw(Table{
		Header: []string{"狀態", "說明"},
		Rows:   [][]string{{"○ 有販售", "在架上"}, {"× 未上架", "沒有"}},
	}, fonts)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	if !bytes.Equal(withEmoji, withMarks) {
		t.Errorf("Draw did not substitute: the emoji table is %d bytes and the "+
			"marked one %d; they should be the same picture",
			len(withEmoji), len(withMarks))
	}
}

// TestAModifierLeavesNoGapWhereItHadNoWidth covers the runes that are there
// to change their neighbour rather than to be seen.
//
// "⚠️" is two runes: the sign and a variation selector asking for the emoji
// form. The selector has no width in the text it came from, so turning it
// into a space puts a hole after the mark that was not there before.
func TestAModifierLeavesNoGapWhereItHadNoWidth(t *testing.T) {
	fonts := loaded(t)

	for _, one := range []struct {
		said string
		want string
	}{
		// The sign survives; only the selector after it goes.
		{"⚠️ 其他通路", "⚠ 其他通路"},
		{"✅️ yes", "○ yes"},
		{"\U0001F44D\U0001F3FD ok", "ok"},
	} {
		if got := drawable(fonts.Body, one.said); got != one.want {
			t.Errorf("drawable(%q) = %q, want %q", one.said, got, one.want)
		}
	}
}
