package builtin_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

func newExecFixture(t *testing.T) (*tool.Registry, string) {
	t.Helper()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	registry := tool.NewRegistry()
	registry.MustRegister(&builtin.ExecCommand{Workspace: ws})

	return registry, root
}

// The tests run whatever the platform provides rather than assuming a POSIX
// shell, because Windows is a target and an agent that needs bash there is an
// agent that does not run there.
func echoCommand(text string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/s", "/c", "echo " + text}
	}
	return "/bin/echo", []string{text}
}

func exitCommand(code string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/s", "/c", "exit " + code}
	}
	return "/bin/sh", []string{"-c", "exit " + code}
}

func TestExecReturnsOutputAndSuccess(t *testing.T) {
	registry, _ := newExecFixture(t)

	program, args := echoCommand("hello from jingclaw")
	result := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})
	if result.IsError {
		t.Fatalf("command failed: %s", result.Content)
	}

	if !strings.Contains(result.Content, "hello from jingclaw") {
		t.Errorf("output missing:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "exit status 0") {
		t.Errorf("exit status not reported:\n%s", result.Content)
	}
}

// A failing build or test suite is the answer to the question the agent asked,
// not a malfunction. It must come back as a readable observation.
func TestNonZeroExitIsAnObservationWithItsOutput(t *testing.T) {
	registry, _ := newExecFixture(t)

	program, args := exitCommand("3")
	result := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args,
	})

	if !result.IsError {
		t.Fatal("a non-zero exit was reported as success")
	}
	if !strings.Contains(result.Content, "exit status 3") {
		t.Errorf("the exit code is not reported:\n%s", result.Content)
	}
}

func TestUnknownProgramIsReportedClearly(t *testing.T) {
	registry, _ := newExecFixture(t)

	result := call(t, registry, "exec_command", map[string]any{
		"program": "definitely-not-a-real-program-xyz",
	})
	if !result.IsError {
		t.Fatal("running a non-existent program reported success")
	}
	assertErrorCode(t, result, tool.CodeNotFound)
}

// A hung command has to stop, and what it printed before hanging is usually
// where the answer is.
func TestTimeoutKillsTheCommandAndKeepsItsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell for the sleep loop")
	}

	registry, _ := newExecFixture(t)

	started := time.Now()
	result := call(t, registry, "exec_command", map[string]any{
		"program":         "/bin/sh",
		"args":            []string{"-c", "echo starting; sleep 30"},
		"timeout_seconds": 1,
	})
	elapsed := time.Since(started)

	if !result.IsError {
		t.Fatal("a command that outlived its timeout reported success")
	}
	assertErrorCode(t, result, tool.CodeTimeout)

	if elapsed > 15*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}
	if !strings.Contains(result.Content, "starting") {
		t.Errorf("output produced before the timeout was discarded:\n%s", result.Content)
	}
}

// Killing only the started process leaves its children holding the pipes open
// and the ports bound.
func TestTimeoutKillsTheWholeProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX; Windows needs a Job Object")
	}

	registry, root := newExecFixture(t)
	marker := filepath.Join(root, "child-still-alive")

	// A child that outlives its parent and then writes a file. If the group is
	// killed, the file never appears.
	result := call(t, registry, "exec_command", map[string]any{
		"program": "/bin/sh",
		"args": []string{"-c",
			"(sleep 3; touch " + marker + ") & echo spawned; sleep 30"},
		"timeout_seconds": 1,
	})
	if !result.IsError {
		t.Fatal("the command was expected to time out")
	}

	// Long enough for the orphan to have written the file, had it survived.
	time.Sleep(4 * time.Second)

	if _, err := os.Stat(marker); err == nil {
		t.Error("a child process outlived the killed command")
	}
}

func TestCwdIsConfinedToTheWorkspace(t *testing.T) {
	registry, root := newExecFixture(t)

	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	program, args := echoCommand("ok")
	inside := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args, "cwd": "sub",
	})
	if inside.IsError {
		t.Errorf("a directory inside the workspace was rejected: %s", inside.Content)
	}

	outside := call(t, registry, "exec_command", map[string]any{
		"program": program, "args": args, "cwd": "../..",
	})
	if !outside.IsError {
		t.Fatal("ran a command outside the workspace")
	}
	assertErrorCode(t, outside, tool.CodeOutsideWorkspace)
}

// The daemon's environment holds provider credentials. Passing it through
// would hand an API key to every command the model decides to run.
func TestCredentialsAreNotPassedToCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to print the environment")
	}

	t.Setenv("GEMINI_API_KEY", "super-secret-value")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "another-secret")

	registry, _ := newExecFixture(t)

	result := call(t, registry, "exec_command", map[string]any{
		"program": "/bin/sh", "args": []string{"-c", "env"},
	})
	if result.IsError {
		t.Fatalf("command failed: %s", result.Content)
	}

	for _, secret := range []string{"super-secret-value", "another-secret"} {
		if strings.Contains(result.Content, secret) {
			t.Errorf("a credential reached the command's environment")
		}
	}
	// PATH still has to be there or nothing runs.
	if !strings.Contains(result.Content, "PATH=") {
		t.Errorf("PATH was not passed through:\n%s", result.Content)
	}
}

// Long output has to be bounded, and the caller told it was cut.
func TestLongOutputIsTruncatedInTheMiddle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell for the output loop")
	}

	registry, _ := newExecFixture(t)

	result := call(t, registry, "exec_command", map[string]any{
		"program": "/bin/sh",
		"args": []string{"-c",
			"echo FIRSTLINE; i=0; while [ $i -lt 4000 ]; do echo 'filler line of some length here'; i=$((i+1)); done; echo LASTLINE"},
		"timeout_seconds": 30,
	})
	if result.IsError {
		t.Fatalf("command failed: %s", result.Content)
	}

	if !result.Truncated {
		t.Error("long output was not reported as truncated")
	}
	// The start says what ran and the end says how it ended; both must survive.
	if !strings.Contains(result.Content, "FIRSTLINE") {
		t.Error("the beginning of the output was lost")
	}
	if !strings.Contains(result.Content, "LASTLINE") {
		t.Error("the end of the output was lost")
	}
	if !strings.Contains(result.Content, "omitted") {
		t.Error("the caller is not told output was omitted")
	}
}

// There is no shell, so shell syntax is passed to the program verbatim rather
// than being interpreted. That is the point: model-written text never becomes
// a command line.
func TestShellSyntaxIsNotInterpreted(t *testing.T) {
	registry, root := newExecFixture(t)
	marker := filepath.Join(root, "should-not-exist")

	program, _ := echoCommand("")
	result := call(t, registry, "exec_command", map[string]any{
		"program": program,
		"args":    []string{"harmless; touch " + marker},
	})
	if result.IsError && runtime.GOOS != "windows" {
		t.Fatalf("command failed: %s", result.Content)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("shell metacharacters in an argument were executed")
	}
}

func TestShellForFindsSomethingOnThisPlatform(t *testing.T) {
	program, prefix, ok := builtin.ShellFor()
	if !ok {
		t.Skip("no shell on this machine")
	}
	if program == "" || len(prefix) == 0 {
		t.Errorf("ShellFor returned %q with prefix %v", program, prefix)
	}
}
