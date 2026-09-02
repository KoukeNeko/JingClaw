package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()

	var counter atomic.Uint64
	manager := NewManager()
	manager.NewID = func() ID { return ID(fmt.Sprintf("prc_%d", counter.Add(1))) }
	t.Cleanup(manager.CloseAll)

	return manager
}

// waitFor polls until the condition holds, so a test does not depend on how
// fast a program happens to start.
func waitFor(t *testing.T, why string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func readAll(t *testing.T, manager *Manager, id ID) string {
	t.Helper()

	output, _, _, err := manager.Read(id, 0)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return output
}

// The point of this package: the program is still there once Start returns.
func TestAProgramOutlivesTheCallThatStartedIt(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !state.Running {
		t.Fatal("the program had already ended when Start returned")
	}
	if state.PID == 0 {
		t.Error("no pid, so nobody at the machine could find it")
	}
}

// A name that does not exist must be an error the caller sees, not an id for
// a process that dies a moment later.
func TestAProgramThatCannotStartIsAnError(t *testing.T) {
	manager := newTestManager(t)

	if _, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "jingclaw-no-such-program",
	}); err == nil {
		t.Fatal("starting a program that does not exist reported success")
	}
}

// A process belongs to a session. One with none belongs to nothing, so
// nothing would ever end it.
func TestAProcessMustBelongToASession(t *testing.T) {
	manager := newTestManager(t)

	if _, err := manager.Start(StartOptions{Program: "sh", Args: []string{"-c", "true"}}); err == nil {
		t.Fatal("a process with no session was accepted")
	}
}

// Output accumulates while nobody is reading, which is the whole reason a
// buffer exists.
func TestOutputIsKeptUntilItIsRead(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "echo first; echo second"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitFor(t, "the program to finish", func() bool {
		got, _ := manager.Get(state.ID)
		return !got.Running
	})

	output := readAll(t, manager, state.ID)
	if !strings.Contains(output, "first") || !strings.Contains(output, "second") {
		t.Errorf("output is %q, want both lines", output)
	}
}

// Error output belongs beside the line it followed. Separated, a reader has to
// guess where a failure happened.
func TestErrorOutputArrivesWithTheRest(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "echo out; echo problem >&2"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the program to finish", func() bool {
		got, _ := manager.Get(state.ID)
		return !got.Running
	})

	output := readAll(t, manager, state.ID)
	if !strings.Contains(output, "problem") {
		t.Errorf("the error output is missing from %q", output)
	}
}

// A caller polling for new output must not be handed the same bytes twice.
func TestReadingResumesWhereItLeftOff(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "echo one; sleep 0.4; echo two"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitFor(t, "the first line", func() bool {
		output, _, _, _ := manager.Read(state.ID, 0)
		return strings.Contains(output, "one")
	})

	first, next, _, err := manager.Read(state.ID, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(first, "one") {
		t.Fatalf("the first read is %q", first)
	}

	waitFor(t, "the second line", func() bool {
		output, _, _, _ := manager.Read(state.ID, next)
		return strings.Contains(output, "two")
	})

	second, _, _, err := manager.Read(state.ID, next)
	if err != nil {
		t.Fatalf("read again: %v", err)
	}
	if strings.Contains(second, "one") {
		t.Errorf("the second read repeats the first: %q", second)
	}
}

// Output past the buffer's limit is lost from the beginning, and the loss is
// reported. Silently missing its middle is how a reader concludes a build
// succeeded.
func TestLostOutputIsReportedRatherThanHidden(t *testing.T) {
	manager := newTestManager(t)
	manager.BufferBytes = 256

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "for i in $(seq 1 400); do echo line $i; done"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the program to finish", func() bool {
		got, _ := manager.Get(state.ID)
		return !got.Running
	})

	got, err := manager.Get(state.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OutputDropped == 0 {
		t.Fatal("400 lines fitted in a 256-byte buffer, so this test proves nothing")
	}

	output, _, skipped, err := manager.Read(state.ID, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(output) > 256 {
		t.Errorf("the buffer kept %d bytes, over its %d limit", len(output), 256)
	}
	if skipped == 0 {
		t.Error("a read from the beginning was not told that the beginning is gone")
	}
	if !strings.Contains(output, "line 400") {
		t.Error("the newest output was dropped; the oldest is the end to lose")
	}
}

// A cursor pointing at output that has since been overwritten must be told
// what it missed rather than quietly answered from a later place — which is
// how a reader ends up looking at the middle of a line and believing it is the
// start of one.
func TestAStaleCursorIsToldWhatItMissed(t *testing.T) {
	manager := newTestManager(t)
	manager.BufferBytes = 128

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "for i in $(seq 1 200); do echo line $i; done"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the program to finish", func() bool {
		got, _ := manager.Get(state.ID)
		return !got.Running
	})

	// Everything up to the newest is still there, so a cursor at the end
	// misses nothing.
	_, next, _, err := manager.Read(state.ID, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, _, skipped, err := manager.Read(state.ID, next); err != nil {
		t.Fatalf("read from the end: %v", err)
	} else if skipped != 0 {
		t.Errorf("a cursor at the newest byte reported %d skipped", skipped)
	}

	// Offset 1 is inside what has been overwritten, so the answer starts
	// later than asked for and has to say so.
	_, _, skippedLater, err := manager.Read(state.ID, 1)
	if err != nil {
		t.Fatalf("read from a stale cursor: %v", err)
	}
	if skippedLater == 0 {
		t.Error("a cursor pointing at overwritten output was answered as though nothing was missed")
	}
}

// Input reaches the program. Without this, a REPL and an installer are both
// out of reach.
func TestInputReachesTheProgram(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "read line; echo you said $line"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := manager.Write(state.ID, "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitFor(t, "the answer", func() bool {
		return strings.Contains(readAll(t, manager, state.ID), "you said hello")
	})
}

// Writing to a program that has ended must say so rather than look like it
// worked.
func TestWritingToAFinishedProgramSaysSo(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1", Program: "sh", Args: []string{"-c", "true"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := manager.Wait(context.Background(), state.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if err := manager.Write(state.ID, "hello\n"); err == nil {
		t.Fatal("writing to a finished program reported success")
	}
}

// A failure has to be distinguishable from success once the program is gone.
func TestHowAProgramEndedIsKept(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1", Program: "sh", Args: []string{"-c", "exit 3"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	final, err := manager.Wait(context.Background(), state.ID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if final.Running {
		t.Error("still reported as running after Wait returned")
	}
	if final.ExitCode != 3 {
		t.Errorf("exit code is %d, want 3", final.ExitCode)
	}
}

// Stopping takes the children with it. Otherwise a dev server's watcher keeps
// the port bound and the next start fails for a reason that looks nothing like
// the cause.
func TestStoppingTakesTheChildrenWithIt(t *testing.T) {
	manager := newTestManager(t)

	// Forward slashes so the path survives being embedded in a shell command:
	// a Windows temp path carries backslashes, which the shell reads as escapes
	// and swallows. os.Stat reads it back either way.
	marker := filepath.ToSlash(t.TempDir()) + "/still-running"
	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args: []string{"-c", fmt.Sprintf(
			"(while true; do touch %s; sleep 0.1; done) & wait", marker)},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	waitFor(t, "the child to be running", func() bool {
		_, statErr := readFileTime(marker)
		return statErr == nil
	})

	if _, err := manager.Stop(state.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	before, err := readFileTime(marker)
	if err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	after, err := readFileTime(marker)
	if err != nil {
		t.Fatalf("read the marker again: %v", err)
	}
	if !after.Equal(before) {
		t.Error("the child is still running after its parent was stopped")
	}
}

// Closing a session ends its processes. One nobody can name any more keeps a
// port bound until somebody notices at the machine.
func TestClosingASessionEndsItsProcesses(t *testing.T) {
	manager := newTestManager(t)

	mine, err := manager.Start(StartOptions{
		SessionID: "ses_1", Program: "sh", Args: []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	theirs, err := manager.Start(StartOptions{
		SessionID: "ses_2", Program: "sh", Args: []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("start the other: %v", err)
	}

	stopped := manager.CloseSession("ses_1")
	if len(stopped) != 1 {
		t.Fatalf("stopped %d processes, want the one belonging to that session", len(stopped))
	}
	if _, err := manager.Get(mine.ID); err == nil {
		t.Error("the closed session's process is still listed")
	}

	other, err := manager.Get(theirs.ID)
	if err != nil {
		t.Fatalf("the other session's process went too: %v", err)
	}
	if !other.Running {
		t.Error("closing one session ended another's process")
	}
}

// A process belongs to the session that started it, and nobody else's list.
func TestProcessesAreListedPerSession(t *testing.T) {
	manager := newTestManager(t)

	for _, session := range []string{"ses_1", "ses_1", "ses_2"} {
		if _, err := manager.Start(StartOptions{
			SessionID: session, Program: "sh", Args: []string{"-c", "sleep 30"},
		}); err != nil {
			t.Fatalf("start in %s: %v", session, err)
		}
	}

	if listed := manager.List("ses_1"); len(listed) != 2 {
		t.Errorf("ses_1 has %d processes, want 2", len(listed))
	}
	if listed := manager.List("ses_2"); len(listed) != 1 {
		t.Errorf("ses_2 has %d processes, want 1", len(listed))
	}
	if listed := manager.List("ses_3"); len(listed) != 0 {
		t.Errorf("a session that started nothing has %d processes", len(listed))
	}
}

// An id that does not exist is an error, not an empty answer that reads like a
// program producing nothing.
func TestAnUnknownProcessIsAnError(t *testing.T) {
	manager := newTestManager(t)

	if _, err := manager.Get("prc_nothing"); err == nil {
		t.Error("an unknown id was answered as a process")
	}
	if _, _, _, err := manager.Read("prc_nothing", 0); err == nil {
		t.Error("reading an unknown id reported success")
	}
	if err := manager.Write("prc_nothing", "x"); err == nil {
		t.Error("writing to an unknown id reported success")
	}
}

func readFileTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// A pseudo-terminal is why this package exists rather than a background flag
// on exec_command: a program that sees a pipe buffers its output into blocks
// and often refuses to prompt at all, so an installer's question never
// arrives and the agent waits for something that was never sent.
func TestATerminalMakesAProgramBehaveInteractively(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", `printf "name? "; read name; echo hello $name`},
		Terminal:  true,
		Columns:   80,
		Rows:      24,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !state.Terminal {
		t.Skip("no pseudo-terminal on this platform")
	}

	// The prompt has no newline after it. Through a pipe it would sit in the
	// program's buffer until it exits, which is after the answer it is
	// waiting for — the deadlock this exists to avoid.
	waitFor(t, "the prompt", func() bool {
		return strings.Contains(readAll(t, manager, state.ID), "name?")
	})

	if err := manager.Write(state.ID, "world\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "the answer", func() bool {
		return strings.Contains(readAll(t, manager, state.ID), "hello world")
	})
}

// A program that draws to the width it is given must be told the truth about
// what that is.
func TestATerminalCanBeResized(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1",
		Program:   "sh",
		Args:      []string{"-c", "sleep 10"},
		Terminal:  true,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !state.Terminal {
		t.Skip("no pseudo-terminal on this platform")
	}

	if err := manager.Resize(state.ID, 200, 60); err != nil {
		t.Errorf("resize: %v", err)
	}
}

// A process without a terminal must say so rather than accept a resize that
// does nothing.
func TestResizingAProcessWithNoTerminalIsRefused(t *testing.T) {
	manager := newTestManager(t)

	state, err := manager.Start(StartOptions{
		SessionID: "ses_1", Program: "sh", Args: []string{"-c", "sleep 10"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := manager.Resize(state.ID, 200, 60); err == nil {
		t.Error("resizing a process with no terminal reported success")
	}
}
