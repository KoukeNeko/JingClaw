// Package supervise starts the parts and keeps them together.
//
// The daemon and the gateway are separate processes on purpose: the gateway
// holds somebody else's bot token and keeps a socket open to the internet, and
// the process that owns the shell, the workspace and the event log must not go
// down with it. One executable and two processes are not in tension — this is
// what makes them one executable without making them one process.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

// readyTimeout is how long a daemon gets to publish itself before starting it
// is called a failure.
const readyTimeout = 60 * time.Second

// stopGrace is how long a part gets to finish after being asked to stop.
const stopGrace = 15 * time.Second

// Run starts whatever is not already running and stays until interrupted.
//
// The rule is that you stop what you started. Started here, the parts are
// stopped on the way out; found already running — a service, or another
// terminal — they are left alone. Without that rule the second run of this
// command is two daemons on one database, each of them certain it is the one.
func Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if running, err := alreadyRunning(); err != nil {
		return err
	} else if running {
		fmt.Println("JingClaw is already running; watching it.")
		<-ctx.Done()
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("supervise: find this program: %w", err)
	}

	// Started from this file rather than from the name on PATH. A machine
	// commonly has an installed copy and a freshly built one, and a gateway
	// from the other copy is the hardest kind of wrong: both processes are
	// correct, and they are not the same program.
	agent, err := start(ctx, self, "daemon")
	if err != nil {
		return err
	}
	defer terminate(agent)

	if err := waitForReady(ctx); err != nil {
		return err
	}

	chat, err := start(ctx, self, "gateway")
	if err != nil {
		return err
	}
	defer terminate(chat)

	fmt.Println("JingClaw is running. Press Ctrl-C to stop it.")

	select {
	case <-ctx.Done():
		return nil
	case err := <-exits(agent):
		return fmt.Errorf("the daemon stopped: %w", err)
	case err := <-exits(chat):
		return fmt.Errorf("the gateway stopped: %w", err)
	}
}

// alreadyRunning reports whether a daemon has published itself and is alive.
//
// A discovery file left by a process that is gone is not a running daemon. It
// is what a crash leaves behind, and treating it as one would mean refusing to
// start for the rest of the machine's life.
func alreadyRunning() (bool, error) {
	path, err := discovery.Path()
	if err != nil {
		return false, fmt.Errorf("supervise: find the discovery file: %w", err)
	}

	file, err := discovery.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		// Unreadable is not running. Starting is the recoverable answer; a
		// daemon that is genuinely up will refuse the address itself.
		return false, nil
	}

	return alive(file.PID), nil
}

// alive asks whether a process exists, without disturbing it.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func start(ctx context.Context, self, part string) (*exec.Cmd, error) {
	command := exec.CommandContext(ctx, self, part)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr

	// Its own process group, so a Ctrl-C in this terminal reaches this
	// program and lets it stop the parts in order, rather than arriving at
	// all three at once and racing the shutdown it was about to run.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("supervise: start the %s: %w", part, err)
	}
	return command, nil
}

// waitForReady blocks until the daemon has published itself.
//
// The gateway is started after, because a gateway that comes up first spends
// its early life failing to reach something that is not there yet and says so
// in a way that reads like a fault.
func waitForReady(ctx context.Context) error {
	deadline := time.NewTimer(readyTimeout)
	defer deadline.Stop()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		if running, _ := alreadyRunning(); running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("supervise: the daemon did not start")
		case <-tick.C:
		}
	}
}

// terminate asks a part to stop and waits, briefly.
//
// Asked rather than killed: the daemon has a database to close and runs to
// let go of, and the difference between a clean stop and a killed one is a
// session that comes back and one that comes back with a hole in it.
func terminate(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(syscall.SIGTERM)

	done := make(chan struct{})
	go func() { _, _ = command.Process.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(stopGrace):
		_ = command.Process.Kill()
	}
}

func exits(command *exec.Cmd) <-chan error {
	out := make(chan error, 1)
	go func() { out <- command.Wait() }()
	return out
}

// Commands are the subcommands for managing what Run starts.
func Commands() []*cobra.Command {
	return []*cobra.Command{
		{
			Use:   "status",
			Short: "Say whether JingClaw is running, and where",
			RunE:  func(*cobra.Command, []string) error { return status() },
		},
		{
			Use:   "stop",
			Short: "Stop a running JingClaw",
			RunE:  func(*cobra.Command, []string) error { return stopRunning() },
		},
	}
}

func status() error {
	path, err := discovery.Path()
	if err != nil {
		return err
	}

	file, err := discovery.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("not running")
		return nil
	}
	if err != nil {
		return err
	}

	if !alive(file.PID) {
		fmt.Printf("not running (a stale %s names pid %d)\n", path, file.PID)
		return nil
	}

	fmt.Printf("running\npid       %d\nlistening %s\n", file.PID, file.BaseURL)
	return nil
}

func stopRunning() error {
	path, err := discovery.Path()
	if err != nil {
		return err
	}

	file, err := discovery.Read(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !alive(file.PID)) {
		fmt.Println("not running")
		return nil
	}
	if err != nil {
		return err
	}

	process, err := os.FindProcess(file.PID)
	if err != nil {
		return fmt.Errorf("supervise: find pid %d: %w", file.PID, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("supervise: stop pid %d: %w", file.PID, err)
	}

	fmt.Printf("asked pid %d to stop\n", file.PID)
	return nil
}
