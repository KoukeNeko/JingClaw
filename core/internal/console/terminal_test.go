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
	screen := NewScreen(&out)

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
	screen := NewScreen(&out)

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
	screen := NewScreen(&out)

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
	screen := NewScreen(&out)

	screen.Prompt()
	typing(screen, "approve abc")
	screen.Take()

	if !strings.Contains(out.String(), prompt+"approve abc\n") {
		t.Errorf("the command was not echoed:\n%q", out.String())
	}
}

func TestBackspaceRemovesTheCharacterBefore(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out)

	screen.Prompt()
	typing(screen, "denyy")
	screen.Backspace()

	if editing := screen.Editing(); editing != "deny" {
		t.Errorf("after a backspace the line is %q, want deny", editing)
	}
}

func TestBackspaceAtTheStartDoesNothing(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out)

	screen.Prompt()
	screen.Backspace()

	if editing := screen.Editing(); editing != "" {
		t.Errorf("backspacing an empty line produced %q", editing)
	}
}

// Typing in the middle inserts there rather than at the end.
func TestInsertingHappensAtTheCursor(t *testing.T) {
	var out strings.Builder
	screen := NewScreen(&out)

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
	screen := NewScreen(&out)

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
	screen := NewScreen(&out)
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
