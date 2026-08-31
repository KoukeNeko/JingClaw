// Package tui is the panel: what is happening, and the decisions waiting.
//
// It reads and decides, and it does not talk. There is no composer, and that
// is the shape of the system rather than a preference — a conversation
// arrives from a platform carrying an identity that platform authenticated
// and somewhere to answer, and a turn typed here would be a run with no
// origin. The CLI keeps "send" for when somebody means it.
//
// What it is for: seeing a session, allowing or refusing what is waiting,
// answering a question a run stopped to ask, and interrupting. Those are the
// things that otherwise mean opening a chat client to unblock a machine that
// is two feet away.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
)

// Options is what the panel needs to run.
type Options struct {
	// Sessions is where what it draws comes from. Nil draws an empty list,
	// which is what the lifecycle checks use: they are about the terminal
	// rather than about any session.
	Sessions Sessions

	// Output is where the screen is drawn. Empty is the real terminal.
	Output io.Writer

	// Input is where keys come from. Empty is the real terminal.
	Input io.Reader

	// PanicForTest makes the panel fail once it is drawing.
	//
	// Here rather than in a test file because what it checks cannot be
	// reached any other way: the question is what a crash leaves behind on a
	// real terminal, and a panic staged from outside would be a panic in the
	// test rather than in the drawing.
	PanicForTest bool
}

// Run shows the panel until it is told to stop.
//
// The terminal is given back on every path out of here, including the ones
// nobody planned. That is the one thing a long-running panel must not get
// wrong: a program that leaves the screen in the alternate buffer with the
// cursor hidden is one somebody closes the window on, and it happens exactly
// when something else has already gone wrong.
//
// The framework does this already, on every way out that has been tried:
// stopping, interrupting, suspending, and panicking. That was measured, by
// removing the restoration below and watching all four checks still pass.
//
// It stays anyway, and the reason is the asymmetry rather than distrust. What
// it costs is three escape sequences nobody sees. What it covers is a version
// of the framework that stops doing it, or a way out nobody has tried yet,
// and the cost of that is a window somebody has to close — arriving exactly
// when something has already gone wrong.
//
// So it is insurance that has never paid out, said plainly because a comment
// claiming this file is what restores the terminal would be false. The tests
// beside it verify the framework; nothing verifies this, which is the honest
// state of a fallback for a case that has not happened.
func Run(ctx context.Context, opts Options) (err error) {
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}

	starting := newPanel(ctx, opts.Sessions)
	starting.panicNow = opts.PanicForTest

	program := tea.NewProgram(starting, programOptions(ctx, opts)...)

	// Last, and unconditional. If the framework already put the terminal
	// back, saying so again costs three escape sequences nobody sees; if it
	// did not, this is the difference between a shell and a window somebody
	// has to close.
	defer func() {
		if recovered := recover(); recovered != nil {
			giveTheTerminalBack(output)
			err = fmt.Errorf("tui: the panel failed: %v", recovered)
			return
		}
		giveTheTerminalBack(output)
	}()

	if _, err := program.Run(); err != nil {
		// Being told to stop is how this ends, not a failure to report.
		if errors.Is(err, context.Canceled) || errors.Is(err, tea.ErrProgramKilled) {
			return nil
		}
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// programOptions is how the panel is wired to the terminal.
func programOptions(ctx context.Context, opts Options) []tea.ProgramOption {
	options := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.Output != nil {
		options = append(options, tea.WithOutput(opts.Output))
	}
	if opts.Input != nil {
		options = append(options, tea.WithInput(opts.Input))
	}
	return options
}

// giveTheTerminalBack undoes what taking the screen did.
//
// Written out rather than described, because these are the three states a
// terminal is left in that a person notices: text going to a screen they
// cannot scroll, a cursor they cannot see, and a shell that mangles anything
// they paste.
func giveTheTerminalBack(output io.Writer) {
	const (
		leaveAltScreen = "\x1b[?1049l"
		showCursor     = "\x1b[?25h"
		pasteOff       = "\x1b[?2004l"
	)
	_, _ = io.WriteString(output, leaveAltScreen+showCursor+pasteOff)
}
