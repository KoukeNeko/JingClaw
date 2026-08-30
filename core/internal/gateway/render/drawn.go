package render

import "strings"

// DrawnTables finds tables the model drew itself inside code fences.
//
// The fence is otherwise inviolable, and for a good reason: mistaking a
// program's output for a drawn table means taking bytes somebody believes are
// verbatim and showing something else. That reason has not gone away, and
// this is only reached when a deployment has asked for tables to become
// pictures — which is to say, has said what it wants done with them.
//
// What makes it defensible is the evidence required. Not "these lines look
// like a table": the fence must hold nothing else at all, every row must
// parse to the same number of cells, and there must be more than one of them.
// A log line, a prompt, a "6 rows in set" footer — any of those and this
// returns nothing and the fence is left exactly as it was.
func DrawnTables(text string) []TableAt {
	lines := strings.Split(text, "\n")

	var (
		found  []TableAt
		inside bool
		start  int
		body   []string
		other  bool
		offset int
	)

	// Offsets are into the original string, so a caller can cut the fence out
	// the same way it cuts a Markdown one.
	starts := make([]int, len(lines))
	at := 0
	for index, line := range lines {
		starts[index] = at
		at += len(line) + 1
	}

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if inside {
				if !other {
					if table, ok := parseDrawn(body); ok {
						end := starts[index] + len(line)
						if end > len(text) {
							end = len(text)
						}
						found = append(found, TableAt{Table: table, Start: start, End: end})
					}
				}
				inside, body, other = false, nil, false
				continue
			}
			inside, start, body, other = true, starts[index], nil, false
			offset = index
			_ = offset
			continue
		}

		if !inside {
			continue
		}
		if trimmed == "" {
			continue
		}
		if !drawnRow(trimmed) {
			other = true
			continue
		}
		body = append(body, trimmed)
	}

	return found
}

// drawnRow reports whether a line is part of a drawn table: a row of cells
// between pipes, or a horizontal border.
func drawnRow(line string) bool {
	if strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") {
		return true
	}
	if strings.HasPrefix(line, "+") && strings.HasSuffix(line, "+") {
		return strings.Trim(line, "+-=") == ""
	}
	return false
}

// parseDrawn turns the lines of a drawn table back into cells.
//
// Ragged is a refusal. A table whose rows do not all have the same number of
// cells is not something this understood, and the honest response to not
// understanding something is to leave it alone.
func parseDrawn(lines []string) (Table, bool) {
	var rows [][]string

	for _, line := range lines {
		if strings.HasPrefix(line, "+") {
			continue
		}

		// Split on the pipes, dropping the empty pieces the leading and
		// trailing ones produce.
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for index := range cells {
			cells[index] = strings.TrimSpace(cells[index])
		}
		rows = append(rows, cells)
	}

	if len(rows) < 2 {
		return Table{}, false
	}

	width := len(rows[0])
	if width < 2 {
		// One column is a list somebody boxed, and a list reads better as a
		// list than as a picture of one.
		return Table{}, false
	}
	for _, row := range rows {
		if len(row) != width {
			return Table{}, false
		}
	}

	// Every cell empty is a border somebody drew with pipes rather than
	// pluses, not a table with content in it.
	content := false
	for _, row := range rows {
		for _, cell := range row {
			if cell != "" {
				content = true
			}
		}
	}
	if !content {
		return Table{}, false
	}

	return Table{Header: rows[0], Rows: rows[1:]}, true
}
