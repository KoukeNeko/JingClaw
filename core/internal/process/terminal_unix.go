//go:build !windows

package process

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// startWithTerminal starts the program attached to a pseudo-terminal.
//
// Worth the trouble for the programs exec_command cannot serve: a REPL, an
// installer that asks a question, ssh. Most of them decide what to do by
// looking at what they are attached to — block-buffering their output for a
// pipe, and often refusing to prompt at all.
func startWithTerminal(command *exec.Cmd, columns, rows int) (terminalFile, error) {
	size := &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)}
	if size.Cols == 0 {
		size.Cols = defaultColumns
	}
	if size.Rows == 0 {
		size.Rows = defaultRows
	}

	// The child needs the terminal as its controlling one, or a program that
	// reads a password or handles ^C finds nothing to talk to.
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setsid = true
	command.SysProcAttr.Setctty = true

	// Setsid makes the child its own session leader, which is what a
	// controlling terminal needs — and it supersedes the process group this
	// package otherwise asks for. Both together are refused by the kernel.
	command.SysProcAttr.Setpgid = false

	file, err := pty.StartWithSize(command, size)
	if err != nil {
		return nil, fmt.Errorf("process: open a terminal for %s: %w", command.Path, err)
	}
	return file, nil
}

func resizeTerminal(terminal terminalFile, columns, rows int) error {
	file, ok := terminal.(*os.File)
	if !ok {
		return fmt.Errorf("process: this terminal cannot be resized")
	}
	return pty.Setsize(file, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)})
}

const (
	defaultColumns = 120
	defaultRows    = 40
)
