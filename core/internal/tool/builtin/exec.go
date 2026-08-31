package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
	"github.com/KoukeNeko/JingClaw/core/internal/sandbox"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// ExecCommand runs a program in the workspace.
//
// Arguments are passed as a list rather than a command line. A model producing
// text that gets handed to a shell is a quoting bug waiting to happen, and it
// makes the policy engine reason about a string instead of about what will
// actually be executed.
type ExecCommand struct {
	Workspace *workspace.Workspace

	Limits Limits

	// Artifacts is where output too large to show is kept. Left nil, a long
	// build log is simply cut in half and the middle is gone.
	Artifacts *artifact.Store

	// Env is the environment child processes receive. Left empty, a minimal
	// one is derived: inheriting the daemon's would hand every command its API
	// keys.
	Env []string

	// Confine says what a command may reach. Nil runs it unconfined, which is
	// what a deployment that has not asked for a sandbox gets and is what
	// this did before there was one.
	//
	// Set, it must work. A sandbox that runs the command anyway when it
	// cannot confine it is worse than none, because somebody believes there
	// is one — so an unavailable backend refuses the call rather than
	// quietly widening it.
	Confine *Confinement
}

// Confinement is the sandbox this deployment asked for.
type Confinement struct {
	// Policy is everything but the workspace, which changes per call.
	Policy sandbox.Policy

	// Environment points a command's caches and home at the sandbox's own,
	// so confining writes does not mean forbidding a compiler to build.
	Environment []string
}

func (t *ExecCommand) Spec() tool.Spec {
	return tool.Spec{
		Name: "exec_command",
		Description: "Run a program in the workspace and return its output. " +
			"Give the program and its arguments separately; there is no shell, so pipes, redirection and " +
			"variable expansion are not interpreted. Waits for the program to finish.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "program": {
      "type": "string",
      "minLength": 1,
      "description": "Executable to run, e.g. go, npm, git."
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
    "timeout_seconds": {
      "type": "integer",
      "minimum": 1,
      "maximum": 600,
      "description": "How long to wait before killing the program. Defaults to 120."
    }
  },
  "required": ["program"],
  "additionalProperties": false
}`),
		Level: tool.LevelExecute,
		Capabilities: tool.Capabilities{
			Provenance: domain.ProvenanceLocalUnknown,
			ReadFS:     true,
			WriteFS:    true,
			Execute:    true,
			// A command can do anything the user can, including reach the
			// network. Claiming otherwise would let a policy under-estimate it.
			Network:     true,
			Destructive: true,
		},
	}
}

type execArgs struct {
	Program        string   `json:"program"`
	Args           []string `json:"args"`
	Cwd            string   `json:"cwd"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (t *ExecCommand) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args execArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
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

	limits := t.Limits.withDefaults()

	timeout := limits.CommandTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > limits.MaxCommandTimeout {
			timeout = limits.MaxCommandTimeout
		}
	}

	// The timeout descends from the run's context, so interrupting a run also
	// kills whatever it started rather than leaving it running unattended.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	program, arguments := args.Program, args.Args
	if t.Confine != nil {
		// The workspace is added here rather than configured, because which
		// directory a command may write is a fact about this call.
		policy := t.Confine.Policy
		policy.Writable = append(append([]string(nil), policy.Writable...), t.Workspace.Root())

		wrapped, wrappedArgs, done, err := sandbox.Wrap(policy, program, arguments)
		if err != nil {
			// Refused rather than run. This is the failure the whole feature
			// turns on: an operator who asked for confinement and a machine
			// that cannot provide it must not get commands running as though
			// they had never asked.
			return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
				"Turn off [sandbox] enabled to run without confinement.",
				"cannot confine this command: %v", err)
		}
		defer done()

		program, arguments = wrapped, wrappedArgs
	}

	command := exec.CommandContext(runCtx, program, arguments...)
	command.Dir = workingDir
	command.Env = t.environment()

	// Killing the process alone leaves its children behind, holding the pipes
	// open and the port bound. The whole group has to go.
	configureProcessGroup(command)
	command.Cancel = func() error { return terminateGroup(command) }
	command.WaitDelay = 5 * time.Second

	var combined bytes.Buffer
	command.Stdout = &combined
	command.Stderr = &combined
	// A program that reads stdin would otherwise block forever waiting for
	// input nobody is going to type.
	command.Stdin = nil

	started := time.Now()
	runErr := command.Run()
	elapsed := time.Since(started)

	return t.result(ctx, args, combined.Bytes(), runErr, runCtx, elapsed, limits.MaxCommandOutput)
}

func (t *ExecCommand) result(
	ctx context.Context,
	args execArgs,
	rawOutput []byte,
	runErr error,
	runCtx context.Context,
	elapsed time.Duration,
	maxOutput int,
) (tool.Result, error) {
	display := commandLine(args)
	output, truncated := boundOutput(rawOutput, maxOutput)

	// A build log that fails at line 40,000 is the most useful thing in the
	// session and the least printable, so the whole of it is kept and the
	// model is told where.
	//
	// Stored before the error paths below, because a command that timed out
	// or exited non-zero is exactly the one whose output somebody wants.
	var stored *tool.Artifact
	if truncated {
		ref, err := archive(ctx, t.Artifacts, rawOutput, "text/plain")
		stored = ref
		output += noteArtifact(ref, err)
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		// Timeouts carry the output produced so far: a hung test suite usually
		// says where it hung before it stops saying anything.
		return tool.Result{}, &tool.Error{
			Code: tool.CodeTimeout,
			Message: fmt.Sprintf("%s did not finish within %s\n\n%s",
				display, elapsed.Round(time.Second), output),
			SuggestedAction: "Raise timeout_seconds, or run something narrower.",
			Retryable:       true,
		}
	}

	var notFound *exec.Error
	if errors.As(runErr, &notFound) {
		return tool.Result{}, tool.Errorf(tool.CodeNotFound,
			"Check the program name, or use one that is installed here.",
			"%s is not available on this machine", args.Program)
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// A non-zero exit is the answer to the question, not a malfunction: a
		// failing test suite is exactly what the agent asked to find out.
		return tool.Result{
			Content: fmt.Sprintf("%s\nexit status %d after %s\n\n%s",
				display, exitErr.ExitCode(), elapsed.Round(time.Millisecond), output),
			Summary:   fmt.Sprintf("%s: exit %d", display, exitErr.ExitCode()),
			IsError:   true,
			Truncated: truncated,
			Artifact:  stored,
		}, nil
	}

	if runErr != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%s: %v", display, runErr)
	}

	body := output
	if strings.TrimSpace(body) == "" {
		body = "(no output)"
	}

	return tool.Result{
		Content: fmt.Sprintf("%s\nexit status 0 after %s\n\n%s",
			display, elapsed.Round(time.Millisecond), body),
		Summary:       fmt.Sprintf("%s: ok", display),
		Truncated:     truncated,
		Artifact:      stored,
		OriginalBytes: int64(len(rawOutput)),
	}, nil
}

// environment builds the child's environment.
//
// The daemon's own environment holds provider credentials. Passing it through
// would hand an API key to every command the model decides to run, so only the
// variables a build actually needs are forwarded.
func (t *ExecCommand) environment() []string {
	base := t.Env
	if len(base) == 0 {
		base = minimalEnvironment(false)
	}
	if t.Confine == nil {
		return base
	}

	// The sandbox's own directories win. Every one of these has a default
	// under the real home, and left alone a compiler writes there and a
	// package manager reads credentials from beside it — which would make
	// hiding the real home either impossible or a list of exceptions.
	return append(base, t.Confine.Environment...)
}

// minimalEnvironment is what a child process is given.
//
// Derived rather than inherited: the daemon's own environment carries the API
// keys, and handing them to every program a model asks for would make one
// careless command an exfiltration.
//
// interactive says whether the program has a terminal. It changes what to
// claim: a program told TERM=dumb will not draw a prompt, which is exactly
// what a caller asking for a terminal wanted it to do.
func minimalEnvironment(interactive bool) []string {
	keep := []string{"PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP",
		"LANG", "LC_ALL", "SystemRoot", "ComSpec", "PATHEXT"}

	env := make([]string, 0, len(keep)+5)
	for _, name := range keep {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	if interactive {
		// A real terminal, so a program may draw prompts and colour. CI is
		// deliberately not set: the tools that read it are the ones that stop
		// asking questions, which is the opposite of why a terminal was asked
		// for.
		return append(env, "TERM=xterm-256color")
	}

	// Tools that phone home or open a pager are a poor fit for a
	// non-interactive caller with no terminal.
	return append(env, "CI=1", "TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat")
}

// boundOutput trims the middle of long output.
//
// The start says what ran and the end says how it ended; the middle of a build
// log is where the least information per byte lives.
func boundOutput(raw []byte, maxOutput int) (string, bool) {
	if len(raw) <= maxOutput {
		return string(raw), false
	}

	half := maxOutput / 2
	head := string(raw[:half])
	tail := string(raw[len(raw)-half:])

	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s",
		head, len(raw)-2*half, tail), true
}

func commandLine(args execArgs) string {
	parts := append([]string{args.Program}, args.Args...)
	return strings.Join(parts, " ")
}

func orDot(path string) string {
	if path == "" {
		return "."
	}
	return path
}

// ShellFor reports the interactive shell available on this platform, for a
// caller that genuinely needs pipes or redirection.
//
// Windows is a first-class target: an agent that requires bash there is an
// agent that does not run on Windows. Nothing uses this yet; it exists so the
// eventual shell mode does not assume a POSIX system.
func ShellFor() (program string, prefix []string, ok bool) {
	if runtime.GOOS == "windows" {
		for _, candidate := range []string{"pwsh.exe", "powershell.exe"} {
			if path, err := exec.LookPath(candidate); err == nil {
				return path, []string{"-NoProfile", "-NonInteractive", "-Command"}, true
			}
		}
		if path, err := exec.LookPath("cmd.exe"); err == nil {
			return path, []string{"/d", "/s", "/c"}, true
		}
		return "", nil, false
	}

	for _, candidate := range []string{os.Getenv("SHELL"), "/bin/zsh", "/bin/bash", "/bin/sh"} {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, []string{"-c"}, true
		}
	}
	return "", nil, false
}

// Preview is the command line this call would run.
//
// The arguments are a program and a list, which is the safe way to pass them
// and the hard way to read them. What somebody deciding wants to see is the
// line as a shell would have written it.
func (t *ExecCommand) Preview(arguments json.RawMessage) string {
	var args execArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	if args.Program == "" {
		return ""
	}

	line := commandLine(args)
	if args.Cwd != "" {
		return fmt.Sprintf("in %s:\n%s", args.Cwd, line)
	}
	return line
}
