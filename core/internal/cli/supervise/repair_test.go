package supervise

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/internal/cli/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
)

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

// A discovery file that names this very process is a leftover from an earlier
// one with the same pid, and is not a daemon that is up; one that names another
// process is not this one's to remove.
func TestALeftoverFileNamingThisProcessIsForgotten(t *testing.T) {
	runtime := t.TempDir()
	t.Setenv("JINGCLAW_RUNTIME_DIR", runtime)
	path := filepath.Join(runtime, "daemon.json")

	publish := func(pid int) {
		t.Helper()
		err := discovery.Write(path, discovery.File{
			PID: pid, BaseURL: "http://127.0.0.1:1", Token: "t", ProtocolVersion: discovery.ProtocolVersion,
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	publish(os.Getpid())
	if running, _ := alreadyRunning(); !running {
		t.Fatal("the leftover does not read as running, so this proves nothing")
	}
	forgetAFileNamingThisProcess()
	if running, _ := alreadyRunning(); running {
		t.Error("a file naming this process still reads as a daemon that is up")
	}

	publish(os.Getppid())
	forgetAFileNamingThisProcess()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("another process's file was removed: %v", err)
	}
}
