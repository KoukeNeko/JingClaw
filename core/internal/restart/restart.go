// Package restart is how the process starts itself again.
//
// A restart is a shutdown like any other, in the same order, followed by the
// same binary run again with the same arguments and environment. Whatever
// ends the run (the daemon alone, or the daemon with the gateway and the
// console in one process) ends it by cancelling the context it was given; this
// package is what cancels that context when the dashboard asks, and what starts
// the new process once main has got back from the old one.
package restart

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// AddrEnv carries the address the old process was serving on to the new one.
// Internal: it is consumed at start and removed from the environment, so
// nothing the process starts inherits it.
const AddrEnv = "JINGCLAW_RESTART_ADDR"

// delay is how long the caller has to be answered before the shutdown starts.
// The shutdown waits for requests in flight anyway; this makes the order
// certain rather than a race.
const delay = 250 * time.Millisecond

var (
	mu     sync.Mutex
	wanted bool
	addr   string
	wake   = make(chan struct{})
	timer  *time.Timer
)

// Request asks for a restart, remembering the address to come back on. False
// when one has already been asked for.
func Request(bound string) bool {
	mu.Lock()
	defer mu.Unlock()
	if wanted {
		return false
	}
	wanted, addr = true, bound
	timer = time.AfterFunc(delay, func() { close(wake) })
	return true
}

// Wanted is whether a restart was asked for.
func Wanted() bool {
	mu.Lock()
	defer mu.Unlock()
	return wanted
}

// Reset forgets a request. For tests, which share a process.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	wanted, addr, timer, wake = false, "", nil, make(chan struct{})
}

// NotifyContext is signal.NotifyContext that also ends when a restart is asked
// for, so the shutdown is the one a signal starts.
func NotifyContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	mu.Lock()
	asked := wake
	mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	ctx, stop := signal.NotifyContext(ctx, signals...)
	go func() {
		select {
		case <-asked:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { stop(); cancel() }
}

// TakeAddr is the address the previous process was serving on, if this one
// was started by a restart and the address is a loopback one. The variable is
// removed whatever it held.
func TakeAddr() string {
	given := os.Getenv(AddrEnv)
	_ = os.Unsetenv(AddrEnv)
	if !config.LoopbackAddress(given) {
		return ""
	}
	return given
}

// Spec is what to run to start again: the same arguments, and the environment
// with the address to come back on. Pure, so it can be checked without
// replacing the test process.
func Spec(args, env []string, bound string) (argv, environ []string) {
	environ = make([]string, 0, len(env)+1)
	for _, pair := range env {
		if !strings.HasPrefix(pair, AddrEnv+"=") {
			environ = append(environ, pair)
		}
	}
	if bound != "" {
		environ = append(environ, AddrEnv+"="+bound)
	}
	return args, environ
}

// executable is the running binary. Linux appends " (deleted)" to the path of
// one that was replaced while running, which is the one an upgrade wants.
func executable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if trimmed := strings.TrimSuffix(path, " (deleted)"); trimmed != path {
		if _, err := os.Stat(trimmed); err == nil {
			return trimmed, nil
		}
	}
	return path, nil
}

// Exec starts the process again, if a restart was asked for. On Unix it
// replaces this process, so the pid and the terminal stay; it returns only on
// failure. On Windows it starts the new process and returns nil, and the
// caller exits.
func Exec() error {
	mu.Lock()
	bound := addr
	mu.Unlock()

	path, err := executable()
	if err != nil {
		return err
	}
	argv, environ := Spec(os.Args, os.Environ(), bound)
	return start(path, argv, environ)
}
