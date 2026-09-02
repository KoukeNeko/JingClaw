//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

// procGroup is the process's own POSIX process group.
//
// A process group is the containment here: put the child in one of its own,
// and a signal to the negative group id reaches everything it spawned. There
// is no handle to keep, so the type carries nothing.
type procGroup struct{}

func newProcGroup() (*procGroup, error) { return &procGroup{}, nil }

// configure puts the child in its own process group.
//
// Without it, stopping a process leaves anything it spawned running: a dev
// server's watcher keeps the port bound, and the next start fails for a reason
// that looks nothing like the cause.
func (g *procGroup) configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// started has nothing to do once the process is running: the group was asked
// for at creation and the kernel made it.
func (g *procGroup) started(*exec.Cmd) error { return nil }

// terminate asks the whole group to stop.
//
// SIGTERM so a program can flush what it was holding; the caller kills after a
// grace period. The negative pid addresses the group rather than only the
// process that was started.
func (g *procGroup) terminate(command *exec.Cmd) error {
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

// close has no handle to release.
func (g *procGroup) close() {}
