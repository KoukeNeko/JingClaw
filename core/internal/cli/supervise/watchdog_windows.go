//go:build windows

package supervise

import (
	"io"
)

// WatchIfAsked does nothing here.
//
// The watchdog outlives a hangup by being in a session of its own, which
// Windows does not have. Containing a part's tree is done by the job object it
// is started in — see group_windows.go — so this way of leaving needs nothing
// separate here.
func WatchIfAsked() {}

// watchOver starts nothing, so there is nothing to hold.
func watchOver(string, ...*supervised) (io.Closer, error) { return nil, nil }
