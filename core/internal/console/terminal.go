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

	mu      sync.Mutex
	editing []rune
	at      int
	drawn   bool
}

// NewScreen writes to out, which is expected to be a terminal.
func NewScreen(out io.Writer) *Screen {
	return &Screen{out: out}
}

// Log writes a line above whatever is being typed.
//
// Safe to call from any goroutine, including while somebody is typing.
func (s *Screen) Log(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.erase()
	_, _ = fmt.Fprintln(s.out, line)
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
		_, _ = fmt.Fprintln(s.out, prompt+line)
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
func (s *Screen) erase() {
	if !s.drawn {
		return
	}
	_, _ = fmt.Fprint(s.out, "\r"+ansi.EraseEntireLine)
	s.drawn = false
}

// draw writes the input line and leaves the cursor where it belongs.
func (s *Screen) draw() {
	_, _ = fmt.Fprint(s.out, prompt+string(s.editing))

	// Back to where the cursor actually is, when it is not at the end. Counted
	// in what the terminal draws rather than in runes, since a Chinese
	// character is two columns wide and an emoji can be several runes and two
	// columns.
	if behind := ansi.GraphemeWidth.StringWidth(string(s.editing[s.at:])); behind > 0 {
		_, _ = fmt.Fprint(s.out, ansi.CursorLeft(behind))
	}
	s.drawn = true
}

// redraw is erase and draw, for when the line itself changed.
func (s *Screen) redraw() {
	s.erase()
	s.draw()
}
