package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// Ambiguity is reported with a few examples rather than every location, which
// on a common substring would be thousands.
const maxAmbiguityReported = 5

// EditFile replaces exact regions of a file.
//
// The safety of this tool rests on one rule: a replacement whose target is not
// unique fails. Guessing which of several identical passages the model meant
// is how an agent silently corrupts a file, and no amount of cleverness
// recovers from that afterwards.
type EditFile struct {
	Workspace *workspace.Workspace
	Observer  *Observer

	// Locks serialises read-verify-write per file. Without it, two edits to
	// the same file can each verify against the original and the second
	// silently discards the first.
	Locks *keyedMutex
}

func NewEditFile(ws *workspace.Workspace, observer *Observer, locks *keyedMutex) *EditFile {
	return &EditFile{Workspace: ws, Observer: observer, Locks: locks}
}

func (t *EditFile) Spec() tool.Spec {
	return tool.Spec{
		Name: "edit_file",
		Description: "Replace exact regions of a file that has already been read. " +
			"Each old_text must appear exactly once: include enough surrounding lines to make it unique. " +
			"Give the file's own text, without the line numbers read_file adds.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path relative to the workspace root."
    },
    "edits": {
      "type": "array",
      "minItems": 1,
      "description": "Replacements applied in order.",
      "items": {
        "type": "object",
        "properties": {
          "old_text": {
            "type": "string",
            "minLength": 1,
            "description": "Exact text to replace, including indentation. Must occur exactly once."
          },
          "new_text": {
            "type": "string",
            "description": "Replacement text. Empty deletes the region."
          }
        },
        "required": ["old_text", "new_text"],
        "additionalProperties": false
      }
    }
  },
  "required": ["path", "edits"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceWrite,
		Capabilities: tool.Capabilities{
			ReadFS:      true,
			WriteFS:     true,
			Destructive: true,
			// Applying the same edit twice fails on the second attempt, since
			// its target no longer exists.
			Idempotent: false,
		},
	}
}

type editFileArgs struct {
	Path  string     `json:"path"`
	Edits []textEdit `json:"edits"`
}

func (t *EditFile) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args editFileArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	engine := t.engine()

	absolute, err := t.Workspace.Resolve(args.Path)
	if err != nil {
		return tool.Result{}, pathError(args.Path, err)
	}

	// Everything from here to the write happens under one lock, so a
	// concurrent edit cannot slip between the check and the change.
	release := t.Locks.Lock(absolute)
	defer release()

	planned, problem := engine.prepareEdit(args.Path, args.Edits)
	if problem != nil {
		return tool.Result{}, problem
	}

	if len(planned.changes) == 0 {
		return tool.Result{
			Content: fmt.Sprintf("%s is unchanged: every replacement produced identical text.", args.Path),
			Summary: fmt.Sprintf("edit %s (no change)", args.Path),
		}, nil
	}

	if _, err := engine.commit([]plannedWrite{planned}); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	return tool.Result{
		Content: renderDiff(args.Path, planned.changes),
		Summary: fmt.Sprintf("edit %s (%d change(s))", args.Path, len(planned.changes)),
	}, nil
}

func (t *EditFile) engine() *editEngine {
	return &editEngine{workspace: t.Workspace, observer: t.Observer, locks: t.Locks}
}

type appliedEdit struct {
	body   string
	region changedRegion
}

type changedRegion struct {
	Line    int
	OldText string
	NewText string
}

func applyEdit(path string, index int, body, oldText, newText string) (appliedEdit, *tool.Error) {
	matches := countOccurrences(body, oldText, maxAmbiguityReported)

	switch {
	case matches == 0:
		return appliedEdit{}, tool.Errorf(tool.CodeNotFound,
			"Read the file again and copy the exact text, including indentation.",
			"edit %d: the text to replace was not found in %s", index+1, path)

	case matches > 1:
		// Picking one would be a guess, and a wrong guess corrupts the file in
		// a way nothing downstream can detect.
		return appliedEdit{}, tool.Errorf(tool.CodeInvalidArguments,
			"Include more surrounding lines so the target is unique.",
			"edit %d: the text to replace appears %s in %s",
			index+1, describeCount(matches), path)
	}

	offset := strings.Index(body, oldText)
	return appliedEdit{
		body: body[:offset] + newText + body[offset+len(oldText):],
		region: changedRegion{
			Line:    lineOf(body, offset),
			OldText: oldText,
			NewText: newText,
		},
	}, nil
}

func describeCount(matches int) string {
	if matches > maxAmbiguityReported {
		return fmt.Sprintf("more than %d times", maxAmbiguityReported)
	}
	return fmt.Sprintf("%d times", matches)
}

// renderDiff shows what changed.
//
// The exact old and new text are already known, so this reports them directly
// rather than reconstructing a diff. Every successful edit produces something
// reviewable; a tool that reports only "done" leaves a human no way to check
// the agent's work short of reading the file themselves.
func renderDiff(path string, changes []changedRegion) string {
	var out strings.Builder
	fmt.Fprintf(&out, "edited %s\n", path)

	for i, change := range changes {
		fmt.Fprintf(&out, "\n@@ change %d at line %d @@\n", i+1, change.Line)
		writePrefixed(&out, "-", change.OldText)
		writePrefixed(&out, "+", change.NewText)
	}

	return out.String()
}

func writePrefixed(out *strings.Builder, prefix, text string) {
	if text == "" {
		return
	}

	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		fmt.Fprintf(out, "%s%s\n", prefix, line)
	}
}

// Preview is the change this call would make, as a diff.
//
// The arguments carry the old and new text in full, so nothing needs to be
// read from disk and nothing is: this runs before anybody has decided, and a
// preview that touched the workspace would be the edit happening without
// approval. What it therefore cannot show is context around the change, or
// whether the target is even present — those are the edit's own job, and it
// refuses on both counts.
func (t *EditFile) Preview(arguments json.RawMessage) string {
	var args editFileArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	if len(args.Edits) == 0 {
		return ""
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", args.Path, args.Path)

	for i, edit := range args.Edits {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "@@ change %d @@\n", i+1)
		writePrefixed(&out, "-", edit.OldText)
		writePrefixed(&out, "+", edit.NewText)
	}

	return out.String()
}
