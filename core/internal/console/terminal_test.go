package console

import (
	"strings"
	"sync"
	"testing"
)

func typing(screen *Screen, text string) {
	for _, r := range text {
		screen.Insert(r)
	}
}

// The whole reason this exists: a line of log arriving while somebody is
// half-way through a command must not take the command with it.
func TestALogLineDoesNotDisturbWhatIsBeingTyped(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "approve abc")
	screen.Log("16:23:04  #a1b2  TOOL ✓  read_file")

	if editing := screen.Editing(); editing != "approve abc" {
		t.Errorf("what was being typed became %q", editing)
	}

	// And the log line is on a line of its own rather than inside the input.
	written := out.String()
	if !strings.Contains(written, "\nTOOL") && !strings.Contains(written, "TOOL") {
		t.Fatalf("the log line is missing:\n%q", written)
	}
	if strings.Contains(written, "approve abcTOOL") || strings.Contains(written, "approve abc16:23") {
		t.Errorf("the log was written into the middle of the input:\n%q", written)
	}
}

// After a log line the input line has to be back on the screen, or the person
// typing is left looking at a blank bottom row.
func TestTheInputLineComesBackAfterALogLine(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "deny xyz")
	screen.Log("something happened")

	written := out.String()
	tail := written[strings.LastIndex(written, "\n")+1:]
	if !strings.Contains(tail, prompt+"deny xyz") {
		t.Errorf("the input line was not redrawn after the log; the last line is %q", tail)
	}
}

func TestTakeReturnsTheLineAndClearsIt(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "  pending  ")

	if line := screen.Take(); line != "pending" {
		t.Errorf("took %q, want pending", line)
	}
	if editing := screen.Editing(); editing != "" {
		t.Errorf("the line was not cleared; it holds %q", editing)
	}
}

// A command that was run stays in the log next to what it did, so the record
// reads as a conversation rather than as effects with no causes.
func TestWhatWasEnteredIsEchoedAbove(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "approve abc")
	screen.Take()

	if !strings.Contains(out.String(), prompt+"approve abc\r\n") {
		t.Errorf("the command was not echoed:\n%q", out.String())
	}
}

func TestBackspaceRemovesTheCharacterBefore(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "denyy")
	screen.Backspace()

	if editing := screen.Editing(); editing != "deny" {
		t.Errorf("after a backspace the line is %q, want deny", editing)
	}
}

func TestBackspaceAtTheStartDoesNothing(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	screen.Backspace()

	if editing := screen.Editing(); editing != "" {
		t.Errorf("backspacing an empty line produced %q", editing)
	}
}

// Typing in the middle inserts there rather than at the end.
func TestInsertingHappensAtTheCursor(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "aprove")
	for range 5 {
		screen.Left()
	}
	screen.Insert('p')

	if editing := screen.Editing(); editing != "approve" {
		t.Errorf("the line is %q, want approve", editing)
	}
}

func TestTheCursorStopsAtBothEnds(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	typing(screen, "ab")
	for range 10 {
		screen.Left()
	}
	screen.Insert('x')
	if editing := screen.Editing(); editing != "xab" {
		t.Errorf("moving left past the start gave %q, want xab", editing)
	}

	for range 10 {
		screen.Right()
	}
	screen.Insert('y')
	if editing := screen.Editing(); editing != "xaby" {
		t.Errorf("moving right past the end gave %q, want xaby", editing)
	}
}

// The log arrives from the stream while the terminal is being read from, so
// the two really do run at once.
func TestLoggingWhileTypingIsSafe(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)
	screen.Prompt()

	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range 200 {
			screen.Log("an event")
		}
	}()
	go func() {
		defer wait.Done()
		for range 200 {
			screen.Insert('x')
		}
	}()
	wait.Wait()

	if editing := screen.Editing(); len(editing) != 200 {
		t.Errorf("typed 200 characters and the line holds %d", len(editing))
	}
}

// In raw mode the terminal does not turn a line feed into a carriage return
// and a line feed. A bare "\n" moves down a row and stays in the column it
// was in, so every line after the first starts wherever the last one ended —
// which is how the prompt ends up in the middle of the screen.
func TestEveryLineEndsWithACarriageReturn(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	screen.Log("first")
	screen.Log("second")

	written := out.String()
	for at, r := range written {
		if r != '\n' {
			continue
		}
		if at == 0 || written[at-1] != '\r' {
			t.Fatalf("a line feed at %d has no carriage return before it: %q", at, written)
		}
	}
	if !strings.Contains(written, "\r\n") {
		t.Errorf("nothing ended a line at all: %q", written)
	}
}

// The prompt starts at the left however the last thing written left the
// cursor.
func TestTheInputLineStartsAtTheLeft(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, nil)

	screen.Prompt()
	screen.Log("a log line")

	written := out.String()
	last := strings.LastIndex(written, prompt)
	if last <= 0 {
		t.Fatalf("no prompt was drawn: %q", written)
	}
	if written[last-1] != '\r' {
		t.Errorf("the prompt was drawn without returning to the left first: %q", written[last-2:])
	}
}

// A command long enough to wrap takes more than one row, and erasing it has
// to clear every one of them: clearing the last leaves the rest on screen,
// and the next thing written lands under half a command nobody is typing.
func TestAWrappedInputLineIsErasedInFull(t *testing.T) {
	var out strings.Builder
	// Twenty columns, so a short command already wraps.
	screen := NewScreen(&out, func() int { return 20 })

	screen.Prompt()
	typing(screen, strings.Repeat("x", 45))

	out.Reset()
	screen.Log("something happened")

	// Three rows of "> " plus 45 characters at twenty columns, so two moves
	// up and three erases.
	written := out.String()
	if ups := strings.Count(written, "\x1b[A"); ups != 2 {
		t.Errorf("moved up %d times to erase three rows: %q", ups, written)
	}
	if erases := strings.Count(written, "\x1b[2K"); erases < 3 {
		t.Errorf("erased %d rows of a three-row input line: %q", erases, written)
	}
}

// One row needs no moving about, which is every command short enough to fit.
func TestAShortInputLineIsErasedWithoutMovingUp(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out, func() int { return 80 })

	screen.Prompt()
	typing(screen, "approve abc")

	out.Reset()
	screen.Log("something happened")

	if strings.Contains(out.String(), "\x1b[A") {
		t.Errorf("moved up to erase a line that fits on one row: %q", out.String())
	}
}
