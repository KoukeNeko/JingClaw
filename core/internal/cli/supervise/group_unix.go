//go:build !windows

package supervise

import (
	"os/exec"
	"syscall"
)

// procGroup is a part's own POSIX process group.
//
// A part is a tree: it runs whatever somebody approved, and those run their own
// children. Putting it in a group of its own means a signal to the negative
// group id reaches everything it started, so stopping the part does not leave
// the rest holding a port, a database, or a lock.
type procGroup struct{}

func newProcGroup() (*procGroup, error) { return &procGroup{}, nil }

// configure puts the part in its own process group. It also lets a Ctrl-C in
// this terminal reach this program and stop the parts in order, rather than
// arriving at all of them at once and racing the shutdown about to run.
func (g *procGroup) configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// started has nothing to do: the group was asked for at creation.
func (g *procGroup) started(*exec.Cmd) error { return nil }

// askToStop signals the part and everything it started, falling back to the
// process alone where the group has already gone.
func (g *procGroup) askToStop(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if syscall.Kill(-command.Process.Pid, syscall.SIGTERM) != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
	}
}

// kill is the same thing without asking, for what did not go.
func (g *procGroup) kill(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if syscall.Kill(-command.Process.Pid, syscall.SIGKILL) != nil {
		_ = command.Process.Kill()
	}
}

// close has no handle to release.
func (g *procGroup) close() {}

// hangup is the signal a terminal sends when it goes away.
const hangup = syscall.SIGHUP
