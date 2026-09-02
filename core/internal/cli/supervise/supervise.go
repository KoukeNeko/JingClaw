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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/KoukeNeko/JingClaw/core/internal/cli/console"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
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
		return attach(ctx)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("supervise: find this program: %w", err)
	}

	// Started from this file rather than from the name on PATH. A machine
	// commonly has an installed copy and a freshly built one, and a gateway
	// from the other copy is the hardest kind of wrong: both processes are
	// correct, and they are not the same program.
	// Where the parts write. When somebody is watching, that is a file: the
	// console owns the bottom line of the terminal, and a part writing to the
	// same stream lands in the middle of whatever is being typed. Without a
	// terminal — under a service, or piped — there is no console and no line
	// to protect, so they write where they always did.
	output, closeOutput, watching := where()
	defer closeOutput()

	agent, err := start(ctx, self, "daemon", output)
	if err != nil {
		return err
	}
	defer terminate(agent)

	if err := waitForReady(ctx); err != nil {
		return err
	}

	chat, err := start(ctx, self, "gateway", output)
	if err != nil {
		return err
	}
	defer terminate(chat)

	if watching {
		return watch(ctx, agent, chat)
	}

	fmt.Println("JingClaw is running. Press Ctrl-C to stop it.")

	select {
	case <-ctx.Done():
		return nil
	case err := <-exits(agent):
		return stopped("daemon", err)
	case err := <-exits(chat):
		return stopped("gateway", err)
	}
}

// where says what the parts should write to, and whether anybody is looking.
//
// A file under the deployment when this is a terminal, so the console has the
// screen to itself and there is still somewhere to look when something goes
// wrong. Standard error otherwise, which is what a service captures.
func where() (io.Writer, func(), bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stderr, func() {}, false
	}

	dir, found := home.Resolve()
	if !found {
		return os.Stderr, func() {}, false
	}
	if err := os.MkdirAll(dir.Log(), 0o700); err != nil {
		return os.Stderr, func() {}, false
	}

	path := filepath.Join(dir.Log(), "jingclaw.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Not worth refusing to start over. Sharing the screen is worse than
		// a console, and better than nothing running at all.
		return os.Stderr, func() {}, false
	}
	return file, func() { _ = file.Close() }, true
}

// watch runs the console until it is closed or a part stops.
func watch(ctx context.Context, agent, chat *exec.Cmd) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// A part stopping ends the console too: what it was showing has gone.
	go func() {
		select {
		case <-ctx.Done():
		case <-exits(agent):
			stop()
		case <-exits(chat):
			stop()
		}
	}()

	if err := console.Run(ctx, "", howLeavingWorks(false)); err != nil {
		// The console failing is not the parts failing. They keep running,
		// and saying so is more use than an error nobody can act on.
		fmt.Fprintln(os.Stderr, "the console stopped:", err)
		fmt.Println("JingClaw is still running. Press Ctrl-C to stop it.")
		<-ctx.Done()
	}
	return nil
}

// attach watches a deployment this command did not start.
//
// A console rather than a wait. "Watching it" said and then nothing drawn is
// worse than saying nothing: the terminal looks attached to something and is
// attached to a signal handler, and the way to find out is to notice that
// hours have passed with no output.
func attach(ctx context.Context) error {
	fmt.Println("JingClaw is already running; watching it.")
	sayIfTheBuildDidNotReachIt()

	if !canAttach(term.IsTerminal(int(os.Stdin.Fd()))) {
		// Nothing to draw on, so there is nothing better to do than wait —
		// which is what a service wants anyway.
		<-ctx.Done()
		return nil
	}

	if err := console.Run(ctx, "", howLeavingWorks(true)); err != nil {
		// The console failing is not the parts failing. They keep running,
		// and saying so is more use than an error nobody can act on.
		fmt.Fprintln(os.Stderr, "the console stopped:", err)
		fmt.Println("JingClaw is still running.")
		<-ctx.Done()
	}
	return nil
}

// sayIfTheBuildDidNotReachIt warns when what is running predates this build.
//
// The wrapper script builds first, and then this attaches to whatever it
// finds. Both halves are working as intended and together they are a trap:
// the screen says a build finished, the deployment answering is the one from
// before it, and a change that landed in between is simply not there. What
// happens next is somebody deciding the change does not work.
//
// Times rather than versions, because the file this compares is the one that
// was just written and the deployment publishes when it started. Nothing is
// claimed when either is unreadable.
func sayIfTheBuildDidNotReachIt() {
	built, ok := whenBuilt()
	if !ok {
		return
	}
	started, ok := whenItStarted()
	if !ok {
		return
	}
	if !buildIsNewerThanWhatIsRunning(built, started) {
		return
	}

	now := time.Now()
	fmt.Printf("Note: this build is newer than what is running "+
		"(built %s, running since %s).\n",
		clockOf(built, now), clockOf(started, now))
	fmt.Println("      Run `stop` and start again to run the code you just built.")
}

// clockOf is a time to read at a glance, dated when it is not from today.
//
// A clock alone cannot say whether the thing running started this morning or
// on Friday, and this note exists to answer exactly that.
func clockOf(when, now time.Time) string {
	if when.YearDay() == now.YearDay() && when.Year() == now.Year() {
		return when.Format(time.Kitchen)
	}
	return when.Format("Jan 2 " + time.Kitchen)
}

// buildIsNewerThanWhatIsRunning is the comparison, kept apart so it can be
// checked without a filesystem.
func buildIsNewerThanWhatIsRunning(built, started time.Time) bool {
	if built.IsZero() || started.IsZero() {
		return false
	}
	return built.After(started)
}

// whenBuilt is when this executable was last written.
func whenBuilt() (time.Time, bool) {
	self, err := os.Executable()
	if err != nil {
		return time.Time{}, false
	}
	info, err := os.Stat(self)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// whenItStarted is when the running deployment published itself.
func whenItStarted() (time.Time, bool) {
	path, err := discovery.Path()
	if err != nil {
		return time.Time{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// howLeavingWorks is what quitting the console means.
//
// Its own function because it is the one part of attaching that can be
// checked without a terminal, and because getting it backwards is silent: a
// console that stopped a deployment it found would end somebody else's
// session, and one that left its own running would leave two daemons for the
// next start to find.
func howLeavingWorks(foundAlreadyRunning bool) console.Leaving {
	if foundAlreadyRunning {
		return console.LeavesItRunning
	}
	return console.StopsIt
}

// canAttach says whether there is anything to draw a console on.
func canAttach(isTerminal bool) bool { return isTerminal }

// stopped says why one of the parts is no longer running.
//
// A part that exits without an error was asked to: jingclaw stop signals the
// daemon, and the daemon exiting is then the whole point rather than a
// failure. Reporting that as one made every clean shutdown end with an error
// line and a non-zero status.
func stopped(part string, err error) error {
	if err == nil {
		fmt.Printf("The %s stopped; stopping the rest.\n", part)
		return nil
	}
	return fmt.Errorf("the %s stopped: %w", part, err)
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

func start(ctx context.Context, self, part string, output io.Writer) (*exec.Cmd, error) {
	command := exec.CommandContext(ctx, self, part)
	command.Stdout = output
	command.Stderr = output

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

	// Waited for. A signal is a request and returns before the process has
	// acted on it, and the wrapper script runs this and then starts again in
	// the same line — so returning early means the second half looks at a
	// process that is still dying, attaches to it, and reports that the build
	// just made has not reached the deployment. Which is advice to run exactly
	// what was just run.
	if !gone(file.PID, alive, stopWithin, time.Sleep) {
		fmt.Printf("asked pid %d to stop; it is still running after %s\n",
			file.PID, stopWithin)
		return nil
	}

	fmt.Printf("stopped pid %d\n", file.PID)
	return nil
}

// stopWithin is how long a stop waits before saying it did not happen.
//
// Long enough for a deployment to close its listeners and its database, and
// short enough that a terminal is not held by a command waiting for something
// that is not going to happen.
const stopWithin = 10 * time.Second

// gone waits for a process to have actually gone, and says whether it did.
//
// The clock is a parameter so this can be checked without one.
func gone(pid int, running func(int) bool, within time.Duration, sleep func(time.Duration)) bool {
	const breath = 50 * time.Millisecond

	for waited := time.Duration(0); waited <= within; waited += breath {
		if !running(pid) {
			return true
		}
		sleep(breath)
	}
	return !running(pid)
}
