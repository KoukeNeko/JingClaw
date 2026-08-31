//go:build linux

package builtin_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/sandbox"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// TestMain makes this binary one that can confine, the way the real one is.
//
// Wrap re-executes the running executable and expects it to apply the policy
// and then become the command. Without this the test binary runs itself as
// tests again, and those start another — which is a hang, and is what
// happened here before the guard in Wrap existed.
func TestMain(m *testing.M) {
	sandbox.WillConfine()

	if sandbox.Confining(os.Args[1:]) {
		sandbox.Confine(os.Args[1:])
	}
	os.Exit(m.Run())
}

// TestABinaryThatCannotConfineIsRefusedRatherThanLooped is the guard itself.
//
// The failure it replaces did not look like a failure: Wrap handed back "run
// me again", the caller did, and the caller ran itself. Twice, in two
// packages, and both times it arrived as a test that never finished.
func TestABinaryThatCannotConfineIsRefusedRatherThanLooped(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("nothing here can confine a command")
	}

	// What a binary that never said so would get. Restored immediately: every
	// other test in this file depends on the declaration TestMain made.
	sandbox.ForgetConfinementForTest(t)

	_, _, done, err := sandbox.Wrap(sandbox.Policy{Network: true}, "true", nil)
	if done != nil {
		done()
	}
	if err == nil {
		t.Fatal("a binary that cannot confine was handed itself to run")
	}
	if !strings.Contains(err.Error(), "WillConfine") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
}

// TestAnExecThatCannotBeConfinedIsRefused is the rule the whole feature rests
// on, checked where Linux can be made to break it.
//
// The Mac version of this points the sandbox at a program that is not there.
// Here the policy itself is one landlock cannot express: it grants access
// rather than removing it, so "everything readable except this directory" has
// no form — under a policy that is deliberately not deny-by-default, a
// directory is hidden only by nothing granting it.
//
// The refusal has to happen before the command starts. Refused afterwards, in
// the confined process, it would reach the caller as a command that failed
// rather than as confinement that was not available.
func TestAnExecThatCannotBeConfinedIsRefused(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("nothing here can confine a command, so there is nothing to refuse")
	}

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	command := &builtin.ExecCommand{
		Workspace: ws,
		Confine: &builtin.Confinement{
			Policy: sandbox.Policy{
				Network:    true,
				Unreadable: []string{"/etc/ssh"},
			},
		},
	}

	arguments, err := json.Marshal(map[string]any{
		"program": "touch",
		"args":    []string{"should-not-happen"},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := command.Execute(t.Context(), tool.Call{Arguments: arguments})
	if err == nil {
		t.Fatalf("the command ran under a policy that could not be kept: %+v", result)
	}
	if !strings.Contains(err.Error(), "confine") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And nothing happened. A refusal that let the command run first would be
	// a report rather than a refusal.
	if _, err := os.Stat(root + "/should-not-happen"); err == nil {
		t.Error("the command ran anyway")
	}
}

// TestAPolicyThisKernelCanKeepIsNotRefused is the precondition.
//
// Without it, the check above would pass against a build that refused every
// policy — which would also stop every command, and would look like working
// confinement until somebody tried to use it.
func TestAPolicyThisKernelCanKeepIsNotRefused(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("nothing here can confine a command")
	}

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	command := &builtin.ExecCommand{
		Workspace: ws,
		Confine:   &builtin.Confinement{Policy: sandbox.Policy{Network: true}},
	}

	arguments, err := json.Marshal(map[string]any{
		"program": "touch",
		"args":    []string{"allowed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := command.Execute(t.Context(), tool.Call{Arguments: arguments}); err != nil {
		t.Fatalf("a policy this kernel can keep was refused: %v", err)
	}
	if _, err := os.Stat(root + "/allowed"); err != nil {
		t.Errorf("the confined command did not run: %v", err)
	}
}
