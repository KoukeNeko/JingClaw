//go:build windows

package restart

import (
	"os"
	"os/exec"
)

// start runs the new process with this one's console and returns, and the
// caller exits. Windows cannot replace a process in place, so the pid changes
// and a terminal it was started from is shared between the two for a moment.
func start(path string, argv, environ []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
