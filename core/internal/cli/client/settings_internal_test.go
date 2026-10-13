package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// theDaemon answers the two services the client commands in this file ask of
// it: the settings, and the page.
type theDaemon struct {
	controlv1connect.UnimplementedConfigServiceHandler
	controlv1connect.UnimplementedBrowserServiceHandler

	text       string
	parseError string
	state      *controlv1.GetAgentStateResponse
	stateError error
}

func (d *theDaemon) GetConfig(
	context.Context, *connect.Request[controlv1.GetConfigRequest],
) (*connect.Response[controlv1.GetConfigResponse], error) {
	return connect.NewResponse(&controlv1.GetConfigResponse{Text: d.text, ParseError: d.parseError}), nil
}

func (d *theDaemon) GetAgentState(
	context.Context, *connect.Request[controlv1.GetAgentStateRequest],
) (*connect.Response[controlv1.GetAgentStateResponse], error) {
	if d.stateError != nil {
		return nil, d.stateError
	}
	return connect.NewResponse(d.state), nil
}

func (d *theDaemon) CreatePairing(
	context.Context, *connect.Request[controlv1.CreatePairingRequest],
) (*connect.Response[controlv1.CreatePairingResponse], error) {
	return connect.NewResponse(&controlv1.CreatePairingResponse{
		Code: "one-time-code", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
	}), nil
}

// publishedBy starts the daemon, publishes it where a client looks for it, and
// leaves the client pointed there.
func publishedBy(t *testing.T, daemon *theDaemon) {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewConfigServiceHandler(daemon))
	mux.Handle(controlv1connect.NewBrowserServiceHandler(daemon))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	err := discovery.Write(filepath.Join(dir, "daemon.json"), discovery.File{
		PID: os.Getpid(), BaseURL: server.URL, Token: "control", ProtocolVersion: discovery.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Where the sign-in sessions go, which these never reach but the commands
	// open.
	t.Setenv(home.EnvVar, t.TempDir())

	was := runtimeDir
	runtimeDir = dir
	t.Cleanup(func() { runtimeDir = was })
}

const tools = `
[[mcp.servers]]
name = "docs"
url = "https://docs.example.test/mcp"
oauth = true

[[mcp.servers]]
name = "files"
command = "files-server"
`

// The tool servers are the ones the running daemon has saved, not the ones in a
// file this process finds: there is no file.
func TestTheToolServersAreTheOnesTheRunningDaemonHasSaved(t *testing.T) {
	publishedBy(t, &theDaemon{text: tools})

	servers, err := configuredServers(context.Background())
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(servers) != 2 || servers[0].Name != "docs" || servers[1].Name != "files" {
		t.Fatalf("the servers are %+v", servers)
	}

	server, _, err := serverNamed(context.Background(), "docs")
	if err != nil {
		t.Fatalf("find one: %v", err)
	}
	if server.URL != "https://docs.example.test/mcp" || !server.OAuth {
		t.Errorf("the server is %+v", server)
	}
}

// A typo is answered with the names that exist, and a server that does not
// sign in is told so, as they were when the list came from a file.
func TestAWrongServerNameIsAnsweredWithTheRightOnes(t *testing.T) {
	publishedBy(t, &theDaemon{text: tools})

	_, _, err := serverNamed(context.Background(), "dcos")
	if err == nil || !strings.Contains(err.Error(), `"dcos"`) ||
		!strings.Contains(err.Error(), "docs") || !strings.Contains(err.Error(), "files") {
		t.Errorf("a typo was answered with %v", err)
	}

	_, _, err = serverNamed(context.Background(), "files")
	if err == nil || !strings.Contains(err.Error(), "does not authorize with oauth") {
		t.Errorf("a server that does not sign in was answered with %v", err)
	}
}

// Without a daemon there is nothing to ask, and the error says so instead of
// leaving the reader to decode a missing file.
func TestWithoutADaemonTheToolServersAreAskedForAfterStartingIt(t *testing.T) {
	was := runtimeDir
	runtimeDir = t.TempDir()
	t.Cleanup(func() { runtimeDir = was })

	_, err := configuredServers(context.Background())

	if err == nil || !strings.Contains(err.Error(), "start it first") ||
		!strings.Contains(err.Error(), "is jingclaw running?") {
		t.Errorf("no daemon gave %v", err)
	}
}

// A daemon that is up but cannot read its saved settings answers with the text
// and the reason. There is no list to give from that, and the way to the repair
// is named.
func TestSavedSettingsThatCannotBeReadAreSaidWithTheWayToFixThem(t *testing.T) {
	publishedBy(t, &theDaemon{text: "this is = not [toml", parseError: "line 1: expected a table"})

	_, err := configuredServers(context.Background())

	if err == nil || !strings.Contains(err.Error(), "expected a table") ||
		!strings.Contains(err.Error(), "jingclaw dashboard") {
		t.Errorf("unreadable settings gave %v", err)
	}
}

// The command itself, end to end: `mcp list` through the real command tree, in
// an environment that names where the daemon is, against a daemon that has the
// servers.
func TestMCPListAsksTheDaemonWhereverTheEnvironmentSaysItIs(t *testing.T) {
	publishedBy(t, &theDaemon{text: tools})
	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", runtimeDir)
	runtimeDir = ""

	parent := &cobra.Command{Use: "jingclaw", SilenceUsage: true, SilenceErrors: true}
	AddTo(parent)
	parent.SetArgs([]string{"mcp", "list"})

	printed := capture(t, func() {
		if err := parent.Execute(); err != nil {
			t.Errorf("mcp list: %v", err)
		}
	})

	for _, want := range []string{
		"docs", "not signed in — run: jingclaw mcp login docs",
		"files", "runs as a command",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("the list does not say %q:\n%s", want, printed)
		}
	}
}

// Where the daemon is comes from the defaults and the environment, and from
// nothing a client could be pointed at: there is no file to name.
func TestWhereTheDaemonIsComesFromTheEnvironmentAndThereIsNoConfigFlag(t *testing.T) {
	was := runtimeDir
	t.Cleanup(func() { runtimeDir = was })

	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", "/somewhere/the/daemon/publishes")

	root := newRootCommand()
	if err := root.PersistentPreRunE(root, nil); err != nil {
		t.Fatalf("pre-run: %v", err)
	}
	if runtimeDir != "/somewhere/the/daemon/publishes" {
		t.Errorf("the runtime directory is %q", runtimeDir)
	}

	if root.PersistentFlags().Lookup("config") != nil {
		t.Error("a client still offers --config")
	}

	parent := &cobra.Command{Use: "jingclaw", SilenceUsage: true, SilenceErrors: true}
	AddTo(parent)
	parent.SetArgs([]string{"session", "list", "--config", "/etc/jingclaw/config.toml"})
	if err := parent.Execute(); err == nil || !strings.Contains(err.Error(), "config") {
		t.Errorf("--config was accepted: %v", err)
	}
}

// A variable that cannot be read stops a client command, as it stopped one that
// read the file: this is where the daemon is looked for, and guessing would
// send the command to somewhere else.
func TestAnEnvironmentThatCannotBeReadStopsAClientCommand(t *testing.T) {
	t.Setenv("JINGCLAW_AGENT_MAX_ITERATIONS", "not-a-number")

	parent := &cobra.Command{Use: "jingclaw", SilenceUsage: true, SilenceErrors: true}
	AddTo(parent)
	parent.SetArgs([]string{"session", "list"})

	if err := parent.Execute(); err == nil || !strings.Contains(err.Error(), "max_iterations") {
		t.Errorf("an unreadable environment gave %v", err)
	}
}

// With no agent, the dashboard command says what is wrong before it hands over
// the link, and on the error stream, so the link on its own is still what a
// script reads.
func TestTheDashboardSaysWhatIsWrongWhenThereIsNoAgent(t *testing.T) {
	publishedBy(t, &theDaemon{state: &controlv1.GetAgentStateResponse{
		Mode: "repair",
		Problems: []*controlv1.ConfigProblem{
			{Key: "gateway.platform", Value: `"slack"`, Why: "is not a platform", Fix: "use discord or telegram"},
			{Why: "no credential:\n  set GEMINI_API_KEY", Fix: "put that right"},
		},
	}})

	out, errOut := runDashboard(t)

	if !strings.HasPrefix(out, "http://") || !strings.Contains(out, "/ui/#pair=one-time-code") ||
		strings.Count(out, "\n") != 1 {
		t.Errorf("what a script reads is not the link alone:\n%q", out)
	}
	for _, want := range []string{
		"The agent is not running",
		`gateway.platform = "slack": is not a platform`, "use discord or telegram",
		"no credential: set GEMINI_API_KEY", "put that right",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("it does not say %q:\n%s", want, errOut)
		}
	}
}

// With an agent there is nothing to add, and the link is all there is.
func TestTheDashboardSaysNothingExtraWhenTheAgentIsRunning(t *testing.T) {
	publishedBy(t, &theDaemon{state: &controlv1.GetAgentStateResponse{Mode: "running"}})

	out, errOut := runDashboard(t)

	if !strings.Contains(out, "/ui/#pair=one-time-code") {
		t.Errorf("no link: %q", out)
	}
	if strings.TrimSpace(errOut) != "" {
		t.Errorf("a daemon with an agent had something to say:\n%s", errOut)
	}
}

// Not being able to ask does not stop the link: the dashboard is how to find
// out more.
func TestTheDashboardStillGivesTheLinkWhenTheAgentStateCannotBeAsked(t *testing.T) {
	publishedBy(t, &theDaemon{stateError: connect.NewError(connect.CodeUnimplemented, errors.New("not here"))})

	out, errOut := runDashboard(t)

	if !strings.Contains(out, "/ui/#pair=one-time-code") {
		t.Errorf("no link: %q", out)
	}
	if !strings.Contains(errOut, "Could not ask whether the agent is running") {
		t.Errorf("it did not say it could not ask:\n%s", errOut)
	}
}

func runDashboard(t *testing.T) (out, errOut string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	command := newDashboardCommand()
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--print"})
	command.SetContext(context.Background())

	if err := command.Execute(); err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	return stdout.String(), stderr.String()
}

// capture returns what run wrote to standard output.
//
// The subcommands print their answers to the real stream rather than to a
// writer the command carries, so a test that wants to read one has to take the
// stream itself.
func capture(t *testing.T, run func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	was := os.Stdout
	os.Stdout = write

	done := make(chan string, 1)
	go func() {
		printed, _ := io.ReadAll(read)
		done <- string(printed)
	}()

	run()

	os.Stdout = was
	if err := write.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return <-done
}
