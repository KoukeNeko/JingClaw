//go:build !windows

package supervise

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts a part in a process group of its own.
//
// So that a Ctrl-C in this terminal reaches this program and lets it stop the
// parts in order, rather than arriving at all three at once and racing the
// shutdown it was about to run.
func ownProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// askTheGroupToStop signals the part and everything it started.
//
// The negative pid addresses the group, which is why the parts are put in one
// of their own: a build tool or a dev server started by a part is in that
// group too, and stopping the part alone would leave it holding a port.
//
// Falling back to the process where the group has already gone, because the
// alternative is refusing to stop what remains.
func askTheGroupToStop(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if syscall.Kill(-command.Process.Pid, syscall.SIGTERM) != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
	}
}

// killTheGroup is the same thing without asking, for what did not go.
func killTheGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	if syscall.Kill(-command.Process.Pid, syscall.SIGKILL) != nil {
		_ = command.Process.Kill()
	}
}

// hangup is the signal a terminal sends when it goes away.
const hangup = syscall.SIGHUP
