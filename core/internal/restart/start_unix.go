//go:build !windows

package restart

import "syscall"

func start(path string, argv, environ []string) error {
	return syscall.Exec(path, argv, environ)
}
