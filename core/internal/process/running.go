package process

import (
	"errors"
	"os/exec"
)

// running is a started program, whatever started it.
//
// A piped command is started through os/exec; a program on a Windows pseudo
// console is started by the console itself, because os/exec cannot attach one.
// The rest of this package waits on, sizes up, and stops a program through this
// interface so it does not have to know which of the two it is holding.
type running interface {
	// pid is the operating system's number for the program, or zero before it
	// has one.
	pid() int

	// wait blocks until the program ends and reports its exit code. It returns
	// an error only when the wait itself failed, not when the program merely
	// exited non-zero: a non-zero exit is the answer to the question that was
	// asked, not a malfunction.
	wait() (int, error)

	// kill ends the program at once.
	kill() error
}

// execProcess is a program started through os/exec.
type execProcess struct{ command *exec.Cmd }

func (p execProcess) pid() int {
	if p.command.Process != nil {
		return p.command.Process.Pid
	}
	return 0
}

func (p execProcess) wait() (int, error) {
	err := p.command.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	default:
		// The pipe or the wait itself failed. Not zero, because zero is the one
		// value that means the program worked.
		return -1, err
	}
}

func (p execProcess) kill() error {
	if p.command.Process != nil {
		return p.command.Process.Kill()
	}
	return nil
}
