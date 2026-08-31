package tui_test

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The sequences a terminal has to be put back to.
//
// Checked as bytes on the wire rather than by asking a library whether it
// tidied up. A panel that leaves the terminal in alternate screen with the
// cursor hidden is a terminal somebody has to close the window on, and
// "the framework handles it" is exactly the assumption worth not making.
//
// What these turned out to be checking is the framework: removing the
// restoration in tui.go leaves all four passing, because Bubble Tea already
// does it on each of these paths. That is the useful thing for them to check
// — the framework is the part that could change under this without anybody
// editing this repository.
const (
	enterAltScreen = "\x1b[?1049h"
	leaveAltScreen = "\x1b[?1049l"
	showCursor     = "\x1b[?25h"
	pasteOff       = "\x1b[?2004l"
)

// runUnderPTY starts the panel on a pseudo-terminal and returns everything it
// wrote, after doing whatever ends it.
//
// A real PTY because that is the thing at risk. Piping stdout would test a
// program that knows it is not on a terminal, which is the one case where
// none of this matters.
func runUnderPTY(t *testing.T, end func(*exec.Cmd, *os.File)) string {
	t.Helper()

	// The test binary itself, told to be the panel. The panel needs no daemon
	// to open a screen and close it again, which is the whole of what is
	// being checked here.
	command := exec.Command(os.Args[0], "-test.run=TestIsThePanel")
	command.Env = append(os.Environ(), "JINGCLAW_TUI_SKELETON=1", "TERM=xterm-256color")

	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("could not open a terminal: %v", err)
	}
	defer terminal.Close()

	var written bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = written.ReadFrom(terminal)
	}()

	// Wait for it to have taken the screen, so that ending it is ending
	// something that had something to put back.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(written.String(), enterAltScreen) {
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatalf("it never took the screen; wrote %q", written.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	end(command, terminal)

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("it did not finish")
	}

	<-read
	return written.String()
}

// restored asserts the terminal was put back, in the right order.
func restored(t *testing.T, output string) {
	t.Helper()

	took := strings.Index(output, enterAltScreen)
	if took < 0 {
		t.Fatalf("it never took the screen: %q", output)
	}

	for _, put := range []struct {
		name, sequence string
	}{
		{"the alternate screen was not left", leaveAltScreen},
		{"the cursor was left hidden", showCursor},
		{"bracketed paste was left on", pasteOff},
	} {
		at := strings.LastIndex(output, put.sequence)
		if at < 0 {
			t.Errorf("%s (no %q anywhere)", put.name, put.sequence)
			continue
		}
		if at < took {
			t.Errorf("%s (it came before the screen was taken)", put.name)
		}
	}
}

// TestItGivesTheTerminalBackWhenItIsToldToStop is the ordinary way out.
func TestItGivesTheTerminalBackWhenItIsToldToStop(t *testing.T) {
	restored(t, runUnderPTY(t, func(command *exec.Cmd, _ *os.File) {
		_ = command.Process.Signal(syscall.SIGTERM)
	}))
}

// TestItGivesTheTerminalBackOnInterrupt covers ctrl-c, which is how somebody
// will actually stop it.
func TestItGivesTheTerminalBackOnInterrupt(t *testing.T) {
	restored(t, runUnderPTY(t, func(command *exec.Cmd, _ *os.File) {
		_ = command.Process.Signal(syscall.SIGINT)
	}))
}

// TestItGivesTheTerminalBackAfterBeingSuspended is the one a framework is
// least likely to have thought about.
//
// Stepping out has to give the shell its terminal back, and stepping in has
// to take it again. A panel that only tidies up on the way out leaves a shell
// that echoes nothing.
//
// Sent as a key rather than as a signal, because that is what happens: in raw
// mode the terminal driver does not turn ctrl-z into SIGTSTP, it hands the
// byte to the program. A check that sent the signal would be checking a path
// nobody takes, and would pass or fail for reasons unrelated to pressing the
// key.
func TestItGivesTheTerminalBackAfterBeingSuspended(t *testing.T) {
	output := runUnderPTY(t, func(command *exec.Cmd, terminal *os.File) {
		_, _ = terminal.Write([]byte{0x1a}) // ctrl-z
		time.Sleep(500 * time.Millisecond)
		_ = command.Process.Signal(syscall.SIGCONT)
		time.Sleep(500 * time.Millisecond)
		_ = command.Process.Signal(syscall.SIGTERM)
	})

	restored(t, output)

	// It took the screen back on being resumed, rather than carrying on
	// writing into a screen it had left.
	if strings.Count(output, enterAltScreen) < 2 {
		t.Errorf("it did not take the screen again after being resumed: %q",
			summarise(output))
	}
}

// TestItGivesTheTerminalBackWhenItCrashes is the case that matters most.
//
// A panel that tidies up only on the paths somebody thought of is one that
// leaves a broken terminal exactly when something has already gone wrong.
// SIGKILL and a runtime fatal are out of scope and cannot be caught; a panic
// can be.
func TestItGivesTheTerminalBackWhenItCrashes(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestIsThePanel")
	command.Env = append(os.Environ(),
		"JINGCLAW_TUI_SKELETON=1", "JINGCLAW_TUI_PANIC=1", "TERM=xterm-256color")

	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("could not open a terminal: %v", err)
	}
	defer terminal.Close()

	var written bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = written.ReadFrom(terminal)
	}()

	_ = command.Wait()
	<-read

	// It is expected to have failed. What is being checked is what it left
	// behind.
	restored(t, written.String())
}

// summarise makes escape sequences readable in a failure.
func summarise(output string) string {
	const most = 400
	if len(output) > most {
		output = output[:most] + "…"
	}
	return strings.ReplaceAll(output, "\x1b", "\\e")
}
