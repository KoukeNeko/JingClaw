//go:build linux

package main

import (
	"os"

	"github.com/KoukeNeko/JingClaw/core/internal/sandbox"
)

// confineIfAsked turns this process into a confined command, when that is
// what it was started to be.
//
// Checked before anything else runs, including flag parsing: a process in
// this mode is not the program, it is the last thing that happens before a
// command replaces it. Cobra never sees these arguments, and nothing here
// opens a database or a socket that would then be inherited by whatever runs.
//
// Why a second process at all is in internal/sandbox. Briefly: Landlock
// restricts whoever asks and everything they go on to exec, and in Go there
// is no safe moment between fork and exec to ask — the runtime is
// multithreaded and a forked child may not allocate, lock or schedule. So the
// asking happens in a process whose whole life is to ask and then be
// replaced.
func confineIfAsked() {
	// Said whether or not this process is the one confining, because it is a
	// fact about the executable rather than about this run: Wrap re-executes
	// it and needs to know it will be understood.
	sandbox.WillConfine()

	if !sandbox.Confining(os.Args[1:]) {
		return
	}
	// Never returns: either the command is running here, or this exits.
	sandbox.Confine(os.Args[1:])
}
