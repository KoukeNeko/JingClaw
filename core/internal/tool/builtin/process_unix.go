//go:build !windows

package builtin

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group.
//
// Without this, killing the child leaves anything it spawned running: a test
// runner's workers keep the port bound and the pipes open, and the tool waits
// on output that will never come.
func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup stops the whole process group.
//
// SIGTERM first so a program can flush what it was doing; the caller's
// WaitDelay escalates to a kill if that is ignored. The negative pid addresses
// the group rather than only the process that was started.
func terminateGroup(command *exec.Cmd) error {
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
