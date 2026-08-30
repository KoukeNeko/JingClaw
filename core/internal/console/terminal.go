package console

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

// prompt is what sits in front of whatever is being typed.
const prompt = "> "

// Screen owns the bottom line of a terminal.
//
// Two things want to write at once: the log, which arrives whenever the agent
// does something, and the person typing, whose half-finished line has to stay
// where it is. Left to share a stream they interleave, and what you get is
//
//	> approv[16:23:04  #a1b2  TOOL ✓  read_file]
//	e abc
//
// which is a line nobody can finish and an entry nobody can read.
//
// So one of them owns the cursor. A line of log erases the input line, writes
// itself, and puts the input line back with the cursor where it was. Nothing
// about what is being typed is saved and restored — it never moved; only its
// drawing did.
//
// This is what JLine does for a Minecraft server, and it is three escape
// sequences rather than a framework.
type Screen struct {
	out io.Writer

	// columns is how wide the terminal is, for working out how many rows the
	// input line occupies. Zero means unknown, and then it is assumed to fit
	// on one — which is what it does until somebody types past the edge.
	columns func() int

	mu      sync.Mutex
	editing []rune
	at      int
	drawn   int
}

// NewScreen writes to out, which is expected to be a terminal.
//
// columns reports the terminal's width, and is asked each time rather than
// remembered because a window can be resized while this is running. Nil is
// allowed and means "assume the input fits on one row".
func NewScreen(out io.Writer, columns func() int) *Screen {
	return &Screen{out: out, columns: columns}
}

// Log writes a line above whatever is being typed.
//
// Safe to call from any goroutine, including while somebody is typing.
func (s *Screen) Log(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.erase()
	_, _ = fmt.Fprint(s.out, line)
	s.newline()
	s.draw()
}

// Prompt shows the input line, if it is not already showing.
func (s *Screen) Prompt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draw()
}

// Insert adds a character where the cursor is.
func (s *Screen) Insert(r rune) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.editing = append(s.editing, 0)
	copy(s.editing[s.at+1:], s.editing[s.at:])
	s.editing[s.at] = r
	s.at++
	s.redraw()
}

// Backspace removes the character before the cursor.
func (s *Screen) Backspace() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.at == 0 {
		return
	}
	s.editing = append(s.editing[:s.at-1], s.editing[s.at:]...)
	s.at--
	s.redraw()
}

// Left and Right move within the line being typed.
func (s *Screen) Left() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at > 0 {
		s.at--
		s.redraw()
	}
}

func (s *Screen) Right() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at < len(s.editing) {
		s.at++
		s.redraw()
	}
}

// Take returns the line and clears it, the way pressing return does.
//
// The line is echoed above first, so the log keeps a record of what was asked
// for next to what it did.
func (s *Screen) Take() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	line := strings.TrimSpace(string(s.editing))
	s.editing = s.editing[:0]
	s.at = 0

	s.erase()
	if line != "" {
		_, _ = fmt.Fprint(s.out, prompt+line)
		s.newline()
	}
	s.draw()

	return line
}

// Set replaces what is being typed, for recalling something from history.
func (s *Screen) Set(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.editing = []rune(line)
	s.at = len(s.editing)
	s.redraw()
}

// Editing is what has been typed so far.
func (s *Screen) Editing() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.editing)
}

// Close leaves the terminal without an input line in it.
func (s *Screen) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.erase()
}

// erase removes the input line from the screen. What is being typed is
// untouched: it lives in s.editing, and only its drawing goes.
//
// Every row of it. A long command wraps, and erasing the current line clears
// the last row of it and leaves the rest on the screen — so the next thing
// written lands underneath half a command that is no longer being typed.
func (s *Screen) erase() {
	if s.drawn == 0 {
		return
	}

	// From the last row back to the first, clearing each.
	for row := s.drawn; row > 1; row-- {
		_, _ = fmt.Fprint(s.out, "\r"+ansi.EraseEntireLine+ansi.CursorUp(1))
	}
	_, _ = fmt.Fprint(s.out, "\r"+ansi.EraseEntireLine)

	s.drawn = 0
}

// draw writes the input line and leaves the cursor where it belongs.
func (s *Screen) draw() {
	// From the left, always. The cursor is wherever the last thing written
	// left it, and in raw mode that is not the start of the line.
	_, _ = fmt.Fprint(s.out, "\r"+prompt+string(s.editing))

	// Back to where the cursor actually is, when it is not at the end. Counted
	// in what the terminal draws rather than in runes, since a Chinese
	// character is two columns wide and an emoji can be several runes and two
	// columns.
	if behind := ansi.GraphemeWidth.StringWidth(string(s.editing[s.at:])); behind > 0 {
		_, _ = fmt.Fprint(s.out, ansi.CursorLeft(behind))
	}

	s.drawn = s.rows()
}

// rows is how many lines of the terminal the input line takes up.
func (s *Screen) rows() int {
	width := 0
	if s.columns != nil {
		width = s.columns()
	}
	if width <= 0 {
		return 1
	}

	drawn := ansi.GraphemeWidth.StringWidth(prompt + string(s.editing))
	if drawn == 0 {
		return 1
	}
	return (drawn + width - 1) / width
}

// newline ends a line and returns to the left.
//
// Written out rather than left to Fprintln, because this runs in raw mode:
// the terminal's habit of turning a line feed into a carriage return and a
// line feed is one of the things raw mode turns off, so "\n" alone moves down
// a row and stays in whatever column it was in. Every line after the first
// then starts wherever the previous one ended.
func (s *Screen) newline() { _, _ = fmt.Fprint(s.out, "\r\n") }

// redraw is erase and draw, for when the line itself changed.
func (s *Screen) redraw() {
	s.erase()
	s.draw()
}
