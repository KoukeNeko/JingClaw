//go:build !windows

package fsperm

import (
	"fmt"
	"os"
)

// File and directory modes that keep a path to its owner. A directory needs
// the execute bit to be entered at all, so the two differ.
const (
	fileMode = 0o600
	dirMode  = 0o700
)

// Restrict chmods path to owner-only, choosing the file or directory mode by
// what path is.
func Restrict(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("fsperm: %w", err)
	}

	mode := os.FileMode(fileMode)
	if info.IsDir() {
		mode = dirMode
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("fsperm: restrict %s: %w", path, err)
	}
	return nil
}

// EnsureOwnerOnly reports whether path denies group and other any access.
func EnsureOwnerOnly(path string) (ownerOnly bool, detail string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, "", fmt.Errorf("fsperm: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return false, fmt.Sprintf("is mode %#o", perm), nil
	}
	return true, "", nil
}
