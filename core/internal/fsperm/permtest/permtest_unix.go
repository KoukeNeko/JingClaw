//go:build !windows

package permtest

import (
	"fmt"
	"os"
)

// Expose makes path readable by group and others.
func Expose(path string) error {
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("permtest: expose %s: %w", path, err)
	}
	return nil
}
