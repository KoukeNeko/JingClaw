//go:build windows

package builtin

import "os/exec"

// configureProcessGroup is a no-op here.
//
// Windows has no process groups in the POSIX sense. Containing a process tree
// properly needs a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, which
// is a larger piece of work; until then a cancelled command kills the process
// it started and may leave descendants behind.
func configureProcessGroup(*exec.Cmd) {}

// terminateGroup kills the started process.
//
// There is no graceful signal to send: Windows has no SIGTERM, and the console
// control events that come closest do not reach a process started without a
// console.
func terminateGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
