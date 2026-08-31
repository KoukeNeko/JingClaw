// Package builtin holds the tools shipped with JingClaw.
package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"io"
	"os"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// ReadFile reads a range of lines from a workspace file.
type ReadFile struct {
	Workspace *workspace.Workspace

	// Observer records what was seen, which is what later lets a write refuse
	// to clobber a file the agent never looked at.
	Observer *Observer

	Limits Limits
}

func (t *ReadFile) Spec() tool.Spec {
	return tool.Spec{
		Name: "read_file",
		Description: "Read a text file from the workspace. Returns numbered lines. " +
			"Prefer a line range for large files; use grep first to find the region worth reading.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path relative to the workspace root, e.g. internal/runtime/runtime.go"
    },
    "start_line": {
      "type": "integer",
      "minimum": 1,
      "description": "First line to return, 1-based. Defaults to the start of the file."
    },
    "end_line": {
      "type": "integer",
      "minimum": 1,
      "description": "Last line to return, inclusive. Defaults to the end of the file."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceRead,
		Capabilities: tool.Capabilities{
			Provenance:   domain.ProvenanceLocalUnknown,
			ReadFS:       true,
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func (t *ReadFile) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args readFileArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	absolute, err := t.Workspace.Resolve(args.Path)
	if err != nil {
		return tool.Result{}, pathError(args.Path, err)
	}

	info, err := os.Stat(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return tool.Result{}, tool.Errorf(tool.CodeNotFound,
				"Use glob_files to find the correct path.",
				"%s does not exist", args.Path)
		}
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}
	if info.IsDir() {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Use glob_files to list a directory.",
			"%s is a directory", args.Path)
	}
	limits := t.Limits.withDefaults()
	if info.Size() > limits.MaxReadableFile {
		return tool.Result{}, tool.Errorf(tool.CodeTooLarge,
			"Use grep to locate the relevant lines, then read that range.",
			"%s is %d bytes, over the %d byte read limit", args.Path, info.Size(), limits.MaxReadableFile)
	}

	raw, err := os.ReadFile(absolute)
	if err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if args.EndLine > 0 && args.StartLine > args.EndLine {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Set start_line no greater than end_line.",
			"start_line %d is after end_line %d", args.StartLine, args.EndLine)
	}

	result, err := readLines(ctx, bytes.NewReader(raw), args, info.Size(), limits.ReadLimit)
	if err != nil {
		return result, err
	}

	// Record the whole file's hash even when only a range was returned: the
	// question a later write asks is whether the file changed, not whether the
	// part that was read did.
	if t.Observer != nil {
		t.Observer.Observe(absolute, hashBytes(raw))
	}

	return result, nil
}

func readLines(ctx context.Context, source io.Reader, args readFileArgs, size, readLimit int64) (tool.Result, error) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	start := args.StartLine
	if start < 1 {
		start = 1
	}

	var (
		out       strings.Builder
		lineNo    int
		emitted   int
		truncated bool
	)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return tool.Result{}, err
		}

		lineNo++
		if lineNo < start {
			continue
		}
		if args.EndLine > 0 && lineNo > args.EndLine {
			truncated = true
			break
		}

		// Detecting binary content by NUL is crude but catches the case that
		// matters: a model asking to read a compiled artifact and filling its
		// context with noise.
		line := scanner.Text()
		if strings.ContainsRune(line, 0) {
			return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
				"This is a binary file; it cannot be read as text.",
				"%s contains binary data", args.Path)
		}

		if int64(out.Len()+len(line)) > readLimit {
			truncated = true
			break
		}

		fmt.Fprintf(&out, "%6d\t%s\n", lineNo, line)
		emitted++
	}

	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
				"Use grep on this file instead.",
				"%s has a line longer than the read limit", args.Path)
		}
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if emitted == 0 {
		return tool.Result{
			Content: fmt.Sprintf("%s has no lines in the requested range.", args.Path),
			Summary: fmt.Sprintf("read %s (empty range)", args.Path),
		}, nil
	}

	content := out.String()
	if truncated {
		// Saying so matters: a model that believes it saw the whole file will
		// confidently reason about code that was never shown to it.
		content += fmt.Sprintf("\n[truncated after line %d; request a later range to continue]\n", start+emitted-1)
	}

	return tool.Result{
		Content:       content,
		Summary:       fmt.Sprintf("read %s (%d lines)", args.Path, emitted),
		Truncated:     truncated,
		OriginalBytes: size,
	}, nil
}

// pathError turns a workspace rejection into an observation the model can act
// on, without echoing back a resolved absolute path it has no business seeing.
func pathError(path string, err error) *tool.Error {
	switch {
	case errors.Is(err, workspace.ErrOutsideWorkspace):
		return tool.Errorf(tool.CodeOutsideWorkspace,
			"Use a path relative to the workspace root.",
			"%s is outside the workspace", path)
	case errors.Is(err, workspace.ErrNotFound):
		return tool.Errorf(tool.CodeNotFound,
			"Use glob_files to find the correct path.",
			"%s does not exist", path)
	default:
		return tool.Errorf(tool.CodeInvalidArguments, "", "%s: %v", path, err)
	}
}
