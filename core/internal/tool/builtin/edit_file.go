package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	Path  string `json:"path"`
	Edits []struct {
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	} `json:"edits"`
}

func (t *EditFile) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args editFileArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	absolute, err := t.Workspace.Resolve(args.Path)
	if err != nil {
		return tool.Result{}, pathError(args.Path, err)
	}

	// Everything from here to the write happens under one lock, so a
	// concurrent edit cannot slip between the check and the change.
	release := t.Locks.Lock(absolute)
	defer release()

	raw, err := os.ReadFile(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return tool.Result{}, tool.Errorf(tool.CodeNotFound,
				"Use write_file to create a new file.",
				"%s does not exist", args.Path)
		}
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if err := t.verifyObserved(args.Path, absolute, raw); err != nil {
		return tool.Result{}, err
	}

	file, ok := parseTextFile(raw)
	if !ok {
		return tool.Result{}, tool.Errorf(tool.CodeUnsupported,
			"This file is not UTF-8 text and cannot be edited as text.",
			"%s is not valid UTF-8", args.Path)
	}

	body := file.Body
	changes := make([]changedRegion, 0, len(args.Edits))

	for index, edit := range args.Edits {
		// Newlines are normalised on both sides. A model working from
		// read_file output cannot tell whether the file uses CRLF, so
		// requiring it to guess would make every edit to a Windows file fail
		// for a reason it cannot see or fix.
		oldText := strings.ReplaceAll(edit.OldText, "\r\n", "\n")
		newText := strings.ReplaceAll(edit.NewText, "\r\n", "\n")

		applied, err := applyEdit(args.Path, index, body, oldText, newText)
		if err != nil {
			return tool.Result{}, err
		}

		changes = append(changes, applied.region)
		body = applied.body
	}

	if body == file.Body {
		return tool.Result{
			Content: fmt.Sprintf("%s is unchanged: every replacement produced identical text.", args.Path),
			Summary: fmt.Sprintf("edit %s (no change)", args.Path),
		}, nil
	}

	rendered := file.Render(body)
	if err := atomicWrite(absolute, []byte(rendered)); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	// What was read is no longer what is on disk.
	t.Observer.Observe(absolute, hashBytes([]byte(rendered)))

	return tool.Result{
		Content: renderDiff(args.Path, changes),
		Summary: fmt.Sprintf("edit %s (%d change(s))", args.Path, len(changes)),
	}, nil
}

// verifyObserved enforces that the agent has seen the file, and seen it as it
// currently is.
func (t *EditFile) verifyObserved(relative, absolute string, raw []byte) *tool.Error {
	observed, seen := t.Observer.Seen(absolute)
	if !seen {
		return tool.Errorf(tool.CodePermissionDenied,
			"Read the file first, then edit it.",
			"%s has not been read in this session", relative)
	}

	if current := hashBytes(raw); current != observed {
		// A human, an editor, or another agent changed the file after it was
		// read. Editing now would apply a change reasoned about against
		// content that no longer exists.
		return tool.Errorf(tool.CodeInvalidArguments,
			"Read the file again to see the current contents, then edit it.",
			"%s changed on disk since it was read", relative)
	}

	return nil
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
