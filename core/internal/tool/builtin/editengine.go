package builtin

import (
	"fmt"
	"os"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// The engine both editing tools go through.
//
// edit_file changes one file and apply_patch changes several, and the plan
// asks for both: a model that has to make one change should not have to build
// a patch, and one making six related changes should not have to make six
// calls each with its own approval.
//
// They share this because the rules are the safety of the thing and must not
// drift: a replacement whose target is not unique fails, a file that changed
// since it was read fails, and a file that was never read fails. Two
// implementations of that is one implementation and one liability.

// editEngine resolves, checks and rewrites files.
type editEngine struct {
	workspace *workspace.Workspace
	observer  *Observer
	locks     *keyedMutex
}

// plannedWrite is a change worked out but not yet made.
//
// Worked out first, for every file, before anything is written. A patch that
// applied three files and then failed on the fourth would leave a workspace
// in a state neither the model nor the person expects, and the model's next
// read would get the mixture.
type plannedWrite struct {
	relative string
	absolute string

	// content is the whole file as it will be, already rendered back into
	// the line endings and trailing newline it had.
	content []byte

	// changes is what to show a person, and delete says the file goes.
	changes []changedRegion
	delete  bool
	create  bool
}

// prepareEdit works out what one file becomes, without writing it.
//
// The caller holds the lock for the file: this reads and computes, and the
// window between reading and writing is exactly what the lock exists to
// close.
func (e *editEngine) prepareEdit(relative string, edits []textEdit) (plannedWrite, *tool.Error) {
	absolute, err := e.workspace.Resolve(relative)
	if err != nil {
		return plannedWrite{}, pathError(relative, err)
	}

	raw, err := os.ReadFile(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return plannedWrite{}, tool.Errorf(tool.CodeNotFound,
				"Use a create operation, or write_file, to make a new file.",
				"%s does not exist", relative)
		}
		return plannedWrite{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if problem := e.verifySeen(relative, absolute, raw); problem != nil {
		return plannedWrite{}, problem
	}

	file, ok := parseTextFile(raw)
	if !ok {
		return plannedWrite{}, tool.Errorf(tool.CodeUnsupported,
			"This file is not UTF-8 text and cannot be edited as text.",
			"%s is not valid UTF-8", relative)
	}

	body := file.Body
	changes := make([]changedRegion, 0, len(edits))

	for index, edit := range edits {
		// Newlines are normalised on both sides. A model working from
		// read_file output cannot tell whether the file uses CRLF, so
		// requiring it to guess would make every edit to a Windows file fail
		// for a reason it cannot see or fix.
		oldText := strings.ReplaceAll(edit.OldText, "\r\n", "\n")
		newText := strings.ReplaceAll(edit.NewText, "\r\n", "\n")

		applied, problem := applyEdit(relative, index, body, oldText, newText)
		if problem != nil {
			return plannedWrite{}, problem
		}

		changes = append(changes, applied.region)
		body = applied.body
	}

	if body == file.Body {
		// Nothing to write. Reported as a planned write with no changes, so
		// the caller can say so rather than claiming an edit happened.
		return plannedWrite{relative: relative, absolute: absolute}, nil
	}

	return plannedWrite{
		relative: relative,
		absolute: absolute,
		content:  []byte(file.Render(body)),
		changes:  changes,
	}, nil
}

// verifySeen enforces that the agent has read the file, and read it as it
// currently is.
func (e *editEngine) verifySeen(relative, absolute string, raw []byte) *tool.Error {
	observed, seen := e.observer.Seen(absolute)
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

// commit writes what was planned.
//
// Every file is written or removed here, after all of them have been worked
// out. A failure at this point is a disk problem rather than a mistake in the
// change, and it is reported with what did land: pretending a half-written
// patch did not happen would be worse than saying so.
func (e *editEngine) commit(planned []plannedWrite) (applied []plannedWrite, err error) {
	for _, one := range planned {
		switch {
		case one.delete:
			if removeErr := os.Remove(one.absolute); removeErr != nil && !os.IsNotExist(removeErr) {
				return applied, fmt.Errorf("could not delete %s: %w", one.relative, removeErr)
			}
			e.observer.Forget(one.absolute)

		case len(one.content) > 0 || one.create:
			if writeErr := atomicWrite(one.absolute, one.content); writeErr != nil {
				return applied, fmt.Errorf("could not write %s: %w", one.relative, writeErr)
			}
			// What was read is no longer what is on disk.
			e.observer.Observe(one.absolute, hashBytes(one.content))

		default:
			// Nothing changed in this file.
			continue
		}

		applied = append(applied, one)
	}

	return applied, nil
}

// textEdit is one exact replacement.
type textEdit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}
