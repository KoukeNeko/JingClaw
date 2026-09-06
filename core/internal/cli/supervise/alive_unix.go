//go:build !windows

package supervise

import (
	"os"
	"syscall"
)

// alive asks whether a process exists, without disturbing it.
//
// Signal 0 is the POSIX way to ask: it runs the kernel's permission and
// existence checks and delivers nothing. A process that is gone answers with
// an error, and one that is running answers with nil.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
