// Package supervise runs the whole product as one process.
//
// `jingclaw` with no arguments runs the daemon, the chat gateway and, on a
// terminal, the console inside this process. They used to be a supervisor and
// two child processes with a watchdog beside them; what that bought was crash
// isolation for the gateway, which holds somebody else's bot token and a
// platform library with a socket open to the internet. That is given up here
// on purpose (the gateway package says what is left of it) in exchange for one
// process to start, stop, containerise and find in ps.
//
// What stays a boundary is the code one: the gateway still reaches the daemon
// over loopback with its own gateway-scoped credential, through the same
// ingress as when it was a process, and has no way into the runtime.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/client"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/console"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/daemon"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// readyTimeout is how long a daemon gets to publish itself before starting it
// is called a failure.
const readyTimeout = 60 * time.Second

// stopGrace is how long the daemon gets to finish after being asked to stop.
// The daemon bounds its own teardown (about twelve seconds); this is the
// outer limit, so a stop cannot hang forever on something that will not go.
const stopGrace = 15 * time.Second

// chatGrace is how long the gateway gets to stop before the daemon is asked to.
const chatGrace = 10 * time.Second

// parts are the two long-running pieces Run composes. Fields rather than
// direct calls so that a test can play them without a database or a bot.
type parts struct {
	daemon  func(ctx context.Context) error
	gateway func(ctx context.Context) error
}

// part is a function running in a goroutine of its own, and how it ended.
type part struct {
	done chan struct{}
	err  error // valid once done is closed
}

func goPart(fn func() error) *part {
	p := &part{done: make(chan struct{})}
	go func() {
		p.err = fn()
		close(p.done)
	}()
	return p
}

// stopsWithin waits for the part to have ended, for at most d.
func (p *part) stopsWithin(d time.Duration) {
	select {
	case <-p.done:
	case <-time.After(d):
	}
}

// Run starts whatever is not already running and stays until interrupted.
//
// The rule is that you stop what you started. Started here, the daemon and the
// gateway are stopped on the way out; found already running — a service, or
// another terminal — they are left alone and this attaches a console to them.
// Without that rule the second run of this command is two daemons on one
// database, each of them certain it is the one.
//
// Two things leave part of it off without ending it, because what is left is
// what somebody puts them right with. A daemon that came up with no agent
// (repair mode) gets no gateway and no console, and the run says why and waits.
// A gateway that cannot start because its settings or its token are wrong is
// said and left off. A gateway that fails any other way is logged and started
// again with a growing wait (see keepChatUp): a crash-looping gateway must not
// take the daemon and the dashboard down with it. Only the daemon ending, or
// the console being closed, ends the run.
func Run(ctx context.Context) error {
	return run(ctx, parts{
		daemon:  func(ctx context.Context) error { return daemon.Run(ctx, nil) },
		gateway: func(ctx context.Context) error { return gateway.Run(ctx, nil) },
	})
}

func run(ctx context.Context, p parts) error {
	// The hangup as well as the interrupt and the terminate. A terminal that
	// goes away — a window closed, an ssh session dropped — sends this, and
	// Go's own answer to it is to die without running anything deferred: the
	// discovery file would be left behind, naming a process that is gone.
	//
	// An ssh session with no terminal at all sends no signal, and there the
	// console's own read returns EOF and leaves the same way.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, hangup)
	defer stop()

	forgetAFileNamingThisProcess()

	if running, err := alreadyRunning(); err != nil {
		return err
	} else if running {
		return attach(ctx)
	}

	// Where this process writes. When somebody is watching, that is a file: the
	// console owns the bottom line of the terminal, and the daemon and the
	// gateway write through os.Stdout and os.Stderr, so those are pointed at
	// the file and the console is handed the real terminal. Without a terminal
	// — under a service, or piped — there is no console and no line to
	// protect, so they write where they always did.
	tty := os.Stdout
	logFile, watching := where()
	if watching {
		oldStdout, oldStderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = logFile, logFile
		defer func() { _ = logFile.Close() }()
		defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()
	}

	// The daemon's context does not descend from ctx's cancellation: a signal
	// stops the gateway first, and the daemon only after it has gone.
	daemonCtx, stopDaemon := context.WithCancel(context.WithoutCancel(ctx))
	defer stopDaemon()
	chatCtx, stopChat := context.WithCancel(ctx)
	defer stopChat()

	agent := goPart(func() error { return p.daemon(daemonCtx) })
	var chat *part

	// Registered after the log file is arranged, so that it runs first: the
	// gateway, then the daemon, which removes the discovery file on its way
	// out, each given a bounded time.
	defer func() {
		stopChat()
		if chat != nil {
			chat.stopsWithin(chatGrace)
		}
		stopDaemon()
		agent.stopsWithin(stopGrace)
	}()

	if err := waitForReady(ctx, agent, alreadyRunning); err != nil {
		return err
	}

	// Asked now that the daemon has published itself, because only then is there
	// anything to ask. One that could not build an agent is up and serving what
	// is needed to fix that, and a gateway has no agent to reach.
	if publishedMode() == discovery.ModeRepair {
		return stayWithoutAnAgent(ctx, tty, agent)
	}

	// Said to whoever is looking. In front of a console that is the console,
	// which owns the screen: a line printed beside it lands in the middle of
	// what is being typed. Dropped when it is not read and the buffer is full,
	// rather than holding up the part that has something to say.
	notices := make(chan string, 4)
	tell := func(line string) { fmt.Println(line) }
	if watching {
		tell = func(line string) {
			select {
			case notices <- line:
			default:
			}
		}
	}

	chat = goPart(func() error {
		keepChatUp(chatCtx, p.gateway, tell, chatPace)
		return nil
	})

	if watching {
		return watch(ctx, tty, agent.done, notices)
	}

	fmt.Println("JingClaw is running. Press Ctrl-C to stop it.")

	select {
	case <-ctx.Done():
		return nil
	case <-agent.done:
		return stopped(os.Stdout, "daemon", agent.err)
	}
}

// where says whether anybody is looking, and gives the file the daemon and the
// gateway should write to if so.
//
// A file under the deployment when this is a terminal, so the console has the
// screen to itself and there is still somewhere to look when something goes
// wrong. Otherwise standard error stays what a service captures.
func where() (*os.File, bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, false
	}

	dir, found := home.Resolve()
	if !found {
		return nil, false
	}
	if err := os.MkdirAll(dir.Log(), 0o700); err != nil {
		return nil, false
	}

	path := filepath.Join(dir.Log(), "jingclaw.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// Not worth refusing to start over. Sharing the screen is worse than
		// a console, and better than nothing running at all.
		return nil, false
	}
	return file, true
}

// watch runs the console until it is closed or the daemon stops.
func watch(ctx context.Context, tty *os.File, agent <-chan struct{}, notices <-chan string) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// The daemon stopping ends the console too: what it was showing has gone.
	go func() {
		select {
		case <-ctx.Done():
		case <-agent:
			stop()
		}
	}()

	if err := console.Run(ctx, "", howLeavingWorks(false), notices, tty); err != nil {
		// The console failing is not the daemon failing. It keeps running,
		// and saying so is more use than an error nobody can act on.
		fmt.Fprintln(tty, "the console stopped:", err)
		fmt.Fprintln(tty, "JingClaw is still running. Press Ctrl-C to stop it.")
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

	if publishedMode() == discovery.ModeRepair {
		// Nothing to draw a console of: it follows an agent's sessions, and
		// there is none. What there is to see is why.
		sayWhyThereIsNoAgent(ctx, os.Stdout)
		<-ctx.Done()
		return nil
	}

	if !canAttach(term.IsTerminal(int(os.Stdin.Fd()))) {
		// Nothing to draw on, so there is nothing better to do than wait —
		// which is what a service wants anyway.
		<-ctx.Done()
		return nil
	}

	if err := console.Run(ctx, "", howLeavingWorks(true), nil, os.Stdout); err != nil {
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

// stopped says why the daemon is no longer running.
//
// One that returns without an error was asked to, or had nothing left to do,
// and is not a failure; reporting that as one made every clean shutdown end
// with an error line and a non-zero status. One that returns an error is quoted
// in the error, which main prints, rather than only in a log file nobody was
// told about.
func stopped(out io.Writer, part string, why error) error {
	if why == nil {
		fmt.Fprintf(out, "The %s stopped; stopping the rest.\n", part)
		return nil
	}
	return fmt.Errorf("the %s stopped: %w", part, why)
}

// forgetAFileNamingThisProcess removes a discovery file that names this very
// process, which at this point can only be a leftover.
//
// Nothing has been published yet, so a file with our own pid is from an earlier
// process that had it and was killed before it could clean up. In a container
// pids repeat from one start to the next, and the leftover would read as a
// daemon that is up: this process would attach to itself and start nothing.
func forgetAFileNamingThisProcess() {
	if path, err := discovery.Path(); err == nil {
		_ = discovery.RemoveIfOwnedBy(path, os.Getpid())
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

// publishedMode is what the daemon says it is, in the file it published:
// discovery.ModeRepair for one running no agent, and nothing for one that is.
//
// Nothing as well when the file cannot be read. This is asked after the daemon
// was seen to have published, and a file gone since is a daemon going, which
// the part watching for that reports.
func publishedMode() string {
	path, err := discovery.Path()
	if err != nil {
		return ""
	}
	file, err := discovery.Read(path)
	if err != nil {
		return ""
	}
	return file.Mode
}

// stayWithoutAnAgent keeps the daemon and its dashboard up until it is asked to
// stop, when the daemon has no agent and there is therefore nothing to watch
// and no gateway to start.
//
// A daemon that could not build an agent is not this command failing. What it
// is serving is what somebody needs to put that right, and ending the run
// would take it away. Why is said once, and then the run waits. A daemon that
// goes away still ends it.
func stayWithoutAnAgent(ctx context.Context, out io.Writer, agent *part) error {
	sayWhyThereIsNoAgent(ctx, out)
	fmt.Fprintln(out, "JingClaw is running without an agent. Press Ctrl-C to stop it.")

	select {
	case <-ctx.Done():
		return nil
	case <-agent.done:
		return stopped(out, "daemon", agent.err)
	}
}

// sayWhyThereIsNoAgent asks the daemon why, and says what it answered.
func sayWhyThereIsNoAgent(ctx context.Context, out io.Writer) {
	asking, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	state, err := client.AgentState(asking, "")
	fmt.Fprintln(out, agentNotRunning(state, err))
}

// agentNotRunning is the one line that says there is no agent, why, and what to
// run about it.
//
// The first problem and how many more there are, not all of them: a line in a
// terminal is not the place to read a list, and the dashboard it points at is
// where each is shown with its fix. Not being able to ask is said in its place,
// because the pointer to the dashboard is the part that still helps.
func agentNotRunning(state *controlv1.GetAgentStateResponse, asked error) string {
	const pointer = " — run: jingclaw dashboard"

	if asked != nil {
		return fmt.Sprintf("agent not running (could not ask the daemon why: %v)%s", asked, pointer)
	}

	problems := state.GetProblems()
	if len(problems) == 0 {
		return "agent not running" + pointer
	}

	more := ""
	if extra := len(problems) - 1; extra > 0 {
		more = fmt.Sprintf(" (and %d more)", extra)
	}
	return fmt.Sprintf("agent not running: %s%s%s", client.ProblemLine(problems[0]), more, pointer)
}

// waitForReady blocks until the daemon has published itself.
//
// The gateway is started after, because a gateway that comes up first spends
// its early life failing to reach something that is not there yet and says so
// in a way that reads like a fault.
func waitForReady(ctx context.Context, agent *part, published func() (bool, error)) error {
	deadline := time.NewTimer(readyTimeout)
	defer deadline.Stop()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		if running, _ := published(); running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-agent.done:
			// It has returned, so waiting out the timeout would be a minute of
			// blank terminal before saying what is already known. What it
			// returned is the answer.
			return failedToStart(agent.err)

		case <-deadline.C:
			// Still running and still not published.
			return failedToStart(nil)

		case <-tick.C:
		}
	}
}

// failedToStart is the message, carrying why when the daemon said.
func failedToStart(why error) error {
	if why != nil {
		return fmt.Errorf("supervise: the daemon did not start: %w", why)
	}
	return errors.New("supervise: the daemon did not start, and said nothing about why")
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

	fmt.Printf("running\npid       %d\nlistening %s\nmode      %s\n",
		file.PID, file.BaseURL, describeMode(file.Mode))
	return nil
}

// describeMode is the mode a daemon published, in words. The absence of one is
// a daemon running an agent, which is every daemon there was before there was
// a mode to publish.
func describeMode(mode string) string {
	if mode == discovery.ModeRepair {
		return "repair — no agent is running; run: jingclaw dashboard"
	}
	return "running"
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
