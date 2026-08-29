package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/process"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

func newProcessTools(t *testing.T) (*StartProcess, *ProcessIO, *StopProcess) {
	t.Helper()

	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	manager := process.NewManager()
	t.Cleanup(manager.CloseAll)

	return &StartProcess{Workspace: ws, Processes: manager},
		&ProcessIO{Processes: manager},
		&StopProcess{Processes: manager}
}

func callWith(t *testing.T, args any) tool.Call {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	return tool.Call{
		ID:        "call_1",
		Arguments: encoded,
		Context:   tool.CallContext{SessionID: "ses_1", RunID: "run_1"},
	}
}

func startedID(t *testing.T, result tool.Result) string {
	t.Helper()
	// "started sh as prc_…"
	fields := strings.Fields(result.Summary)
	if len(fields) < 4 {
		t.Fatalf("cannot find the id in %q", result.Summary)
	}
	return fields[len(fields)-1]
}

// The whole reason these are separate from exec_command: a caller reads what a
// program has said so far while it is still saying it.
func TestAProcessCanBeReadWhileItIsStillRunning(t *testing.T) {
	start, io, stop := newProcessTools(t)
	ctx := context.Background()

	started, err := start.Execute(ctx, callWith(t, startProcessArgs{
		Program: "sh",
		Args:    []string{"-c", "echo listening on 3000; sleep 30"},
	}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := startedID(t, started)

	read, err := io.Execute(ctx, callWith(t, processIOArgs{
		ProcessID: id, WaitSeconds: 5,
	}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(read.Content, "listening on 3000") {
		t.Errorf("the output is missing: %q", read.Content)
	}
	if !strings.Contains(read.Content, "still running") {
		t.Errorf("a running process was not reported as running: %q", read.Content)
	}

	if _, err := stop.Execute(ctx, callWith(t, stopProcessArgs{ProcessID: id})); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A caller that polls must not be handed the same output again: a model that
// reads the same line twice concludes it happened twice.
func TestReadingTwiceDoesNotRepeatTheOutput(t *testing.T) {
	start, io, _ := newProcessTools(t)
	ctx := context.Background()

	started, err := start.Execute(ctx, callWith(t, startProcessArgs{
		Program: "sh", Args: []string{"-c", "echo once; sleep 5"},
	}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := startedID(t, started)

	first, err := io.Execute(ctx, callWith(t, processIOArgs{ProcessID: id, WaitSeconds: 5}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	next := nextOffset(t, first.Content)

	second, err := io.Execute(ctx, callWith(t, processIOArgs{
		ProcessID: id, Offset: next, WaitSeconds: 0,
	}))
	if err != nil {
		t.Fatalf("read again: %v", err)
	}
	if strings.Contains(second.Content, "once") {
		t.Errorf("the second read repeats the first: %q", second.Content)
	}
	if !strings.Contains(second.Content, "no new output") {
		t.Errorf("nothing new was not said plainly: %q", second.Content)
	}
}

// Input reaches the program, which is what makes an installer or a REPL
// reachable at all.
func TestInputAndTheAnswerAreOneCall(t *testing.T) {
	start, io, _ := newProcessTools(t)
	ctx := context.Background()

	started, err := start.Execute(ctx, callWith(t, startProcessArgs{
		Program: "sh", Args: []string{"-c", "read name; echo hello $name"},
	}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := startedID(t, started)

	answered, err := io.Execute(ctx, callWith(t, processIOArgs{
		ProcessID: id, Input: "world\n", WaitSeconds: 5,
	}))
	if err != nil {
		t.Fatalf("write and read: %v", err)
	}
	if !strings.Contains(answered.Content, "hello world") {
		t.Errorf("the answer never came: %q", answered.Content)
	}
}

// A finished program must be reported as finished, with how it ended. Read as
// still running, a model waits for output that is never coming.
func TestAFinishedProcessSaysHowItEnded(t *testing.T) {
	start, io, _ := newProcessTools(t)
	ctx := context.Background()

	started, err := start.Execute(ctx, callWith(t, startProcessArgs{
		Program: "sh", Args: []string{"-c", "echo done; exit 2"},
	}))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := startedID(t, started)

	deadline := time.Now().Add(5 * time.Second)
	var content string
	for time.Now().Before(deadline) {
		read, readErr := io.Execute(ctx, callWith(t, processIOArgs{ProcessID: id, WaitSeconds: 1}))
		if readErr != nil {
			t.Fatalf("read: %v", readErr)
		}
		content = read.Content
		if strings.Contains(content, "ended with exit code") {
			break
		}
	}
	if !strings.Contains(content, "ended with exit code 2") {
		t.Errorf("how it ended was never reported: %q", content)
	}
}

// "Which process" is a thing a model gets wrong, and it must be told rather
// than handed an empty answer that reads like a program producing nothing.
func TestAnUnknownProcessIsRefusedClearly(t *testing.T) {
	_, io, stop := newProcessTools(t)
	ctx := context.Background()

	_, err := io.Execute(ctx, callWith(t, processIOArgs{ProcessID: "prc_nothing"}))
	if err == nil {
		t.Fatal("reading an unknown process reported success")
	}
	var refusal *tool.Error
	if !asToolError(err, &refusal) || refusal.Code != tool.CodeNotFound {
		t.Errorf("the refusal is %v, want a not_found", err)
	}

	if _, err := stop.Execute(ctx, callWith(t, stopProcessArgs{ProcessID: "prc_nothing"})); err == nil {
		t.Error("stopping an unknown process reported success")
	}
}

// A process belongs to a session, and a call with none belongs to nothing.
func TestACallWithNoSessionIsRefused(t *testing.T) {
	start, _, _ := newProcessTools(t)

	call := callWith(t, startProcessArgs{Program: "sh", Args: []string{"-c", "true"}})
	call.Context.SessionID = ""

	if _, err := start.Execute(context.Background(), call); err == nil {
		t.Error("a process with no session was started")
	}
}

// Where it runs is the workspace, checked the same way every other tool checks
// it. A process is the tool with the longest reach, so it is the worst one to
// let out.
func TestAProcessCannotBeStartedOutsideTheWorkspace(t *testing.T) {
	start, _, _ := newProcessTools(t)

	_, err := start.Execute(context.Background(), callWith(t, startProcessArgs{
		Program: "sh", Args: []string{"-c", "true"}, Cwd: "../../..",
	}))
	if err == nil {
		t.Fatal("a process was started outside the workspace")
	}
}

func nextOffset(t *testing.T, content string) int64 {
	t.Helper()

	at := strings.LastIndex(content, "next_offset ")
	if at < 0 {
		t.Fatalf("no next_offset in %q", content)
	}
	rest := content[at+len("next_offset "):]
	var offset int64
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		offset = offset*10 + int64(r-'0')
	}
	return offset
}

func asToolError(err error, out **tool.Error) bool {
	for err != nil {
		if converted, ok := err.(*tool.Error); ok {
			*out = converted
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
