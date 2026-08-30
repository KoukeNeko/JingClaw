package render

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// markdown parses only what this package needs to find: tables.
//
// A parser rather than string splitting, because a table cell may contain an
// escaped bar or a code span holding one:
//
//	| `foo\|bar` | alternation |
//
// Splitting on "|" turns that row into four cells, and the table silently
// comes out wrong rather than failing.
var markdown = goldmark.New(goldmark.WithExtensions(extension.Table))

// TableAt is one table found in a piece of text, and where it was.
type TableAt struct {
	Table Table

	// Start and End bound the table in the original text, so a caller can
	// replace it and leave everything around it exactly as the model wrote it.
	Start, End int
}

// Tables finds the Markdown tables in text.
//
// Everything else is left alone. This package renders an answer for a chat
// platform, not a document: reflowing prose the model wrote would change what
// it said, and the only thing being changed here is the one construction no
// platform in use can display.
func Tables(source string) []TableAt {
	raw := []byte(source)
	document := markdown.Parser().Parse(text.NewReader(raw))

	var found []TableAt
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		table, ok := node.(*extast.Table)
		if !ok {
			continue
		}

		start, end, ok := span(table, raw)
		if !ok {
			continue
		}

		found = append(found, TableAt{
			Table: readTable(table, raw),
			Start: start,
			End:   end,
		})
	}
	return found
}

// span is where a table begins and ends in the source.
//
// Taken from its cells. Neither the table nor its rows carry lines of their
// own — only the cells do — so the extent is the widest reach of any of them,
// then grown outwards to whole lines.
func span(table *extast.Table, source []byte) (int, int, bool) {
	start, end := -1, -1

	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			lines := cell.Lines()
			if lines.Len() == 0 {
				continue
			}
			if first := lines.At(0); start < 0 || first.Start < start {
				start = first.Start
			}
			if last := lines.At(lines.Len() - 1); last.Stop > end {
				end = last.Stop
			}
		}
	}
	if start < 0 || end < 0 {
		return 0, 0, false
	}

	// A cell's line begins after the leading "|", so walk back to the start of
	// the line: the whole table has to be replaced, not the part after its
	// first bar. The delimiter row between the header and the body carries no
	// cells at all, so walking outwards is also what includes it.
	for start > 0 && source[start-1] != '\n' {
		start--
	}
	for end < len(source) && source[end] != '\n' {
		end++
	}
	return start, end, true
}

func readTable(table *extast.Table, source []byte) Table {
	var built Table

	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		cells := readRow(row, source)
		if _, isHeader := row.(*extast.TableHeader); isHeader {
			built.Header = cells
			continue
		}
		built.Rows = append(built.Rows, cells)
	}
	return built
}

func readRow(row ast.Node, source []byte) []string {
	var cells []string
	for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
		cells = append(cells, cellText(cell, source))
	}
	return cells
}

// cellText is a cell's visible text.
//
// The inline markup inside a cell is dropped rather than rendered: a fixed
// width block cannot show bold, and leaving the asterisks in would put
// punctuation in a column somebody is trying to read down.
func cellText(cell ast.Node, source []byte) string {
	var out bytes.Buffer

	_ = ast.Walk(cell, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.Text:
			out.Write(typed.Segment.Value(source))
			if typed.SoftLineBreak() || typed.HardLineBreak() {
				out.WriteByte(' ')
			}
		case *ast.CodeSpan:
			for child := typed.FirstChild(); child != nil; child = child.NextSibling() {
				if text, ok := child.(*ast.Text); ok {
					out.Write(text.Segment.Value(source))
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})

	return out.String()
}
