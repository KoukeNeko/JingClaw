package gateway

import (
	"regexp"
	"strings"
)

// drawnTable matches a row of a table somebody drew by hand: "| a | b |" or
// "+---+---+".
var drawnTable = regexp.MustCompile(`^\s*(\|.*\||\+[-=+]+\+)\s*$`)

// DrawsATableInAFence reports whether text contains a fenced block that is a
// table the model drew rather than output from a program.
//
// It reports and changes nothing, on purpose. The two ways of being wrong here
// cost wildly different amounts: mistaking a drawn table for program output
// leaves a crooked table, while mistaking program output for a drawn table
// means rewriting bytes somebody believes are verbatim — a test failure, a
// snapshot, a diff, output being compared on whitespace. The first is untidy;
// the second is a lie.
//
// And there is no grammar that separates them. This is a drawn table and a
// MySQL client's output and a unit test's expected value, and nothing about
// the text says which:
//
//	+----+----------+
//	| id | name     |
//	+----+----------+
//
// So the fence stays inviolable and this exists to say how often the model is
// asked not to and does it anyway. A rule nobody measures is a rule nobody
// knows is working.
func DrawsATableInAFence(text string) bool {
	inside, drawn, other := false, 0, 0

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inside && drawn >= 3 && other == 0 {
				return true
			}
			inside, drawn, other = !inside, 0, 0
			continue
		}
		if !inside || strings.TrimSpace(line) == "" {
			continue
		}
		if drawnTable.MatchString(line) {
			drawn++
			continue
		}
		other++
	}
	return false
}
