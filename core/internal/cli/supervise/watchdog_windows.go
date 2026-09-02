//go:build windows

package supervise

import (
	"io"
	"os/exec"
)

// WatchIfAsked does nothing here.
//
// The watchdog watches process groups and outlives a hangup by being in a
// session of its own, and Windows has neither. Containing a tree there needs
// a Job Object, which is a different mechanism and not this one.
func WatchIfAsked() {}

// watchOver starts nothing, so there is nothing to hold.
func watchOver(string, ...*exec.Cmd) (io.Closer, error) { return nil, nil }
