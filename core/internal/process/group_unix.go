//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group.
//
// Without it, stopping a process leaves anything it spawned running: a dev
// server's watcher keeps the port bound, and the next start fails for a reason
// that looks nothing like the cause.
func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the whole group to stop.
//
// SIGTERM so a program can flush what it was holding; the caller kills after a
// grace period. The negative pid addresses the group rather than only the
// process that was started.
func terminateGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}

	if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
		// The group may already be gone, in which case signalling the process
		// alone is the best that remains.
		return command.Process.Signal(syscall.SIGTERM)
	}
	return nil
}
