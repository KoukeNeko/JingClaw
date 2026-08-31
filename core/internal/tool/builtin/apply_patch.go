package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// ApplyPatch changes several files as one thing.
//
// Not an alternative to edit_file. A model making one change should not have
// to build a patch, and a model making six related changes should not have to
// make six calls — each with its own approval, each landing separately, and
// the workspace passing through five states nobody asked for.
//
// The reason it is worth its own tool rather than a loop over edit_file: it
// works out every file before it writes any of them. A rename that moved a
// function and left its callers pointing at nothing is not something a person
// should have to approve one file at a time and then watch fail halfway.
type ApplyPatch struct {
	Workspace *workspace.Workspace
	Observer  *Observer
	Locks     *keyedMutex
}

func NewApplyPatch(ws *workspace.Workspace, observer *Observer, locks *keyedMutex) *ApplyPatch {
	return &ApplyPatch{Workspace: ws, Observer: observer, Locks: locks}
}

// maxPatchFiles is where a patch stops being a change and starts being a
// rewrite. A person asked to approve forty files at once approves without
// reading, which makes the approval worse than none.
const maxPatchFiles = 20

func (t *ApplyPatch) Spec() tool.Spec {
	return tool.Spec{
		Name: "apply_patch",
		Description: "Change several files as one operation: create, update or delete. " +
			"Every file is worked out before any of them is written, so a patch that cannot " +
			"apply cleanly changes nothing at all. Use it for a change that spans files — " +
			"renaming something and fixing its callers, adding a type and using it. " +
			"Use edit_file for a change to one file. " +
			"Updating a file needs it to have been read first, as edit_file does.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "minItems": 1,
      "description": "The files to change, applied together.",
      "items": {
        "type": "object",
        "properties": {
          "op": {
            "type": "string",
            "enum": ["create", "update", "delete"],
            "description": "create makes a new file; update replaces exact text in one that exists; delete removes one."
          },
          "path": {"type": "string", "minLength": 1, "description": "Path relative to the workspace root."},
          "content": {"type": "string", "description": "The whole file, for create."},
          "edits": {
            "type": "array",
            "minItems": 1,
            "description": "Exact replacements, for update. Each old_text must appear exactly once.",
            "items": {
              "type": "object",
              "properties": {
                "old_text": {"type": "string", "minLength": 1, "description": "Text to replace. Must be unique in the file."},
                "new_text": {"type": "string", "description": "What replaces it. Empty removes the text."}
              },
              "required": ["old_text", "new_text"],
              "additionalProperties": false
            }
          }
        },
        "required": ["op", "path"],
        "additionalProperties": false
      }
    }
  },
  "required": ["operations"],
  "additionalProperties": false
}`),
		Level: tool.LevelWorkspaceWrite,
		Capabilities: tool.Capabilities{
			// The result is a diff, which is workspace text coming back. Not
			// the operator's words, whatever the operator asked for.
			Provenance: domain.ProvenanceLocalUnknown,

			ReadFS:  true,
			WriteFS: true,
			// A patch can delete, and a deletion is not undone by running the
			// same call again.
			Destructive: true,
			Idempotent:  false,
		},
	}
}

type patchOperation struct {
	Op      string     `json:"op"`
	Path    string     `json:"path"`
	Content string     `json:"content"`
	Edits   []textEdit `json:"edits"`
}

type applyPatchArgs struct {
	Operations []patchOperation `json:"operations"`
}

func (t *ApplyPatch) Execute(_ context.Context, call tool.Call) (tool.Result, error) {
	var args applyPatchArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}
	if len(args.Operations) == 0 {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Give at least one operation.", "the patch is empty")
	}
	if len(args.Operations) > maxPatchFiles {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments,
			"Split it into patches somebody could read before approving one.",
			"a patch may touch %d files, and this one touches %d",
			maxPatchFiles, len(args.Operations))
	}
	if err := checkOnePerFile(args.Operations); err != nil {
		return tool.Result{}, err
	}

	engine := t.engine()

	// Every file this patch touches is locked before any of it is read, and
	// held until all of it is written. Locking each in turn as it is reached
	// would let another edit land between the file this patch has already
	// planned and the one it has not.
	release, err := t.lockAll(args.Operations)
	if err != nil {
		return tool.Result{}, err
	}
	defer release()

	planned := make([]plannedWrite, 0, len(args.Operations))
	for _, operation := range args.Operations {
		one, problem := t.plan(engine, operation)
		if problem != nil {
			// Nothing has been written. A patch that cannot apply cleanly
			// changes nothing at all, which is the whole reason this is one
			// call rather than several.
			return tool.Result{}, problem
		}
		planned = append(planned, one)
	}

	applied, err := engine.commit(planned)
	if err != nil {
		// Past the point of no return: some files are written and some are
		// not. Said plainly, with what did land, because a model told only
		// "it failed" would try the whole patch again on a workspace that is
		// already half changed.
		return tool.Result{}, tool.Errorf(tool.CodeInternal,
			"Read the files named above before changing anything else; the workspace is part-way through this patch.",
			"the patch failed part-way: %v. Applied: %s", err, names(applied))
	}

	return tool.Result{
		Content: renderPatch(applied),
		Summary: summarisePatch(applied),
	}, nil
}

func (t *ApplyPatch) engine() *editEngine {
	return &editEngine{workspace: t.Workspace, observer: t.Observer, locks: t.Locks}
}

// plan works out one operation without writing anything.
func (t *ApplyPatch) plan(engine *editEngine, operation patchOperation) (plannedWrite, *tool.Error) {
	switch operation.Op {
	case "update":
		if len(operation.Edits) == 0 {
			return plannedWrite{}, tool.Errorf(tool.CodeInvalidArguments,
				"Give the replacements to make, or use create to replace the whole file.",
				"the update to %s has no edits", operation.Path)
		}
		return engine.prepareEdit(operation.Path, operation.Edits)

	case "create":
		return t.planCreate(operation)

	case "delete":
		return t.planDelete(engine, operation)

	default:
		return plannedWrite{}, tool.Errorf(tool.CodeInvalidArguments,
			`Use "create", "update" or "delete".`,
			"%q is not something that can be done to a file", operation.Op)
	}
}

func (t *ApplyPatch) planCreate(operation patchOperation) (plannedWrite, *tool.Error) {
	absolute, err := t.Workspace.Resolve(operation.Path)
	if err != nil {
		return plannedWrite{}, pathError(operation.Path, err)
	}

	// Refused rather than overwriting. "Create" says the model believes this
	// file is not there, and a create that silently replaced somebody's work
	// would be the worst kind of surprise: invisible, and attributed to a
	// patch that said it was adding something.
	if _, statErr := os.Stat(absolute); statErr == nil {
		return plannedWrite{}, tool.Errorf(tool.CodeInvalidArguments,
			"Use update to change a file that exists, after reading it.",
			"%s already exists", operation.Path)
	}

	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return plannedWrite{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	return plannedWrite{
		relative: operation.Path,
		absolute: absolute,
		content:  []byte(operation.Content),
		create:   true,
		changes:  []changedRegion{{Line: 1, NewText: operation.Content}},
	}, nil
}

func (t *ApplyPatch) planDelete(engine *editEngine, operation patchOperation) (plannedWrite, *tool.Error) {
	absolute, err := t.Workspace.Resolve(operation.Path)
	if err != nil {
		return plannedWrite{}, pathError(operation.Path, err)
	}

	raw, readErr := os.ReadFile(absolute)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return plannedWrite{}, tool.Errorf(tool.CodeNotFound,
				"", "%s does not exist", operation.Path)
		}
		return plannedWrite{}, tool.Errorf(tool.CodeInternal, "", "%v", readErr)
	}

	// The same rule updating has, and for a stronger reason: deleting a file
	// the agent has not read, or has read and which has since changed, throws
	// away work nothing here can recover.
	if problem := engine.verifySeen(operation.Path, absolute, raw); problem != nil {
		return plannedWrite{}, problem
	}

	return plannedWrite{
		relative: operation.Path,
		absolute: absolute,
		delete:   true,
	}, nil
}

// lockAll takes the lock for every file the patch touches, in a fixed order.
//
// Sorted, because two patches taking the same locks in different orders is a
// deadlock — and two runs editing overlapping files is exactly the case this
// is here for.
func (t *ApplyPatch) lockAll(operations []patchOperation) (func(), error) {
	paths := make([]string, 0, len(operations))
	for _, operation := range operations {
		absolute, err := t.Workspace.Resolve(operation.Path)
		if err != nil {
			return nil, pathError(operation.Path, err)
		}
		paths = append(paths, absolute)
	}
	sort.Strings(paths)

	releases := make([]func(), 0, len(paths))
	for _, path := range paths {
		releases = append(releases, t.Locks.Lock(path))
	}

	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

// checkOnePerFile refuses a patch that touches the same file twice.
//
// Two operations on one file cannot both be planned against the same content:
// the second would be worked out against the file as it is on disk rather
// than as the first will leave it, and would then be applied on top. Refused
// rather than ordered, because "which one wins" is not a question the model
// meant to ask.
func checkOnePerFile(operations []patchOperation) *tool.Error {
	seen := map[string]bool{}
	for _, operation := range operations {
		path := filepath.Clean(operation.Path)
		if seen[path] {
			return tool.Errorf(tool.CodeInvalidArguments,
				"Put every change to one file in a single update.",
				"%s appears twice in this patch", operation.Path)
		}
		seen[path] = true
	}
	return nil
}

func names(applied []plannedWrite) string {
	if len(applied) == 0 {
		return "nothing"
	}
	out := make([]string, 0, len(applied))
	for _, one := range applied {
		out = append(out, one.relative)
	}
	return strings.Join(out, ", ")
}

func renderPatch(applied []plannedWrite) string {
	if len(applied) == 0 {
		return "Nothing changed: every replacement produced identical text."
	}

	var out strings.Builder
	for i, one := range applied {
		if i > 0 {
			out.WriteString("\n")
		}
		switch {
		case one.delete:
			fmt.Fprintf(&out, "deleted %s\n", one.relative)
		case one.create:
			fmt.Fprintf(&out, "created %s (%d bytes)\n", one.relative, len(one.content))
		default:
			out.WriteString(renderDiff(one.relative, one.changes))
		}
	}
	return out.String()
}

func summarisePatch(applied []plannedWrite) string {
	var created, updated, deleted int
	for _, one := range applied {
		switch {
		case one.delete:
			deleted++
		case one.create:
			created++
		default:
			updated++
		}
	}

	parts := make([]string, 0, 3)
	if created > 0 {
		parts = append(parts, fmt.Sprintf("%d created", created))
	}
	if updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", updated))
	}
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", deleted))
	}
	if len(parts) == 0 {
		return "patch (no change)"
	}
	return "patch: " + strings.Join(parts, ", ")
}

// Preview is what this patch would do, for somebody deciding whether to allow
// it.
//
// The whole change across every file. This is the tool whose arguments are
// least readable as they stand — several files of old and new text — and the
// one where the preview matters most, because approving it approves all of
// them at once.
func (t *ApplyPatch) Preview(arguments json.RawMessage) string {
	var args applyPatchArgs
	if err := json.Unmarshal(arguments, &args); err != nil {
		return ""
	}
	if len(args.Operations) == 0 {
		return ""
	}

	var out strings.Builder
	for i, operation := range args.Operations {
		if i > 0 {
			out.WriteString("\n")
		}

		switch operation.Op {
		case "delete":
			fmt.Fprintf(&out, "--- %s (deleted)\n", operation.Path)

		case "create":
			fmt.Fprintf(&out, "+++ %s (new)\n", operation.Path)
			writePrefixed(&out, "+", boundPreview(operation.Content))

		default:
			fmt.Fprintf(&out, "--- %s\n+++ %s\n", operation.Path, operation.Path)
			for index, edit := range operation.Edits {
				fmt.Fprintf(&out, "@@ change %d @@\n", index+1)
				writePrefixed(&out, "-", edit.OldText)
				writePrefixed(&out, "+", edit.NewText)
			}
		}
	}

	return out.String()
}
