//go:build windows

package process

import (
	"errors"
	"os/exec"
)

// startWithTerminal reports that there is no terminal here.
//
// Windows has one — ConPTY, through CreatePseudoConsole — and it is a real
// piece of work rather than a missing import: the API wants handles wired up
// before the process is created, which os/exec does not expose, so it needs
// its own CreateProcess call with an attribute list. Written as an honest nil
// rather than an approximation, so a caller is told it did not get a terminal
// instead of waiting for a prompt that never flushes.
func startWithTerminal(*exec.Cmd, int, int) (terminalFile, error) {
	return nil, nil
}

func resizeTerminal(terminalFile, int, int) error {
	return errors.New("process: no terminal on this platform to resize")
}
