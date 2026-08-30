package render

import "testing"

// What the model actually produced, copied from a channel.
const modelDrewIt = "以下為江總督整理：\n\n```text\n" +
	"+---------------+------------------------+\n" +
	"| 時間節點      | ARR（年經常性營收）    |\n" +
	"+---------------+------------------------+\n" +
	"| 2025 年 7 月  | 30 萬美元（~960萬）    |\n" +
	"| 2025 年 11 月 | 50 萬美元（~1600萬）   |\n" +
	"+---------------+------------------------+\n" +
	"```\n\n所以還在早期。\n"

func TestATableTheModelDrewIsFound(t *testing.T) {
	found := DrawnTables(modelDrewIt)

	if len(found) != 1 {
		t.Fatalf("found %d drawn tables, want 1", len(found))
	}

	table := found[0].Table
	if len(table.Header) != 2 || table.Header[0] != "時間節點" {
		t.Errorf("the header is %v", table.Header)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("the table has %d rows, want 2", len(table.Rows))
	}
	if table.Rows[1][0] != "2025 年 11 月" {
		t.Errorf("the last row starts with %q", table.Rows[1][0])
	}
}

// Cut out the same way a Markdown table is, so what surrounds it survives.
func TestWhatSurroundsItIsUntouched(t *testing.T) {
	found := DrawnTables(modelDrewIt)
	at := found[0]

	before := modelDrewIt[:at.Start]
	after := modelDrewIt[at.End:]

	if want := "以下為江總督整理：\n\n"; before != want {
		t.Errorf("what comes before is %q, want %q", before, want)
	}
	if want := "\n\n所以還在早期。\n"; after != want {
		t.Errorf("what comes after is %q, want %q", after, want)
	}
}

// The reason the fence is otherwise inviolable. Every one of these is
// something somebody believes is verbatim, and every one of them must come
// out of here untouched.
func TestProgramOutputIsLeftAlone(t *testing.T) {
	for name, text := range map[string]string{
		"a mysql footer": "```\n" +
			"+----+-------+\n| id | name  |\n+----+-------+\n|  1 | ada   |\n+----+-------+\n" +
			"2 rows in set (0.00 sec)\n```\n",

		"a prompt above it": "```\nmysql> select * from people;\n" +
			"+----+-------+\n| id | name  |\n+----+-------+\n|  1 | ada   |\n+----+-------+\n```\n",

		"a diff": "```\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n```\n",

		"ragged rows": "```\n| a | b |\n| c |\n| d | e |\n```\n",

		"one column": "```\n| a |\n| b |\n| c |\n```\n",

		"just a rule": "```\n+---+---+\n+---+---+\n```\n",

		"nothing fenced": "| a | b |\n| c | d |\n",
	} {
		if found := DrawnTables(text); len(found) != 0 {
			t.Errorf("%s was taken for a drawn table: %+v", name, found[0].Table)
		}
	}
}

// Two of them in one answer, each cut out separately.
func TestTwoDrawnTablesAreBothFound(t *testing.T) {
	twice := "一：\n\n```text\n| a | b |\n| 1 | 2 |\n```\n\n二：\n\n```text\n| c | d |\n| 3 | 4 |\n```\n"

	found := DrawnTables(twice)
	if len(found) != 2 {
		t.Fatalf("found %d, want 2", len(found))
	}
	if found[0].Table.Header[0] != "a" || found[1].Table.Header[0] != "c" {
		t.Errorf("found %v and %v", found[0].Table.Header, found[1].Table.Header)
	}
	if found[0].End > found[1].Start {
		t.Error("the two overlap")
	}
}

// A fence with a language on it is still a fence. The model puts "text" on
// the ones it draws, and that is not evidence either way.
func TestTheLanguageTagDoesNotMatter(t *testing.T) {
	for _, fence := range []string{"```", "```text", "```markdown"} {
		drawn := fence + "\n| a | b |\n| 1 | 2 |\n```\n"
		if found := DrawnTables(drawn); len(found) != 1 {
			t.Errorf("%q gave %d tables, want 1", fence, len(found))
		}
	}
}
