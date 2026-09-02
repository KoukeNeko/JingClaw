//go:build !windows

package supervise

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// WatchFlag is the first argument that means "be the thing that cleans up".
//
// A hidden subcommand rather than a second binary, so that what cleans up is
// the same file as what started it and cannot be a different build.
const WatchFlag = "__watch"

// lifelineFD is where the watchdog finds the pipe, counting from the three
// the runtime always gives a process.
const lifelineFD = 3

// WatchIfAsked becomes the watchdog when this process was started as one.
//
// Called before anything else, because this process may not be the program:
// it may be the small thing left behind to notice that the program is gone.
//
// It exists because there is one way of leaving that nothing the program runs
// can cover. A console that is killed outright runs no shutdown, and its
// parts are re-parented and go on holding the port and the database. Only
// something that is already running and watching can clean that up.
func WatchIfAsked() {
	if len(os.Args) < 2 || os.Args[1] != WatchFlag {
		return
	}

	groups := make([]int, 0, len(os.Args)-2)
	for _, arg := range os.Args[2:] {
		var pgid int
		if _, err := fmt.Sscanf(arg, "%d", &pgid); err == nil && pgid > 0 {
			groups = append(groups, pgid)
		}
	}

	// The lifeline. Its other end is held by the console, and the read below
	// returns the moment that end is gone — whether it was closed on the way
	// out or vanished with a process that was killed.
	lifeline := os.NewFile(lifelineFD, "lifeline")
	if lifeline != nil {
		_, _ = io.Copy(io.Discard, lifeline)
	}

	endGroups(groups)
	os.Exit(0)
}

// endGroups asks each group to stop and then insists.
func endGroups(groups []int) {
	for _, pgid := range groups {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}

	// Waited on rather than slept through. The common case is a console that
	// left properly and stopped its parts on the way, so there is nothing
	// here to wait for — and a watchdog sitting out fifteen seconds after
	// that is a leftover process somebody finds and wonders about.
	for waited := time.Duration(0); waited <= stopGrace; waited += 50 * time.Millisecond {
		if !anyAlive(groups) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, pgid := range groups {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// anyAlive says whether any of the groups still has anything in it.
//
// Signal zero asks the kernel without sending anything, and a group with
// nothing left in it answers ESRCH.
func anyAlive(groups []int) bool {
	for _, pgid := range groups {
		if syscall.Kill(-pgid, 0) == nil {
			return true
		}
	}
	return false
}

// watchOver starts the watchdog and returns the end of the lifeline to hold.
//
// Closing what comes back, or dying and letting the kernel close it, is what
// tells the watchdog to act. Nil when there is nothing to watch or the
// watchdog would not start: it is insurance against a way of leaving that is
// rare, and refusing to run at all because the insurance is unavailable would
// be the wrong trade.
func watchOver(self string, parts ...*exec.Cmd) (io.Closer, error) {
	groups := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != nil && part.Process != nil {
			groups = append(groups, fmt.Sprint(part.Process.Pid))
		}
	}
	if len(groups) == 0 {
		return nil, nil
	}

	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("supervise: make a lifeline: %w", err)
	}

	command := exec.Command(self, append([]string{WatchFlag}, groups...)...)
	command.ExtraFiles = []*os.File{read}

	// A session of its own, so the terminal's hangup does not reach it. It
	// has to outlive the console by exactly as long as it takes to clean up
	// after it — a watchdog that died with the thing it watches would be an
	// elaborate way of doing nothing.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := command.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("supervise: start the watchdog: %w", err)
	}

	// Closed because this process has no use for it. What ends the read on
	// the other side is every write end being gone, and this process holds
	// the only one — so keeping this open would not stop the watchdog
	// noticing, it would only be a descriptor nobody can account for.
	_ = read.Close()

	return write, nil
}
