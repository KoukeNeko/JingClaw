package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/process"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// The three tools here are one capability split by what a caller is doing:
// starting something that keeps running, talking to it, and ending it.
//
// Separate from exec_command rather than a flag on it, because the shape of
// the answer is different. exec_command answers "here is what it printed and
// how it ended"; these answer "it is running, and here is what it has said so
// far" — and a model handed one shape when it expected the other reads a
// still-running server as a finished command.

// StartProcess runs a program that keeps going after the call returns.
type StartProcess struct {
	Workspace *workspace.Workspace
	Processes *process.Manager

	// Env is what the program receives. Empty derives a minimal one; the
	// daemon's own would hand every program its API keys.
	Env []string
}

func (t *StartProcess) Spec() tool.Spec {
	return tool.Spec{
		Name: "start_process",
		Description: "Start a program that keeps running after this returns, and return its id. " +
			"For servers, watchers, REPLs and anything that asks a question partway through. " +
			"Use exec_command instead for anything that finishes on its own — a build, a test run, a git command. " +
			"Read what it printed with process_io and end it with stop_process; a process left running is " +
			"still running.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "program": {
      "type": "string",
      "minLength": 1,
      "description": "Executable to run, e.g. npm, python3, ssh."
    },
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Arguments, one per element. Do not put them all in one string."
    },
    "cwd": {
      "type": "string",
      "description": "Working directory relative to the workspace root. Defaults to the root."
    },
    "terminal": {
      "type": "boolean",
      "description": "Give it a terminal. Needed for anything that prompts: many programs buffer their output and refuse to prompt when they see a pipe. Costs escape sequences in the output, so leave it off when you only want to read what it prints."
    },
    "columns": {"type": "integer", "minimum": 20, "maximum": 500, "description": "Terminal width. Defaults to 120."},
    "rows": {"type": "integer", "minimum": 5, "maximum": 200, "description": "Terminal height. Defaults to 40."}
  },
  "required": ["program"],
  "additionalProperties": false
}`),
		Level: tool.LevelExecute,
		Capabilities: tool.Capabilities{
			ReadFS:  true,
			WriteFS: true,
			Execute: true,
			Network: true,
			// A program that outlives the run cannot be undone by the run
			// ending, which is more than exec_command claims and is the honest
			// floor for something that keeps a port bound after the turn.
			Destructive: true,
		},
	}
}

// environment is what the program receives, derived rather than inherited so
// that a long-running program does not hold the daemon's API keys for as long
// as it runs.
func (t *StartProcess) environment(interactive bool) []string {
	if len(t.Env) > 0 {
		return t.Env
	}
	return minimalEnvironment(interactive)
}

type startProcessArgs struct {
	Program  string   `json:"program"`
	Args     []string `json:"args"`
	Cwd      string   `json:"cwd"`
	Terminal bool     `json:"terminal"`
	Columns  int      `json:"columns"`
	Rows     int      `json:"rows"`
}

func (t *StartProcess) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args startProcessArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}
	if call.Context.SessionID == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"", "a process must belong to a session, and this call has none")
	}

	workingDir, err := t.Workspace.Resolve(args.Cwd)
	if err != nil {
		return tool.Result{}, pathError(orDot(args.Cwd), err)
	}
	if info, statErr := os.Stat(workingDir); statErr != nil || !info.IsDir() {
		return tool.Result{}, tool.Errorf(tool.CodeNotFound,
			"Give a directory inside the workspace, or omit cwd for the root.",
			"%s is not a directory in the workspace", orDot(args.Cwd))
	}

	state, err := t.Processes.Start(process.StartOptions{
		SessionID: call.Context.SessionID,
		Program:   args.Program,
		Args:      args.Args,
		Dir:       workingDir,
		Env:       t.environment(args.Terminal),
		Terminal:  args.Terminal,
		Columns:   args.Columns,
		Rows:      args.Rows,
	})
	if err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal,
			"Check the program name, or use one that is installed here.",
			"%v", err)
	}

	return tool.Result{
		Content: describeStarted(state, args),
		Summary: fmt.Sprintf("started %s as %s", args.Program, state.ID),
	}, nil
}

func describeStarted(state process.State, args startProcessArgs) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s is running as %s (pid %d).\n", args.Program, state.ID, state.PID)

	if args.Terminal && !state.Terminal {
		// Said rather than hidden. A caller that believes it has a terminal
		// waits for a prompt a block-buffered program is never going to send.
		out.WriteString("No terminal was available on this platform, so it is attached to pipes; " +
			"a program that prompts may not show its prompt.\n")
	}

	out.WriteString("It has produced no output yet. Read it with process_io, which returns " +
		"only what is new since the offset you give it.")
	return out.String()
}

// ProcessIO reads what a process has printed and writes to it.
//
// One tool rather than two, because the two halves are one conversation: a
// caller writes an answer and then reads what came of it, and splitting them
// means the model has to remember to do both.
type ProcessIO struct {
	Processes *process.Manager
}

func (t *ProcessIO) Spec() tool.Spec {
	return tool.Spec{
		Name: "process_io",
		Description: "Read what a running process has printed, and optionally send it input first. " +
			"Returns only what is new since the offset you give, along with the offset to use next; " +
			"pass 0 the first time. Input is sent exactly as given, so end a line with \\n if the " +
			"program is waiting for one.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "process_id": {"type": "string", "minLength": 1, "description": "The id start_process returned."},
    "input": {
      "type": "string",
      "description": "Sent to the program before reading. Include a trailing newline if it is waiting for a line."
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "description": "Read output from here. Use the next_offset from the previous call; 0 reads from the beginning."
    },
    "wait_seconds": {
      "type": "integer",
      "minimum": 0,
      "maximum": 60,
      "description": "Wait this long for new output before answering. Use it after sending input rather than calling repeatedly. Defaults to 0."
    }
  },
  "required": ["process_id"],
  "additionalProperties": false
}`),
		Level: tool.LevelExecute,
		Capabilities: tool.Capabilities{
			// Writing to a process is making a program act, which is
			// execution however small the string is.
			Execute: true,
			Network: true,
		},
	}
}

type processIOArgs struct {
	ProcessID   string `json:"process_id"`
	Input       string `json:"input"`
	Offset      int64  `json:"offset"`
	WaitSeconds int    `json:"wait_seconds"`
}

func (t *ProcessIO) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args processIOArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	id := process.ID(args.ProcessID)
	if _, err := t.Processes.Get(id); err != nil {
		return tool.Result{}, processError(err, args.ProcessID)
	}

	if args.Input != "" {
		if err := t.Processes.Write(id, args.Input); err != nil {
			return tool.Result{}, processError(err, args.ProcessID)
		}
	}

	output, next, skipped, err := t.waitForOutput(ctx, id, args)
	if err != nil {
		return tool.Result{}, processError(err, args.ProcessID)
	}

	state, err := t.Processes.Get(id)
	if err != nil {
		return tool.Result{}, processError(err, args.ProcessID)
	}

	return tool.Result{
		Content: describeOutput(state, output, next, skipped),
		Summary: summariseOutput(state, output),
	}, nil
}

// waitForOutput answers once there is something new, or once the wait is over.
//
// Polled rather than signalled: the alternative is a channel per reader, and
// the thing being waited on is a program that has already been asked a
// question — a fifth of a second late is not a cost anybody can measure.
func (t *ProcessIO) waitForOutput(
	ctx context.Context,
	id process.ID,
	args processIOArgs,
) (string, int64, int64, error) {
	const pollInterval = 200 * time.Millisecond

	output, next, skipped, err := t.Processes.Read(id, args.Offset)
	if err != nil || output != "" || args.WaitSeconds == 0 {
		return output, next, skipped, err
	}

	deadline := time.Now().Add(time.Duration(args.WaitSeconds) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return output, next, skipped, nil
		case <-time.After(pollInterval):
		}

		output, next, skipped, err = t.Processes.Read(id, args.Offset)
		if err != nil {
			return "", 0, 0, err
		}
		if output != "" {
			return output, next, skipped, nil
		}

		// A program that has ended will not say anything more, so waiting out
		// the rest of the interval is time spent on nothing.
		if state, getErr := t.Processes.Get(id); getErr == nil && !state.Running {
			return output, next, skipped, nil
		}
	}
	return output, next, skipped, nil
}

func describeOutput(state process.State, output string, next, skipped int64) string {
	var out strings.Builder

	if skipped > 0 {
		fmt.Fprintf(&out, "[%d bytes of earlier output were dropped; the buffer keeps only the most recent]\n",
			skipped)
	}
	if output == "" {
		out.WriteString("[no new output]\n")
	} else {
		out.WriteString(output)
		if !strings.HasSuffix(output, "\n") {
			out.WriteString("\n")
		}
	}

	if state.Running {
		fmt.Fprintf(&out, "\n[still running; next_offset %d]", next)
	} else {
		fmt.Fprintf(&out, "\n[ended with exit code %d; next_offset %d]", state.ExitCode, next)
	}
	return out.String()
}

func summariseOutput(state process.State, output string) string {
	status := "running"
	if !state.Running {
		status = fmt.Sprintf("exited %d", state.ExitCode)
	}
	return fmt.Sprintf("%s: %d bytes, %s", state.ID, len(output), status)
}

// StopProcess ends a running program.
type StopProcess struct {
	Processes *process.Manager
}

func (t *StopProcess) Spec() tool.Spec {
	return tool.Spec{
		Name: "stop_process",
		Description: "End a running process and everything it started. " +
			"It is asked to stop first and killed if it does not.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "process_id": {"type": "string", "minLength": 1, "description": "The id start_process returned."}
  },
  "required": ["process_id"],
  "additionalProperties": false
}`),
		Level: tool.LevelExecute,
		Capabilities: tool.Capabilities{
			Execute: true,
			// Ending a program can lose what it had not written out.
			Destructive: true,
		},
	}
}

type stopProcessArgs struct {
	ProcessID string `json:"process_id"`
}

func (t *StopProcess) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args stopProcessArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	state, err := t.Processes.Stop(process.ID(args.ProcessID))
	if err != nil {
		return tool.Result{}, processError(err, args.ProcessID)
	}

	return tool.Result{
		Content: fmt.Sprintf("%s has ended (exit code %d).", state.ID, state.ExitCode),
		Summary: fmt.Sprintf("stopped %s", state.ID),
	}, nil
}

// processError turns a manager error into one a model can act on.
func processError(err error, id string) error {
	switch {
	case errors.Is(err, process.ErrNotFound):
		return tool.Errorf(tool.CodeNotFound,
			"Use the id start_process returned. A process that has been stopped is gone.",
			"there is no process %s", id)
	case errors.Is(err, process.ErrClosed):
		return tool.Errorf(tool.CodeInvalidArguments,
			"Read its remaining output with process_io, then start a new one if you need it.",
			"%s has already ended, so nothing can be sent to it", id)
	default:
		return tool.Errorf(tool.CodeInternal, "", "%v", err)
	}
}
