//go:build !windows

package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// stubEnv, when set, makes this test binary stand in for the program: started
// with "run" as its first argument it runs the real composition (run) with a
// stub daemon and a stub gateway in place of the two long-running parts, in a
// process of its own, so that real signals, a real discovery file and a real
// exit status can be observed without a database or a bot.
const stubEnv = "SUPERVISE_TEST_STUB"

func TestMain(m *testing.M) {
	if os.Getenv(stubEnv) != "" && len(os.Args) > 1 && os.Args[1] == "run" {
		playTheProgram()
	}
	os.Exit(m.Run())
}

// playTheProgram is what the program's own main does with Run, with the parts
// played.
func playTheProgram() {
	chatPace = pace{first: 50 * time.Millisecond, longest: 200 * time.Millisecond, healthy: time.Hour}

	err := run(context.Background(), parts{daemon: playTheDaemon, gateway: playTheGateway})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func discoveryPath() string {
	return filepath.Join(os.Getenv("JINGCLAW_RUNTIME_DIR"), "daemon.json")
}

// playTheDaemon publishes itself the way a daemon does, in the mode the test
// asked for, and on being stopped removes what it published, as the real one
// does.
func playTheDaemon(ctx context.Context) error {
	err := discovery.Write(discoveryPath(), discovery.File{
		PID:             os.Getpid(),
		BaseURL:         os.Getenv("SUPERVISE_TEST_URL"),
		Token:           "control",
		GatewayToken:    "gateway",
		Mode:            os.Getenv("SUPERVISE_TEST_MODE"),
		ProtocolVersion: discovery.ProtocolVersion,
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	fmt.Println("stub daemon: asked to stop")
	_ = discovery.RemoveIfOwnedBy(discoveryPath(), os.Getpid())
	fmt.Println("stub daemon: stopped")
	return nil
}

// playTheGateway records that it was started and then behaves as the test
// asked: it cannot be configured, fails, panics, or just runs. Stopping takes
// a moment, so that a daemon which was stopped at the same time as it, and not
// after it, can be told apart.
func playTheGateway(ctx context.Context) error {
	file, err := os.OpenFile(os.Getenv("SUPERVISE_TEST_MARK"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, _ = file.WriteString("started\n")
	_ = file.Close()
	starts := startsOf(os.Getenv("SUPERVISE_TEST_MARK"))

	switch os.Getenv("SUPERVISE_TEST_GATEWAY") {
	case "config":
		return gateway.Unconfigured(errors.New(os.Getenv("SUPERVISE_TEST_SAY")))
	case "fail":
		if starts <= 3 {
			return errors.New("the connection loop gave up")
		}
	case "panic":
		if starts == 1 {
			panic(errors.New("Post https://api.telegram.org/botSECRET-TOKEN-123/getUpdates: refused"))
		}
	}

	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	fmt.Println("stub gateway: stopped")
	return nil
}

func startsOf(mark string) int {
	data, _ := os.ReadFile(mark)
	return strings.Count(string(data), "started")
}

// agentSays is a daemon's ConfigService that answers one question.
type agentSays struct {
	controlv1connect.UnimplementedConfigServiceHandler
	problems []*controlv1.ConfigProblem
}

func (a agentSays) GetAgentState(
	context.Context, *connect.Request[controlv1.GetAgentStateRequest],
) (*connect.Response[controlv1.GetAgentStateResponse], error) {
	return connect.NewResponse(&controlv1.GetAgentStateResponse{Mode: "repair", Problems: a.problems}), nil
}

// aDeployment is a place for the stub parts to publish and to leave a mark in,
// and a daemon's address for them to publish.
type aDeployment struct {
	mark string
}

func newDeployment(t *testing.T, mode string, problems ...*controlv1.ConfigProblem) aDeployment {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewConfigServiceHandler(agentSays{problems: problems}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mark := filepath.Join(t.TempDir(), "gateway-starts")

	t.Setenv(stubEnv, "1")
	t.Setenv("JINGCLAW_RUNTIME_DIR", t.TempDir())
	t.Setenv(home.EnvVar, t.TempDir())
	t.Setenv("SUPERVISE_TEST_URL", server.URL)
	t.Setenv("SUPERVISE_TEST_MODE", mode)
	t.Setenv("SUPERVISE_TEST_MARK", mark)

	return aDeployment{mark: mark}
}

func (d aDeployment) gatewayStarts() int { return startsOf(d.mark) }

func (d aDeployment) gatewayWasStarted() bool { return d.gatewayStarts() > 0 }

func published() bool {
	_, err := os.Stat(discoveryPath())
	return err == nil
}

// lockedBuffer is what a run's output goes to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// aRun is the program's run going in a process of its own.
type aRun struct {
	t       *testing.T
	command *exec.Cmd
	said    *lockedBuffer
	ended   chan error
	over    bool
}

// running starts the program with no terminal, so that it writes where a
// service would capture it, and returns before it has done anything.
func running(t *testing.T) *aRun {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find this binary: %v", err)
	}

	r := &aRun{t: t, said: &lockedBuffer{}, ended: make(chan error, 1)}
	r.command = exec.Command(self, "run")
	r.command.Stdout = r.said
	r.command.Stderr = r.said
	if err := r.command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { r.ended <- r.command.Wait() }()

	t.Cleanup(func() {
		if !r.over {
			_ = r.command.Process.Kill()
			<-r.ended
		}
	})
	return r
}

// stillGoingAfter says whether the run has not ended of its own accord by the
// time the wait is over.
func (r *aRun) stillGoingAfter(wait time.Duration) bool {
	r.t.Helper()

	select {
	case err := <-r.ended:
		r.over = true
		r.t.Errorf("the run ended on its own (%v):\n%s", err, r.said)
		return false
	case <-time.After(wait):
		return true
	}
}

// interrupt ends it the way a terminal would, and returns what it printed.
func (r *aRun) interrupt() string {
	r.t.Helper()

	if err := r.command.Process.Signal(syscall.SIGTERM); err != nil {
		r.t.Fatalf("interrupt: %v", err)
	}
	return r.waitUntilItEnds()
}

func (r *aRun) waitUntilItEnds() string {
	r.t.Helper()

	select {
	case err := <-r.ended:
		r.over = true
		if err != nil {
			r.t.Errorf("an interrupted run did not end cleanly (%v):\n%s", err, r.said)
		}
	case <-time.After(20 * time.Second):
		r.t.Fatalf("the run did not end when interrupted:\n%s", r.said)
	}
	return r.said.String()
}

// A daemon that came up with no agent gets no gateway and no console, and the
// run says why and stays until it is stopped.
//
// The gateway has no ingress to reach in that state, and a supervisor that
// started it anyway would produce a process that fails over and over for a
// reason the operator cannot see from the terminal. Ending the run would be
// worse: the dashboard that fixes it would go with it.
func TestARepairingDaemonGetsNoGatewayAndTheRunStaysUp(t *testing.T) {
	d := newDeployment(t, discovery.ModeRepair,
		problem("gateway.platform", `"slack"`, "is not a platform"))
	r := running(t)

	// Long enough for the gateway to have been started if it was going to be:
	// the daemon is ready within a tick, and the next thing Run does is start it.
	if !r.stillGoingAfter(1500 * time.Millisecond) {
		return
	}
	if d.gatewayWasStarted() {
		t.Error("a gateway was started against a daemon with no agent")
	}

	printed := r.interrupt()
	for _, want := range []string{
		`agent not running: gateway.platform = "slack": is not a platform`,
		"run: jingclaw dashboard",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("it did not say %q:\n%s", want, printed)
		}
	}
}

// A run that is told to stop stops the gateway first and the daemon after it,
// and the daemon takes its discovery file with it.
//
// The gateway is a client of the daemon: stopped the other way round it spends
// its last moments failing against something that has gone. The stub gateway
// takes a moment to stop, so a daemon stopped at the same time as it, rather
// than after, would print first.
func TestStoppingStopsTheGatewayThenTheDaemonAndRemovesTheDiscoveryFile(t *testing.T) {
	d := newDeployment(t, "")
	r := running(t)

	waitFor(t, d.gatewayWasStarted, "the gateway to be started")
	if !published() {
		t.Fatal("the gateway was started before the daemon had published itself")
	}
	if !r.stillGoingAfter(500 * time.Millisecond) {
		return
	}

	printed := r.interrupt()

	gatewayStopped := strings.Index(printed, "stub gateway: stopped")
	daemonAsked := strings.Index(printed, "stub daemon: asked to stop")
	if gatewayStopped < 0 || daemonAsked < 0 {
		t.Fatalf("one of the two never stopped:\n%s", printed)
	}
	if daemonAsked < gatewayStopped {
		t.Errorf("the daemon was asked to stop before the gateway had stopped:\n%s", printed)
	}
	if published() {
		t.Error("the discovery file was left behind")
	}
	if !strings.Contains(printed, "JingClaw is running") {
		t.Errorf("it never said it was running:\n%s", printed)
	}
}

// A gateway that cannot be configured is said, left off and not tried again,
// and the daemon stays up. The daemon and the dashboard are what the person
// fixing it needs.
func TestAGatewayThatCannotBeConfiguredLeavesTheRunUpAndIsNotRetried(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_GATEWAY", "config")
	t.Setenv("SUPERVISE_TEST_SAY", "the discord bot token: no credential: set DISCORD_BOT_TOKEN")
	r := running(t)

	waitFor(t, d.gatewayWasStarted, "the gateway to be started")

	// Long enough for it to have been tried again, at the pace this run is
	// given, many times over.
	if !r.stillGoingAfter(1500 * time.Millisecond) {
		return
	}
	if starts := d.gatewayStarts(); starts != 1 {
		t.Errorf("a configuration failure was tried %d times", starts)
	}
	if !published() {
		t.Error("the daemon went away with the gateway")
	}

	printed := r.interrupt()
	if !strings.Contains(printed,
		"chat is off: the discord bot token: no credential: set DISCORD_BOT_TOKEN"+
			" (put that right, then restart: jingclaw stop, then jingclaw)") {
		t.Errorf("it did not say chat is off, and why:\n%s", printed)
	}
}

// Any other failure is logged and the gateway started again, and the process
// and the daemon carry on. Today it ended the supervisor; a crash-looping bot
// taking the dashboard down with it is the wrong way round.
func TestAGatewayThatFailsIsStartedAgainAndTheRunCarriesOn(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_GATEWAY", "fail")
	r := running(t)

	waitFor(t, func() bool { return d.gatewayStarts() >= 4 }, "the gateway to be started four times")
	if !r.stillGoingAfter(500 * time.Millisecond) {
		return
	}
	if !published() {
		t.Error("the daemon went away while the gateway was failing")
	}

	printed := r.interrupt()
	for _, want := range []string{"starting it again", "the connection loop gave up", "chat stopped unexpectedly"} {
		if !strings.Contains(printed, want) {
			t.Errorf("it did not say %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "chat is off") {
		t.Errorf("a failure that is retried was reported as chat being switched off:\n%s", printed)
	}
	if n := strings.Count(printed, "chat stopped unexpectedly"); n != 1 {
		t.Errorf("the notice was given %d times in one streak of failures", n)
	}
}

// A panic in the gateway is contained: the run goes on, the gateway is started
// again, the panic is logged, and what it panicked with is not, because a
// platform library's errors quote the URL of the request and the URL holds the
// bot token.
func TestAPanicInTheGatewayIsContainedAndLoggedWithoutItsMessage(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_GATEWAY", "panic")
	r := running(t)

	waitFor(t, func() bool { return d.gatewayStarts() >= 2 }, "the gateway to be started again after its panic")
	if !r.stillGoingAfter(500 * time.Millisecond) {
		return
	}
	if !published() {
		t.Error("the daemon went away with the gateway's panic")
	}

	printed := r.interrupt()
	if !strings.Contains(printed, "the gateway panicked") {
		t.Errorf("the panic was not logged:\n%s", printed)
	}
	if strings.Contains(printed, "SECRET-TOKEN-123") {
		t.Errorf("the panic's message, which holds a token, was logged:\n%s", printed)
	}
}

// A second run finds the first and attaches to it rather than starting
// another, and leaving it leaves the first one running.
func TestASecondRunAttachesAndLeavingItLeavesTheFirstRunning(t *testing.T) {
	d := newDeployment(t, "")
	first := running(t)
	waitFor(t, d.gatewayWasStarted, "the first run to be up")

	second := running(t)
	waitFor(t, func() bool { return strings.Contains(second.said.String(), "already running") },
		"the second run to find the first")
	if !second.stillGoingAfter(500 * time.Millisecond) {
		return
	}
	if n := d.gatewayStarts(); n != 1 {
		t.Errorf("the second run started its own gateway (%d starts)", n)
	}

	second.interrupt()
	if !first.stillGoingAfter(500*time.Millisecond) || !published() {
		t.Error("leaving the second run stopped the first")
	}

	// And stop, which finds the first by what it published, ends it.
	if err := stopRunning(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-first.ended:
		first.over = true
		if err != nil {
			t.Errorf("the run that was stopped did not end cleanly (%v):\n%s", err, first.said)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("stop did not end the run:\n%s", first.said)
	}
	if published() {
		t.Error("the discovery file was left behind by a stop")
	}
}

// A daemon that returns by itself ends the run, with what it said.
func TestADaemonThatEndsEndsTheRun(t *testing.T) {
	// A deployment of its own: this run looks for one already running first.
	t.Setenv("JINGCLAW_RUNTIME_DIR", t.TempDir())
	t.Setenv(home.EnvVar, t.TempDir())
	err := run(context.Background(), parts{
		daemon: func(context.Context) error { return errors.New("the database is locked") },
		gateway: func(context.Context) error {
			t.Error("a gateway was started against a daemon that never came up")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "the database is locked") {
		t.Errorf("the run ended with %v", err)
	}
}

// A daemon found already running in repair is not watched with a console either.
//
// The console follows an agent's sessions, and there are none; it would open on
// a daemon that answers every one of its requests with not found. What there is
// to see is why.
func TestAttachingToARepairingDaemonSaysWhyInsteadOfDrawingAConsole(t *testing.T) {
	newDeployment(t, discovery.ModeRepair, problem("", "", "no credential: set GEMINI_API_KEY"))

	// Published by hand, as a daemon started by somebody else would have, with
	// this very process as its pid so that it reads as alive.
	err := discovery.Write(filepath.Join(os.Getenv("JINGCLAW_RUNTIME_DIR"), "daemon.json"), discovery.File{
		PID: os.Getpid(), BaseURL: os.Getenv("SUPERVISE_TEST_URL"), Token: "control",
		Mode: discovery.ModeRepair, ProtocolVersion: discovery.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	r := running(t)
	if !r.stillGoingAfter(1500 * time.Millisecond) {
		return
	}

	printed := r.interrupt()
	for _, want := range []string{
		"already running",
		"agent not running: no credential: set GEMINI_API_KEY",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("it did not say %q:\n%s", want, printed)
		}
	}
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
