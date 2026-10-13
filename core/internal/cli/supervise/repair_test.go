package supervise

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

// A gateway that cannot start for a reason in the settings or the credential
// is the one exit that does not end the run.
//
// The daemon and the dashboard are what somebody puts that right with, so
// ending the run over a missing token would take away the thing that fixes it.
// Every other way of stopping is a fault, and a supervisor that carried on
// without a part that crashed would be one that hides crashes.
func TestOnlyAConfigurationExitLeavesChatOffWithoutEndingTheRun(t *testing.T) {
	cases := []struct {
		status int
		ends   bool
	}{
		{status: 0, ends: true},
		{status: 1, ends: true},
		{status: 2, ends: true},
		{status: 77, ends: true},
		{status: 79, ends: true},
		// Killed by a signal: Go reports -1 and no status of its own.
		{status: -1, ends: true},
		{status: gateway.ExitConfiguration, ends: false},
	}
	for _, one := range cases {
		if got := chatEndsTheRun(one.status); got != one.ends {
			t.Errorf("a gateway that exited with %d: ends the run is %v, want %v", one.status, got, one.ends)
		}
	}
}

// The status is a contract with whatever starts the gateway by itself, so it is
// written down here as the number it is rather than as the name for it.
func TestTheConfigurationStatusIsTheOneSysexitsCallsConfig(t *testing.T) {
	if gateway.ExitConfiguration != 78 {
		t.Errorf("the gateway's configuration status is %d, want 78", gateway.ExitConfiguration)
	}
}

func problem(key, value, why string) *controlv1.ConfigProblem {
	return &controlv1.ConfigProblem{Key: key, Value: value, Why: why}
}

// The line says there is no agent, why, and what to run about it, with the first
// problem and how many more there are rather than all of them.
func TestTheLineThatSaysThereIsNoAgentNamesTheFirstProblemAndWhatToRun(t *testing.T) {
	state := &controlv1.GetAgentStateResponse{
		Mode: "repair",
		Problems: []*controlv1.ConfigProblem{
			problem("provider.backend", `"gemini"`, "needs a key and none was found"),
			problem("gateway.platform", `"slack"`, "is not a platform"),
		},
	}

	said := agentNotRunning(state, nil)

	for _, want := range []string{
		"agent not running:",
		`provider.backend = "gemini": needs a key and none was found`,
		"(and 1 more)",
		"run: jingclaw dashboard",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the line does not say %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "slack") {
		t.Errorf("the second problem is in the line as well as the first:\n%s", said)
	}
	if strings.Contains(said, "\n") {
		t.Errorf("it is not one line:\n%s", said)
	}
}

// A problem that is not about a setting has no key to lead with, and a message
// that spans lines is still one line.
func TestAStartupProblemIsSaidWithoutAKeyAndOnOneLine(t *testing.T) {
	state := &controlv1.GetAgentStateResponse{
		Mode:     "repair",
		Problems: []*controlv1.ConfigProblem{problem("", "", "no credential:\n  set GEMINI_API_KEY")},
	}

	said := agentNotRunning(state, nil)

	if want := "agent not running: no credential: set GEMINI_API_KEY — run: jingclaw dashboard"; said != want {
		t.Errorf("said\n  %q\nwant\n  %q", said, want)
	}
}

// Not being able to ask is said in place of why, and the pointer to the
// dashboard is still there, because that is the part that helps.
func TestNotBeingAbleToAskWhyStillPointsAtTheDashboard(t *testing.T) {
	said := agentNotRunning(nil, errors.New("connection refused"))

	if !strings.Contains(said, "connection refused") || !strings.Contains(said, "run: jingclaw dashboard") {
		t.Errorf("said %q", said)
	}

	if said := agentNotRunning(&controlv1.GetAgentStateResponse{Mode: "repair"}, nil); said !=
		"agent not running — run: jingclaw dashboard" {
		t.Errorf("a daemon with no reason to give said %q", said)
	}
}

// What a gateway said is quoted without the prefix this program puts in front
// of it, and from the last line that has one, because the reason is what it
// printed on the way out.
func TestWhatTheGatewaySaidIsQuotedWithoutItsErrorPrefix(t *testing.T) {
	said := &lastWords{limit: 500}
	fmt.Fprintln(said, `{"level":"INFO","msg":"something earlier"}`)
	fmt.Fprintln(said, "error: the discord bot token: no credential: set DISCORD_BOT_TOKEN")

	if got, want := reasonOf(said), "the discord bot token: no credential: set DISCORD_BOT_TOKEN"; got != want {
		t.Errorf("the reason is %q, want %q", got, want)
	}

	// With no such line, everything it said: a gateway that exited with the
	// status and printed something else has still told us something.
	other := &lastWords{limit: 500}
	fmt.Fprint(other, "something that is not an error line")
	if got := reasonOf(other); got != "something that is not an error line" {
		t.Errorf("a gateway that printed no error line was quoted as %q", got)
	}

	if got := reasonOf(&lastWords{limit: 10}); !strings.Contains(got, "said nothing") {
		t.Errorf("a gateway that said nothing was quoted as %q", got)
	}
}

// The mode is what the daemon published, and a daemon that published none is one
// running an agent, which is every daemon there was before there was a mode.
func TestTheModeIsReadFromWhatTheDaemonPublished(t *testing.T) {
	runtime := t.TempDir()
	t.Setenv("JINGCLAW_RUNTIME_DIR", runtime)

	if mode := publishedMode(); mode != "" {
		t.Errorf("with nothing published the mode is %q", mode)
	}

	publish := func(mode string) {
		t.Helper()
		err := discovery.Write(filepath.Join(runtime, "daemon.json"), discovery.File{
			PID: 1, BaseURL: "http://127.0.0.1:1", Token: "t", Mode: mode,
			ProtocolVersion: discovery.ProtocolVersion,
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	publish("")
	if mode := publishedMode(); mode != "" {
		t.Errorf("a daemon that published no mode reads as %q", mode)
	}

	publish(discovery.ModeRepair)
	if mode := publishedMode(); mode != discovery.ModeRepair {
		t.Errorf("a daemon in repair reads as %q", mode)
	}
}

// `status` shows the mode, and says in words that a repair daemon has no agent.
func TestStatusShowsTheMode(t *testing.T) {
	if got := describeMode(""); got != "running" {
		t.Errorf("a daemon running an agent is described as %q", got)
	}
	repair := describeMode(discovery.ModeRepair)
	if !strings.HasPrefix(repair, "repair") || !strings.Contains(repair, "no agent") ||
		!strings.Contains(repair, "jingclaw dashboard") {
		t.Errorf("a repair daemon is described as %q", repair)
	}
}
