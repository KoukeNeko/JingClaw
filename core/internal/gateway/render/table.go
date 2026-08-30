package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Table is a parsed Markdown table, ready to be turned into something a
// platform can show.
//
// Discord renders no table syntax at all, so a model's pipe-delimited table
// arrives as a wall of bars. Telegram is the same. What each platform can show
// differs, so this carries the table rather than a rendering of it.
type Table struct {
	// Header is the first row, repeated on every page of a long table.
	Header []string

	// Rows are the body, in order.
	Rows [][]string
}

// cellWidth is how wide a cell is on screen.
//
// Not len, not the rune count, and not the number of code points: those are
// three different numbers and none of them is this one. "中" is one code point
// two columns wide; "é" may be two code points one column wide; a flag is two
// regional indicators and one glyph.
//
// ANSI sequences are ignored rather than counted, which matters because a
// model that has been reading terminal output will occasionally paste some.
func cellWidth(text string) int {
	return ansi.GraphemeWidth.StringWidth(text)
}

// Width is how wide this table would be as a fixed-width block, including the
// separators between columns.
//
// The caller uses it to decide whether a fixed-width rendering fits at all. A
// table that does not fit is not a narrower table; it is a different shape,
// and pretending otherwise produces something nobody can read.
func (t Table) Width() int {
	widths := t.columnWidths()
	if len(widths) == 0 {
		return 0
	}

	total := 0
	for _, width := range widths {
		total += width
	}
	// "a | b | c": three spaces and a bar between each pair of columns.
	return total + (len(widths)-1)*3
}

// columnWidths is the widest cell in each column, header included.
func (t Table) columnWidths() []int {
	columns := len(t.Header)
	for _, row := range t.Rows {
		if len(row) > columns {
			columns = len(row)
		}
	}
	if columns == 0 {
		return nil
	}

	widths := make([]int, columns)
	for index, cell := range t.Header {
		widths[index] = cellWidth(cell)
	}
	for _, row := range t.Rows {
		for index, cell := range row {
			if width := cellWidth(cell); width > widths[index] {
				widths[index] = width
			}
		}
	}
	return widths
}

// Aligned renders the table as fixed-width text, one line per row.
//
// Padded by display width, so a column of Chinese lines up with a column of
// English. That alignment holds in a terminal, where a cell is a cell. It
// mostly holds in a Discord code block, where the Latin face is monospace —
// but a CJK glyph may come from a fallback font, and standard emoji differ by
// platform. So this is the best a sender can do rather than something the
// receiver guarantees, and a table whose alignment has to be exact belongs in
// an attachment instead.
func (t Table) Aligned() []string {
	widths := t.columnWidths()
	if len(widths) == 0 {
		return nil
	}

	lines := make([]string, 0, len(t.Rows)+2)
	lines = append(lines, alignedRow(t.Header, widths))

	rule := make([]string, len(widths))
	for index, width := range widths {
		rule[index] = strings.Repeat("─", width)
	}
	lines = append(lines, strings.Join(rule, "─┼─"))

	for _, row := range t.Rows {
		lines = append(lines, alignedRow(row, widths))
	}
	return lines
}

func alignedRow(cells []string, widths []int) string {
	padded := make([]string, len(widths))
	for index := range widths {
		cell := ""
		if index < len(cells) {
			cell = cells[index]
		}
		padded[index] = cell + strings.Repeat(" ", widths[index]-cellWidth(cell))
	}
	// The last column is not padded: trailing spaces are invisible and only
	// bring the line closer to a length limit.
	return strings.TrimRight(strings.Join(padded, " │ "), " ")
}

// AsRows renders the table as one paragraph per row.
//
// For a table too wide to line up, and for one whose columns are labels rather
// than a grid — "what this is" against "what it says". A reader of those is
// reading across a row, not down a column, and a row that has wrapped across
// three lines of a chat window is not a row any more.
//
// The header becomes the label on each line, so a row still says what its
// values mean when it is no longer beneath the header.
func (t Table) AsRows(style Style) []string {
	if len(t.Rows) == 0 {
		return nil
	}

	rendered := make([]string, 0, len(t.Rows))
	for _, row := range t.Rows {
		var out strings.Builder
		for index, cell := range row {
			if strings.TrimSpace(cell) == "" {
				continue
			}
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			if index < len(t.Header) && strings.TrimSpace(t.Header[index]) != "" {
				out.WriteString(style.bold(t.Header[index]) + ": ")
			}
			out.WriteString(cell)
		}
		if out.Len() > 0 {
			rendered = append(rendered, out.String())
		}
	}
	return rendered
}

// AsDelimited renders the table as delimited text, for an attachment.
//
// Tab-separated rather than comma-separated: the cells here are prose written
// by a model, commas are everywhere in it, and quoting rules are one more
// thing to get wrong. A tab is not something a model puts inside a table cell.
func (t Table) AsDelimited() string {
	var out strings.Builder

	if len(t.Header) > 0 {
		out.WriteString(strings.Join(t.Header, "\t"))
		out.WriteString("\n")
	}
	for _, row := range t.Rows {
		out.WriteString(strings.Join(row, "\t"))
		out.WriteString("\n")
	}
	return out.String()
}

// tableBudget leaves room for the fences and a little slack, so a rendered
// table does not sit exactly on the limit and force a split.
const tableBudget = 1900

// renderTables replaces every Markdown table in text with something the
// platform can actually show.
//
// Discord renders no table syntax, and neither does Telegram: a model's table
// arrives as a wall of bars. What replaces it depends on how wide it is, which
// is the only question that matters — a narrow table lines up in a monospaced
// block, and a wide one has to stop being a grid.
//
// Everything around the table is left exactly as written. This renders an
// answer for a chat window, not a document.
func renderTables(text string, style Style) string {
	found := Tables(text)
	if len(found) == 0 {
		return text
	}

	var out strings.Builder
	written := 0

	for _, at := range found {
		out.WriteString(text[written:at.Start])
		out.WriteString(renderTable(at.Table, style))
		written = at.End
	}
	out.WriteString(text[written:])

	return out.String()
}

func renderTable(table Table, style Style) string {
	// No fence, or too wide to line up: a grid nobody can read down is worse
	// than the same rows written out.
	if style.Fence == "" || style.TableColumns == 0 || table.Width() > style.TableColumns {
		return strings.Join(table.AsRows(style), "\n\n")
	}

	block := style.Fence + "\n" + strings.Join(table.Aligned(), "\n") + "\n" + style.Fence

	// One that fits the width but not a message is not a table either. Falling
	// back to rows rather than paging it: a table split across messages has a
	// header on the first one and orphaned rows after it.
	if len(block) > tableBudget {
		return strings.Join(table.AsRows(style), "\n\n")
	}
	return block
}

// TableText writes a table out the way a platform without pictures gets it.
//
// Exported for the one caller that draws them and needs a way back: a table
// that would not draw still has to appear, and an answer with a gap where one
// was is worse than an answer with an unaligned one in it.
func TableText(table Table, style Style) string { return renderTable(table, style) }
