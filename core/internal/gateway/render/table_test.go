package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	jcgateway "github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// width is measured the same way the renderer measures it. Asserting against
// a fixed string instead would break whenever Unicode revises the width of an
// emoji, which is a change in the world rather than a defect here.
func width(text string) int { return ansi.GraphemeWidth.StringWidth(text) }

// Every line has to be the same width on screen, or the columns do not line
// up — which is the entire point of rendering a table this way.
func TestEveryLineIsTheSameWidth(t *testing.T) {
	table := render.Table{
		Header: []string{"角色", "官方譯名", "note"},
		Rows: [][]string{
			{"故事舞台 (Kivotos)", "奇普托斯", "long"},
			{"白子", "Shiroko", ""},
			{"é", "combining", "x"},
		},
	}

	lines := table.Aligned()
	if len(lines) == 0 {
		t.Fatal("nothing was rendered")
	}

	// The trailing column is trimmed, so compare up to the last separator:
	// what has to align is every column but the last. The rule under the
	// header joins with a different glyph and is checked on its own.
	want := -1
	for _, line := range lines {
		cut := strings.LastIndex(line, "│")
		if cut < 0 {
			// The rule line, which is full width because nothing on it is
			// trimmed.
			if got := width(line); got != table.Width() {
				t.Errorf("the rule is %d wide, want %d: %q", got, table.Width(), line)
			}
			continue
		}
		got := width(line[:cut])
		if want < 0 {
			want = got
			continue
		}
		if got != want {
			t.Errorf("line %q is %d wide up to its last separator, want %d",
				line, got, want)
		}
	}
}

// A cell of Chinese is twice as wide as its rune count, and a cell of English
// is not. Padding by rune count is the mistake this guards against: it is the
// obvious implementation and it produces a table that looks correct in the
// test file and crooked on screen.
func TestChineseIsPaddedByWidthNotByRuneCount(t *testing.T) {
	table := render.Table{
		Header: []string{"a", "b"},
		Rows:   [][]string{{"中文", "x"}, {"abcd", "y"}},
	}

	lines := table.Aligned()
	// "中文" is 2 runes and 4 columns; "abcd" is 4 of both. Padded by runes,
	// the first row would be two columns short.
	if width(lines[2][:strings.LastIndex(lines[2], "│")]) !=
		width(lines[3][:strings.LastIndex(lines[3], "│")]) {
		t.Errorf("a Chinese cell and a Latin one of the same width do not align:\n%s\n%s",
			lines[2], lines[3])
	}
}

// Emoji width is not promised on Discord — the desktop and iOS clients use
// different emoji fonts — but the renderer must still measure a flag as one
// glyph rather than as two regional indicators, or the padding is wrong by a
// whole column before any font is involved.
func TestAFlagIsOneGlyphWide(t *testing.T) {
	if got := width("🇹🇼"); got != 2 {
		t.Errorf("a flag measured %d columns, want 2", got)
	}

	table := render.Table{
		Header: []string{"flag", "name"},
		Rows:   [][]string{{"🇹🇼", "one"}, {"ab", "two"}},
	}

	lines := table.Aligned()
	if width(lines[2][:strings.LastIndex(lines[2], "│")]) !=
		width(lines[3][:strings.LastIndex(lines[3], "│")]) {
		t.Errorf("a flag does not align with two Latin characters:\n%s\n%s",
			lines[2], lines[3])
	}
}

// A model that has been reading terminal output pastes escape sequences
// sooner or later. They are zero columns wide however many bytes they are.
func TestEscapeSequencesAreNotCounted(t *testing.T) {
	if got := width("\x1b[31mred\x1b[0m"); got != 3 {
		t.Errorf("an escaped cell measured %d columns, want 3", got)
	}
}

// A short row must not make the columns before it collapse: a table where one
// row has fewer cells is a table a model produced, not an error.
func TestAShortRowStillAligns(t *testing.T) {
	table := render.Table{
		Header: []string{"one", "two", "three"},
		Rows:   [][]string{{"a", "b", "c"}, {"d"}},
	}

	lines := table.Aligned()
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 4", len(lines))
	}
	if !strings.HasPrefix(lines[3], "d  ") {
		t.Errorf("the short row was not padded: %q", lines[3])
	}
}

// Width decides whether a fixed-width rendering is attempted at all, so it has
// to account for the separators rather than only the cells.
func TestWidthCountsTheSeparators(t *testing.T) {
	table := render.Table{
		Header: []string{"ab", "cd"},
		Rows:   [][]string{{"ef", "gh"}},
	}

	// Two columns of two, plus " │ " between them.
	if got := table.Width(); got != 7 {
		t.Errorf("width is %d, want 7", got)
	}

	rendered := table.Aligned()[0]
	if got := width(rendered); got != table.Width() {
		t.Errorf("Width says %d but a rendered line is %d: %q",
			table.Width(), got, rendered)
	}
}

func TestAnEmptyTableRendersNothing(t *testing.T) {
	if lines := (render.Table{}).Aligned(); lines != nil {
		t.Errorf("an empty table rendered %d lines", len(lines))
	}
	if got := (render.Table{}).Width(); got != 0 {
		t.Errorf("an empty table is %d wide", got)
	}
}

// message wraps text the way a dispatch carries it, and renders it.
//
// The whole path rather than the table renderer alone: what has to be true is
// that a table in an answer never reaches a reader as bars, and that is a
// property of the path, not of one function in it.
func rendered(t *testing.T, text string, style render.Style) string {
	t.Helper()

	payload, err := json.Marshal(jcgateway.MessagePayload{Text: text})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	out, err := render.Dispatch(jcgateway.Dispatch{
		Kind:    jcgateway.DispatchMessage,
		Payload: string(payload),
	}, style)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

// discordish is a platform that can show a monospaced block.
var discordish = render.Style{
	MaxLength: 2000, SoftLength: 1900,
	Bold: "**", Fence: "```", TableColumns: 76,
}

// plainish is one that cannot: a fence would show its own backticks.
var plainish = render.Style{MaxLength: 4096, SoftLength: 3900}

func TestANarrowTableBecomesAlignedText(t *testing.T) {
	source := "| 角色 | 譯名 |\n|---|---|\n| 白子 | Shiroko |\n"

	got := rendered(t, source, discordish)
	if !strings.Contains(got, "```") {
		t.Errorf("a narrow table did not become a block:\n%s", got)
	}
	if strings.Contains(got, "|---|") {
		t.Errorf("the markdown delimiter row survived:\n%s", got)
	}
}

// The failure this whole file exists for: a table posted as-is is a wall of
// bars, because no platform here renders table syntax.
func TestNoRawTableSyntaxSurvives(t *testing.T) {
	source := "| 舞台 | 台灣譯名 | 匪區誤用 |\n|---|---|---|\n" +
		"| 故事舞台 (Kivotos) | 奇普托斯 | 基沃托斯 |\n"

	for name, style := range map[string]render.Style{
		"with a fence":    discordish,
		"without a fence": plainish,
	} {
		got := rendered(t, source, style)
		if strings.Contains(got, "|---") || strings.Contains(got, "---|") {
			t.Errorf("%s: a delimiter row reached the reader:\n%s", name, got)
		}
	}
}

// Too wide to line up, so it stops being a grid. A table that wraps in the
// window is worse than the rows it was made from.
func TestAWideTableBecomesRows(t *testing.T) {
	wide := strings.Repeat("x", 60)
	source := "| one | two |\n|---|---|\n| " + wide + " | " + wide + " |\n"

	got := rendered(t, source, discordish)
	if strings.Contains(got, "```") {
		t.Errorf("a table far past the column budget was still made a block:\n%s", got)
	}
	if !strings.Contains(got, "**one**") {
		t.Errorf("the header did not become a label:\n%s", got)
	}
}

// A platform with no fence has nowhere to put a monospaced block.
func TestWithoutAFenceEveryTableBecomesRows(t *testing.T) {
	source := "| a | b |\n|---|---|\n| 1 | 2 |\n"

	got := rendered(t, source, plainish)
	if strings.Contains(got, "```") {
		t.Errorf("a fence was used on a platform that has none:\n%s", got)
	}
	if !strings.Contains(got, "1") || !strings.Contains(got, "2") {
		t.Errorf("the values were lost:\n%s", got)
	}
}

// The prose around a table is the model's answer. Only the one construction
// no platform can display is replaced.
func TestTheTextAroundATableIsUntouched(t *testing.T) {
	source := "Here is what I found.\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nThat is all.\n"

	got := rendered(t, source, discordish)
	if !strings.HasPrefix(got, "Here is what I found.") {
		t.Errorf("the text before the table changed:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "That is all.") {
		t.Errorf("the text after the table changed:\n%s", got)
	}
}

func TestAnAnswerWithNoTableIsUnchanged(t *testing.T) {
	source := "Nothing here is a table, though it has a | bar in it."

	if got := rendered(t, source, discordish); got != source {
		t.Errorf("an answer without a table was rewritten:\ngot  %q\nwant %q", got, source)
	}
}
