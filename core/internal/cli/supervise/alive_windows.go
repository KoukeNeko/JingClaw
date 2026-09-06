//go:build windows

package supervise

import "golang.org/x/sys/windows"

// stillActive is the exit code Windows reports for a process that has not
// exited (STILL_ACTIVE / STATUS_PENDING).
const stillActive = 259

// alive asks whether a process exists, without disturbing it.
//
// The POSIX probe the other platforms use — os.Process.Signal(0) — does not
// exist here: on Windows os.Process.Signal supports nothing but Kill and
// returns an error for signal 0, so a healthy daemon would read as gone. That
// is precisely what happened: the supervisor started the daemon, could never
// see it publish itself, and tore down a process that was working.
//
// So this asks Windows directly. A process still running has the exit code
// STILL_ACTIVE; one that has exited has its real code, and one that is gone
// entirely cannot be opened at all.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		// It opened, so it is there; only reading its exit code failed. The
		// answer that keeps a running daemon running is that it is alive.
		return true
	}
	return code == stillActive
}
