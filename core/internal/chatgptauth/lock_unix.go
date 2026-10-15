//go:build !windows

package chatgptauth

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created owner-only if
// missing) and blocks until it has it. The lock is the file descriptor's, so it
// is released when the returned func runs or the process dies.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
