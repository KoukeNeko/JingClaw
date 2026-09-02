//go:build windows

package supervise

import "os/exec"

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
