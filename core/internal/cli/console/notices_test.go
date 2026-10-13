package console

import (
	"context"
	"strings"
	"testing"
	"time"
)

// What the program that started the console has to say is drawn by the console.
//
// The console owns the terminal: a line printed beside it lands in the middle of
// what is being typed. So a supervisor that learns the chat gateway has been
// switched off hands the line over, and it is drawn above the input line like
// anything else the log says.
func TestANoticeFromWhateverStartedTheConsoleIsDrawn(t *testing.T) {
	s, out := aConsole()

	// Closed after the one line, so the wait ends once it has been drawn and
	// the output can be read without racing the screen that writes it.
	notices := make(chan string, 1)
	notices <- "chat is off: no credential"
	close(notices)

	finished := make(chan struct{})
	go func() {
		s.announce(context.Background(), notices)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("it went on waiting on a channel that was closed")
	}

	if !strings.Contains(out.String(), "chat is off: no credential") {
		t.Errorf("the notice was never drawn: %q", out.String())
	}
}

// Closing the console ends the wait, with nothing more said.
func TestTheWaitForNoticesEndsWithTheConsole(t *testing.T) {
	s, _ := aConsole()

	ctx, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		s.announce(ctx, make(chan string))
		close(finished)
	}()

	stop()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("it kept waiting for notices after the console was closed")
	}
}
