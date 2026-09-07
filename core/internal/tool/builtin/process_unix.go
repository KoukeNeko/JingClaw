//go:build !windows

package builtin

import (
	"os/exec"
	"syscall"
)

// processGroup contains a command and everything it spawns in its own POSIX
// process group, so that cancelling the command stops the tree rather than
// orphaning whatever it started.
//
// Without this, killing the child leaves anything it spawned running: a test
// runner's workers keep the port bound and the pipes open, and the tool waits
// on output that will never come.
type processGroup struct{}

// newProcessGroup creates the group state. There is nothing to allocate on a
// POSIX system — the group is established by a start attribute — so this cannot
// fail.
func newProcessGroup() (*processGroup, error) {
	return &processGroup{}, nil
}

// configure asks for the child to be started in its own process group.
func (g *processGroup) configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// started has nothing to do once the process is running: the group was
// established at start.
func (g *processGroup) started(*exec.Cmd) error {
	return nil
}

// terminate stops the whole process group.
//
// SIGTERM first so a program can flush what it was doing; the caller's
// WaitDelay escalates to a kill if that is ignored. The negative pid addresses
// the group rather than only the process that was started.
func (g *processGroup) terminate(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}

	if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
		// The group may already be gone, in which case there is nothing left
		// to signal and killing the process alone is the best remaining
		// option.
		return command.Process.Kill()
	}
	return nil
}

// close has nothing to release on a POSIX system.
func (g *processGroup) close() {}
