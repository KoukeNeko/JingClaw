package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/sandbox"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// A machine that cannot confine refuses the command rather than running it.
//
// The daemon also refuses at startup, so in an ordinary deployment this is
// never reached — which is exactly why it is worth a check of its own. Two
// layers where one would do is only defence in depth while both of them
// work, and the one nothing exercises is the one that quietly stops.
func TestAnExecThatCannotBeConfinedIsRefused(t *testing.T) {
	// Somewhere there is no such program, which is the one thing no real Mac
	// is: every one of them has sandbox-exec.
	t.Setenv(sandbox.SeatbeltEnv, "/nonexistent/sandbox-exec")

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	command := &builtin.ExecCommand{
		Workspace: ws,
		Confine:   &builtin.Confinement{Policy: sandbox.Policy{}},
	}

	arguments, err := json.Marshal(map[string]any{
		"program": "/usr/bin/touch",
		"args":    []string{"/tmp/should-not-happen"},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := command.Execute(t.Context(), tool.Call{Arguments: arguments})
	if err == nil {
		t.Fatalf("the command ran without being confined: %+v", result)
	}
	if !strings.Contains(err.Error(), "confine") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// And with confinement off, nothing about this path applies: a deployment
// that never asked for a sandbox runs what it always ran.
func TestWithoutConfinementTheCommandRuns(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	command := &builtin.ExecCommand{Workspace: ws}

	arguments, err := json.Marshal(map[string]any{
		"program": "/usr/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := command.Execute(t.Context(), tool.Call{Arguments: arguments}); err != nil {
		t.Errorf("an unconfined command failed: %v", err)
	}
}
