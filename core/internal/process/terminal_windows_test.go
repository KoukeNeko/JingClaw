//go:build windows

package process

import (
	"os"
	"strings"
	"testing"
)

// The Windows terminal path end to end: a program asked for with a terminal runs
// on a pseudo console, its output comes back through the manager, and the
// terminal can be resized while it runs. This is the integration the conpty
// package exists for.
func TestAWindowsTerminalRunsAProgramAndResizes(t *testing.T) {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		t.Skip("no command interpreter to run")
	}

	manager := newTestManager(t)
	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   shell,
		Args: []string{"/c",
			"echo terminal-works & %SystemRoot%\\System32\\ping.exe -n 4 127.0.0.1 >nul"},
		Env:       []string{"SystemRoot=" + os.Getenv("SystemRoot")},
		Terminal:  true,
		Columns:   80,
		Rows:      25,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !state.Terminal {
		t.Fatal("a terminal was asked for and the program did not get one")
	}

	if err := manager.Resize(state.ID, 100, 40); err != nil {
		t.Errorf("resize a running terminal: %v", err)
	}

	waitFor(t, "the program to finish", func() bool {
		got, _ := manager.Get(state.ID)
		return !got.Running
	})

	output := readAll(t, manager, state.ID)
	if !strings.Contains(output, "terminal-works") {
		t.Errorf("the program's output did not come through the terminal:\n%q", output)
	}
}
