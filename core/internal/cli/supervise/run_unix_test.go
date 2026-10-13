//go:build !windows

package supervise

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
// with "daemon", "gateway" or "run" as its first argument it plays that part
// instead of running the tests. Run starts its parts from os.Executable, which
// in a test is this binary, so this is what lets the real Run be driven end to
// end, in a process of its own, without a real daemon.
const stubEnv = "SUPERVISE_TEST_STUB"

func TestMain(m *testing.M) {
	// The watchdog is this same binary asked to clean up, as it is in the real
	// program.
	WatchIfAsked()

	if os.Getenv(stubEnv) != "" && len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			playTheDaemon()
		case "gateway":
			playTheGateway()
		case "run":
			playTheProgram()
		}
	}
	os.Exit(m.Run())
}

// playTheProgram is what the program's own main does with Run.
func playTheProgram() {
	if err := Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// playTheDaemon publishes itself the way a daemon does, in the mode the test
// asked for, and waits to be stopped.
func playTheDaemon() {
	err := discovery.Write(filepath.Join(os.Getenv("JINGCLAW_RUNTIME_DIR"), "daemon.json"), discovery.File{
		PID:             os.Getpid(),
		BaseURL:         os.Getenv("SUPERVISE_TEST_URL"),
		Token:           "control",
		GatewayToken:    "gateway",
		Mode:            os.Getenv("SUPERVISE_TEST_MODE"),
		ProtocolVersion: discovery.ProtocolVersion,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub daemon:", err)
		os.Exit(2)
	}
	select {}
}

// playTheGateway records that it was started, says why it is stopping, and
// exits with the status the test asked for.
func playTheGateway() {
	_ = os.WriteFile(os.Getenv("SUPERVISE_TEST_MARK"), nil, 0o600)
	fmt.Fprintln(os.Stderr, "error: "+os.Getenv("SUPERVISE_TEST_SAY"))

	status, _ := strconv.Atoi(os.Getenv("SUPERVISE_TEST_EXIT"))
	os.Exit(status)
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

	mark := filepath.Join(t.TempDir(), "gateway-was-started")

	t.Setenv(stubEnv, "1")
	t.Setenv("JINGCLAW_RUNTIME_DIR", t.TempDir())
	t.Setenv(home.EnvVar, t.TempDir())
	t.Setenv("SUPERVISE_TEST_URL", server.URL)
	t.Setenv("SUPERVISE_TEST_MODE", mode)
	t.Setenv("SUPERVISE_TEST_MARK", mark)

	return aDeployment{mark: mark}
}

func (d aDeployment) gatewayWasStarted() bool {
	_, err := os.Stat(d.mark)
	return err == nil
}

// aRun is the program's Run going in a process of its own.
type aRun struct {
	t       *testing.T
	command *exec.Cmd
	said    *bytes.Buffer
	ended   chan error
	over    bool
}

// running starts Run with no terminal, so that it writes where a service would
// capture it, and returns before it has done anything.
func running(t *testing.T) *aRun {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find this binary: %v", err)
	}

	r := &aRun{t: t, said: &bytes.Buffer{}, ended: make(chan error, 1)}
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

// The same harness with a daemon that has an agent does start the gateway,
// which is what makes the test above mean something.
func TestADaemonWithAnAgentGetsAGateway(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_EXIT", "0")
	t.Setenv("SUPERVISE_TEST_SAY", "anything")
	r := running(t)

	waitFor(t, d.gatewayWasStarted, "the gateway to be started")

	// It exits straight away, which ends the run: nothing to wait for but that.
	select {
	case <-r.ended:
		r.over = true
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not end after the gateway did")
	}
}

// A gateway that exits because its settings or its token are wrong is said, and
// left off. The run stays up, because the daemon and the dashboard are still
// what the person fixing it needs.
func TestAGatewayThatCannotBeConfiguredLeavesTheRunUp(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_EXIT", strconv.Itoa(gateway.ExitConfiguration))
	t.Setenv("SUPERVISE_TEST_SAY", "the discord bot token: no credential: set DISCORD_BOT_TOKEN")
	r := running(t)

	waitFor(t, d.gatewayWasStarted, "the gateway to be started")

	// And then long enough for the run to have ended if it was going to.
	if !r.stillGoingAfter(1500 * time.Millisecond) {
		return
	}

	printed := r.interrupt()
	if !strings.Contains(printed, "chat is off: the discord bot token: no credential: set DISCORD_BOT_TOKEN") {
		t.Errorf("it did not say chat is off, and why:\n%s", printed)
	}
}

// Any other exit is a fault, and ends the run as it always has.
func TestAnyOtherGatewayExitStillEndsTheRun(t *testing.T) {
	d := newDeployment(t, "")
	t.Setenv("SUPERVISE_TEST_EXIT", "1")
	t.Setenv("SUPERVISE_TEST_SAY", "the connection loop panicked")
	r := running(t)

	select {
	case err := <-r.ended:
		r.over = true
		if err == nil {
			t.Errorf("a gateway that crashed ended the run cleanly:\n%s", r.said)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a gateway that crashed did not end the run")
	}

	if !d.gatewayWasStarted() {
		t.Error("the gateway was never started, so this proved nothing")
	}
	printed := r.said.String()
	for _, want := range []string{"the gateway stopped", "the connection loop panicked"} {
		if !strings.Contains(printed, want) {
			t.Errorf("it did not say %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "chat is off") {
		t.Errorf("a crash was reported as chat being switched off:\n%s", printed)
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
