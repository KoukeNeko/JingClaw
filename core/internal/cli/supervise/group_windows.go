//go:build windows

package supervise

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup is a no-op here.
//
// Windows has no process groups in the POSIX sense, and no Setpgid to ask for
// one — the field does not exist on this platform's SysProcAttr, which is why
// this file exists at all.
//
// What is lost is the ordering: a Ctrl-C reaches the parts as well as this
// program, so they may begin stopping before the supervisor asks them to.
// They stop cleanly either way; what is not guaranteed is that the daemon
// outlives the gateway on the way down.
func ownProcessGroup(*exec.Cmd) {}

// askTheGroupToStop has no group to ask here, and no graceful signal to send:
// Windows has no SIGTERM, and the console control events that come closest do
// not reach a process started without a console.
func askTheGroupToStop(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}

func killTheGroup(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}

// hangup is what a terminal going away would send, where terminals send that.
//
// Windows does not, and Go's signal package will not deliver it. Named here so
// the list of ways to leave reads the same on both platforms, and does nothing
// on this one.
const hangup = syscall.SIGHUP
