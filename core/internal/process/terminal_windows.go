//go:build windows

package process

import (
	"errors"
	"os/exec"

	"github.com/KoukeNeko/JingClaw/core/internal/conpty"
)

// startWithTerminal runs the program on a Windows pseudo console.
//
// The console creates the process itself — os/exec cannot attach one — so the
// command built for the piped path is not started here; its program, arguments,
// directory and environment are what the console is told to run. The running
// process is then placed in the group's job so stopping it takes its tree.
func startWithTerminal(command *exec.Cmd, group *procGroup, columns, rows int) (terminalFile, running, error) {
	console, err := conpty.Start(command.Path, command.Args[1:], command.Dir, command.Env, columns, rows)
	if err != nil {
		return nil, nil, err
	}

	// Assigned rather than started suspended into the job, because the console
	// owns the creation. A child spawned in the moment before the assignment
	// could escape; the window is small, and the job still takes everything
	// spawned after it.
	if err := group.containRunning(console.PID()); err != nil {
		_ = console.Kill()
		_, _ = console.Wait()
		_ = console.Close()
		return nil, nil, err
	}

	return console, consoleProcess{console}, nil
}

func resizeTerminal(terminal terminalFile, columns, rows int) error {
	console, ok := terminal.(*conpty.Console)
	if !ok {
		return errors.New("process: this terminal cannot be resized")
	}
	return console.Resize(columns, rows)
}

// consoleProcess is a program started by a Windows pseudo console.
type consoleProcess struct{ console *conpty.Console }

func (p consoleProcess) pid() int           { return p.console.PID() }
func (p consoleProcess) wait() (int, error) { return p.console.Wait() }
func (p consoleProcess) kill() error        { return p.console.Kill() }
