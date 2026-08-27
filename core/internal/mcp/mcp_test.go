package mcp_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/mcp"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

func asToolError(err error, into **tool.Error) bool {
	return errors.As(err, into)
}

// helperConfig points a server config at this test binary running as a server.
func helperConfig(name string) mcp.ServerConfig {
	return mcp.ServerConfig{
		Name:    name,
		Command: os.Args[0],
		Env:     map[string]string{helperEnv: "1"},
		Level:   tool.LevelWorkspaceRead,
	}
}

func connect(t *testing.T, cfg mcp.ServerConfig, limits mcp.Limits) *mcp.Server {
	t.Helper()
	return connectWithStore(t, cfg, limits, nil)
}

func connectWithStore(
	t *testing.T,
	cfg mcp.ServerConfig,
	limits mcp.Limits,
	artifacts *artifact.Store,
) *mcp.Server {
	t.Helper()

	server, err := mcp.Connect(context.Background(), cfg, limits, artifacts,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	return server
}

func toolNamed(t *testing.T, server *mcp.Server, name string) tool.Tool {
	t.Helper()

	for _, offered := range server.Tools() {
		if offered.Spec().Name == name {
			return offered
		}
	}

	var available []string
	for _, offered := range server.Tools() {
		available = append(available, offered.Spec().Name)
	}
	t.Fatalf("no tool named %s; the server offered %v", name, available)
	return nil
}

// The whole milestone in one assertion: an external tool wears the same
// interface as a built-in one, so it needs no second registry, no second
// permission path and no second result shape.
func TestAToolOnAServerIsJustATool(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{})

	echo := toolNamed(t, server, "mcp_helper_echo")

	result, err := echo.Execute(context.Background(), tool.Call{
		ID:        "call_1",
		Name:      "mcp_helper_echo",
		Arguments: arguments(t, map[string]any{"text": "測試"}),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(result.Content, "you said: 測試") {
		t.Errorf("the arguments did not round trip: %q", result.Content)
	}
	if result.IsError {
		t.Error("a successful call came back marked as an error")
	}
}

// A server saying its tool is harmless must not make it harmless. The policy
// engine reads Level and Capabilities, so a server that could set them could
// walk past the approval a truthful one would have to stop for.
func TestTheServerDoesNotGetToSayHowDangerousItIs(t *testing.T) {
	cfg := helperConfig("helper")
	cfg.Level = tool.LevelHighImpact

	server := connect(t, cfg, mcp.Limits{})
	spec := toolNamed(t, server, "mcp_helper_echo").Spec()

	if spec.Level != tool.LevelHighImpact {
		t.Errorf("level is %s, want the one this machine configured", spec.Level)
	}

	// Assumed rather than asked. What runs behind the call is another program,
	// and claiming otherwise is a guess in the direction that costs most.
	for name, granted := range map[string]bool{
		"execute":     spec.Capabilities.Execute,
		"network":     spec.Capabilities.Network,
		"destructive": spec.Capabilities.Destructive,
	} {
		if !granted {
			t.Errorf("an external tool is not marked %s", name)
		}
	}
	if spec.Capabilities.Idempotent || spec.Capabilities.ParallelSafe {
		t.Error("an external tool was assumed safe to repeat or to run concurrently")
	}
}

// Installing a server must never change what read_file means.
func TestToolNamesAreNamespaced(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{})

	for _, offered := range server.Tools() {
		name := offered.Spec().Name
		if !strings.HasPrefix(name, "mcp_helper_") {
			t.Errorf("%q is not namespaced to its server", name)
		}
	}
}

// A name the model cannot call is a tool that does not exist, and truncating
// to fit would invent collisions between tools that differ only in the part
// that was cut off.
func TestAnUnusableNameIsRefusedRatherThanTruncated(t *testing.T) {
	cfg := helperConfig(strings.Repeat("long", 20))

	server, err := mcp.Connect(context.Background(), cfg, mcp.Limits{}, nil,
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	if count := len(server.Tools()); count != 0 {
		t.Errorf("%d tools were kept despite unusable names", count)
	}
}

// A tool that failed is an observation the model can act on, not an error that
// ends the run.
func TestAFailureFromTheServerIsAnObservation(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{})

	result, err := toolNamed(t, server, "mcp_helper_boom").
		Execute(context.Background(), tool.Call{ID: "call_1", Name: "mcp_helper_boom"})
	if err != nil {
		t.Fatalf("a tool failing was reported as an error: %v", err)
	}

	if !result.IsError {
		t.Error("a failure came back unmarked")
	}
	if !strings.Contains(result.Content, "does not exist") {
		t.Errorf("the model is not told what went wrong: %q", result.Content)
	}
}

// A tool that can fill the context window in one call can end a session in one
// call.
func TestOneResultCannotFillTheContextWindow(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{MaxOutput: 4000})

	result, err := toolNamed(t, server, "mcp_helper_huge").
		Execute(context.Background(), tool.Call{ID: "call_1", Name: "mcp_helper_huge"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(result.Content) > 4200 {
		t.Errorf("the result is %d bytes against a 4000-byte bound", len(result.Content))
	}
	if !result.Truncated {
		t.Error("a bounded result did not say it was truncated")
	}
	// Both ends, so the model can see what it got and what it ended with.
	for _, want := range []string{"HEAD", "TAIL", "omitted"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("the bounded result lost %q", want)
		}
	}
}

// An answer larger than fits is still an answer somebody asked for. Deciding
// on their behalf that the middle did not matter is worse than keeping it.
func TestAnOversizedAnswerIsKeptWhole(t *testing.T) {
	store, err := artifact.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}

	server := connectWithStore(t, helperConfig("helper"), mcp.Limits{MaxOutput: 4000}, store)

	result, err := toolNamed(t, server, "mcp_helper_huge").
		Execute(context.Background(), tool.Call{ID: "call_1", Name: "mcp_helper_huge"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Artifact == nil {
		t.Fatal("an answer that did not fit was not kept")
	}
	if !strings.Contains(result.Content, result.Artifact.ID) {
		t.Error("the model cannot see the id of what was kept")
	}

	window, total, err := store.ReadRange(result.Artifact.ID, 0, 8)
	if err != nil {
		t.Fatalf("read what was kept: %v", err)
	}
	if !strings.HasPrefix(string(window), "HEAD") {
		t.Errorf("what was kept starts with %q", window)
	}
	if total <= 4000 {
		t.Errorf("what was kept is %d bytes, no more than what was shown", total)
	}
}

// A server that stops answering is a fact about this machine, not a mistake
// the model made. Told plainly, and told that retrying will not help, it does
// something else instead of calling a dead tool until it runs out of turns.
func TestADeadServerIsReportedToTheModel(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{CallTimeout: 5 * time.Second})

	_, err := toolNamed(t, server, "mcp_helper_die").
		Execute(context.Background(), tool.Call{ID: "call_1", Name: "mcp_helper_die"})
	if err == nil {
		t.Fatal("a server that exited mid-call reported success")
	}

	var failure *tool.Error
	if !asToolError(err, &failure) {
		t.Fatalf("a dead server produced %T rather than something the model can read: %v", err, err)
	}
	if failure.Retryable {
		t.Error("the model is told to retry a server that is gone")
	}
	if failure.SuggestedAction == "" {
		t.Error("the model is not told what to do instead")
	}
}

// A call that never comes back has to end, and end as something the model can
// act on.
func TestACallThatNeverAnswersTimesOut(t *testing.T) {
	server := connect(t, helperConfig("helper"), mcp.Limits{CallTimeout: 300 * time.Millisecond})

	started := time.Now()
	_, err := toolNamed(t, server, "mcp_helper_slow").
		Execute(context.Background(), tool.Call{ID: "call_1", Name: "mcp_helper_slow"})
	elapsed := time.Since(started)

	var failure *tool.Error
	if !asToolError(err, &failure) {
		t.Fatalf("a hung call produced %T: %v", err, err)
	}
	if failure.Code != tool.CodeTimeout {
		t.Errorf("code is %s, want %s", failure.Code, tool.CodeTimeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call took %s to give up on a 300ms limit", elapsed)
	}
}

// The daemon's environment holds the provider credentials. Handing those to
// every server somebody installs would make installing one an act of trust
// nobody was asked for.
func TestTheDaemonsEnvironmentIsNotInherited(t *testing.T) {
	t.Setenv("JINGCLAW_TEST_SECRET", "a-provider-key")
	t.Setenv("JINGCLAW_TEST_SHARED", "meant-to-be-passed")

	cfg := helperConfig("helper")
	cfg.PassEnv = []string{"JINGCLAW_TEST_SHARED"}
	cfg.Env["JINGCLAW_TEST_LITERAL"] = "from-the-config-file"

	server := connect(t, cfg, mcp.Limits{})
	readEnv := toolNamed(t, server, "mcp_helper_read_env")

	read := func(name string) string {
		t.Helper()
		result, err := readEnv.Execute(context.Background(), tool.Call{
			ID:        "call_1",
			Name:      "mcp_helper_read_env",
			Arguments: arguments(t, map[string]any{"name": name}),
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return strings.TrimSpace(result.Content)
	}

	if got := read("JINGCLAW_TEST_SECRET"); strings.Contains(got, "a-provider-key") {
		t.Errorf("the server can see a credential nobody passed it: %q", got)
	}
	if got := read("JINGCLAW_TEST_SHARED"); got != "meant-to-be-passed" {
		t.Errorf("pass_env did not reach the server: %q", got)
	}
	if got := read("JINGCLAW_TEST_LITERAL"); got != "from-the-config-file" {
		t.Errorf("a configured value did not reach the server: %q", got)
	}
	// PATH has to survive, or almost nothing starts.
	if read("PATH") == "" {
		t.Error("the server has no PATH and could not find its own interpreter")
	}
}

// Registering has to go through the same registry as everything else, which is
// also what makes a collision with a built-in impossible to miss.
func TestServersRegisterAlongsideTheBuiltIns(t *testing.T) {
	manager := mcp.Start(context.Background(),
		[]mcp.ServerConfig{helperConfig("one"), helperConfig("two")},
		mcp.Limits{}, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = manager.Close() })

	if manager.Connected() != 2 {
		t.Fatalf("%d of 2 servers connected", manager.Connected())
	}

	registry := tool.NewRegistry()
	registry.MustRegister(&fakeBuiltin{})

	if err := manager.Register(registry); err != nil {
		t.Fatalf("register: %v", err)
	}

	names := make(map[string]bool)
	for _, spec := range registry.Specs() {
		names[spec.Name] = true
	}

	for _, want := range []string{"read_file", "mcp_one_echo", "mcp_two_echo"} {
		if !names[want] {
			t.Errorf("%s is not in the registry", want)
		}
	}
}

// A server that will not start must not stop the agent working, and must not
// do so silently either.
func TestABrokenServerDoesNotStopTheOthers(t *testing.T) {
	broken := helperConfig("broken")
	broken.Command = "/nonexistent/definitely-not-a-program"

	manager := mcp.Start(context.Background(),
		[]mcp.ServerConfig{broken, helperConfig("working")},
		mcp.Limits{}, nil, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = manager.Close() })

	if manager.Connected() != 1 {
		t.Errorf("%d servers connected, want only the working one", manager.Connected())
	}
	if manager.ToolCount() == 0 {
		t.Error("the working server contributed nothing")
	}
}

// fakeBuiltin stands in for a real built-in, so the namespacing can be checked
// against a name that actually matters.
type fakeBuiltin struct{}

func (*fakeBuiltin) Spec() tool.Spec {
	return tool.Spec{
		Name:        "read_file",
		Description: "Read part of a file in the workspace.",
		InputSchema: []byte(`{"type":"object"}`),
		Level:       tool.LevelWorkspaceRead,
	}
}

func (*fakeBuiltin) Execute(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{Content: "built in"}, nil
}
