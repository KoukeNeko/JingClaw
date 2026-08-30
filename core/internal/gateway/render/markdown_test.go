package render_test

import (
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/gateway/render"
)

// The reason this uses a parser rather than splitting on bars. A cell may
// carry an escaped bar, or a code span holding one, and splitting turns that
// row into more cells than it has — silently, so the table comes out wrong
// instead of failing.
func TestABarInsideACellIsNotASeparator(t *testing.T) {
	source := "| Pattern | Meaning |\n|---|---|\n| `foo\\|bar` | alternation |\n"

	found := render.Tables(source)
	if len(found) != 1 {
		t.Fatalf("%d tables found, want 1", len(found))
	}

	row := found[0].Table.Rows[0]
	if len(row) != 2 {
		t.Fatalf("the row has %d cells, want 2: %q", len(row), row)
	}
	if !strings.Contains(row[0], "|") {
		t.Errorf("the bar inside the cell was lost: %q", row[0])
	}
}

func TestAHeaderAndItsRowsAreRead(t *testing.T) {
	source := "text before\n\n| 角色 | 譯名 |\n|---|---|\n| 白子 | Shiroko |\n| 陽奈 | Hina |\n\ntext after\n"

	found := render.Tables(source)
	if len(found) != 1 {
		t.Fatalf("%d tables found, want 1", len(found))
	}

	table := found[0].Table
	if len(table.Header) != 2 || table.Header[0] != "角色" {
		t.Errorf("header is %q", table.Header)
	}
	if len(table.Rows) != 2 || table.Rows[1][1] != "Hina" {
		t.Errorf("rows are %q", table.Rows)
	}
}

// The span has to cover the whole table, so a caller can replace it and leave
// the prose around it exactly as the model wrote it.
func TestTheSpanCoversTheWholeTableAndNothingElse(t *testing.T) {
	before := "before\n\n"
	table := "| a | b |\n|---|---|\n| 1 | 2 |"
	after := "\n\nafter"
	source := before + table + after

	found := render.Tables(source)
	if len(found) != 1 {
		t.Fatalf("%d tables found, want 1", len(found))
	}

	got := source[found[0].Start:found[0].End]
	if got != table {
		t.Errorf("the span is %q, want %q", got, table)
	}

	replaced := source[:found[0].Start] + "REPLACED" + source[found[0].End:]
	if replaced != before+"REPLACED"+after {
		t.Errorf("replacing the span disturbed the text around it:\n%q", replaced)
	}
}

// Two tables in one answer is ordinary. Each has to be found separately, or
// replacing one corrupts the other.
func TestEachTableIsFoundSeparately(t *testing.T) {
	source := "| a |\n|---|\n| 1 |\n\nbetween\n\n| b |\n|---|\n| 2 |\n"

	found := render.Tables(source)
	if len(found) != 2 {
		t.Fatalf("%d tables found, want 2", len(found))
	}
	if found[0].End > found[1].Start {
		t.Errorf("the spans overlap: %d..%d and %d..%d",
			found[0].Start, found[0].End, found[1].Start, found[1].End)
	}
}

// An answer with no table must come back untouched, and cheaply: most answers
// have none.
func TestTextWithoutATableIsLeftAlone(t *testing.T) {
	for _, source := range []string{
		"just a sentence",
		"a | b, but not a table",
		"```\n| looks | like |\n|---|---|\n```",
		"",
	} {
		if found := render.Tables(source); len(found) != 0 {
			t.Errorf("%q produced %d tables", source, len(found))
		}
	}
}

// Bold in a cell is punctuation once the table is fixed-width: a column
// somebody reads down should not have asterisks in it.
func TestInlineMarkupInsideACellIsDropped(t *testing.T) {
	source := "| a | b |\n|---|---|\n| **bold** | _italic_ |\n"

	found := render.Tables(source)
	if len(found) != 1 {
		t.Fatalf("%d tables found, want 1", len(found))
	}

	row := found[0].Table.Rows[0]
	if row[0] != "bold" || row[1] != "italic" {
		t.Errorf("markup survived into the cells: %q", row)
	}
}
