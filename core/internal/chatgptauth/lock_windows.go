//go:build windows

package chatgptauth

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on path (created if missing) and blocks
// until it has it. It is released when the returned func runs or the process
// dies. The file holds nothing; the deployment directory is already owner-only.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped)); _ = f.Close() }, nil
}
