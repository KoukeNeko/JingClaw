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
