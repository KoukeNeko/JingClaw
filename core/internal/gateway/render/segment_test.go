package render

import "testing"

const oneTable = "看一下這個：\n\n" +
	"| 項目 | 數據 |\n|---|---|\n| 融資 | 200 萬 |\n| 用戶 | 10 萬人 |\n\n" +
	"所以他們還在早期。"

// The order is the answer's order. A table that arrived after the sentence
// explaining it would read as being about something else.
func TestATableInTheMiddleBecomesThreePieces(t *testing.T) {
	segments := Segments(oneTable)

	if len(segments) != 3 {
		t.Fatalf("got %d pieces, want 3: %+v", len(segments), segments)
	}

	if segments[0].Kind != SegmentText || segments[0].Text != "看一下這個：" {
		t.Errorf("the first piece is %+v", segments[0])
	}
	if segments[1].Kind != SegmentTable {
		t.Errorf("the second piece is not the table: %+v", segments[1])
	}
	if segments[2].Kind != SegmentText || segments[2].Text != "所以他們還在早期。" {
		t.Errorf("the third piece is %+v", segments[2])
	}
}

func TestTheTableKeepsItsRowsAndHeader(t *testing.T) {
	segments := Segments(oneTable)
	table := segments[1].Table

	if len(table.Header) != 2 || table.Header[0] != "項目" {
		t.Errorf("the header is %v", table.Header)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("the table has %d rows, want 2", len(table.Rows))
	}
	if table.Rows[1][1] != "10 萬人" {
		t.Errorf("the last cell is %q", table.Rows[1][1])
	}
}

// Text with no table is one piece, which is what every caller that does not
// draw pictures gets — and is why they need no special case.
func TestTextWithNoTableIsOnePiece(t *testing.T) {
	segments := Segments("just an answer")

	if len(segments) != 1 {
		t.Fatalf("got %d pieces, want 1", len(segments))
	}
	if segments[0].Kind != SegmentText || segments[0].Text != "just an answer" {
		t.Errorf("the piece is %+v", segments[0])
	}
}

// A message with nothing in it is one platforms refuse, and one a reader
// would not want if they did not.
func TestNothingBetweenTwoTablesIsNotAPiece(t *testing.T) {
	twice := "| a |\n|---|\n| 1 |\n\n| b |\n|---|\n| 2 |\n"

	segments := Segments(twice)
	for index, segment := range segments {
		if segment.Kind == SegmentText && segment.Text == "" {
			t.Errorf("piece %d is an empty message", index)
		}
	}

	tables := 0
	for _, segment := range segments {
		if segment.Kind == SegmentTable {
			tables++
		}
	}
	if tables != 2 {
		t.Errorf("found %d tables, want 2", tables)
	}
}

// A table at the very start or the very end has nothing before or after it.
func TestATableAloneIsOnePiece(t *testing.T) {
	segments := Segments("| a |\n|---|\n| 1 |\n")

	if len(segments) != 1 {
		t.Fatalf("got %d pieces, want 1: %+v", len(segments), segments)
	}
	if segments[0].Kind != SegmentTable {
		t.Errorf("the piece is not the table: %+v", segments[0])
	}
}
