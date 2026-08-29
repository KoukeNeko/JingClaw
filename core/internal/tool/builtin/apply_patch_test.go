package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

func newPatchTools(t *testing.T) (*builtin.ApplyPatch, *builtin.ReadFile, string) {
	t.Helper()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()

	return builtin.NewApplyPatch(ws, observed, locks),
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		root
}

func writeSource(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func read(t *testing.T, reader *builtin.ReadFile, path string) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": path})
	if _, err := reader.Execute(context.Background(), tool.Call{
		ID: "call_read", Name: "read_file", Arguments: args,
	}); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
}

func patch(t *testing.T, tool_ *builtin.ApplyPatch, operations ...map[string]any) (tool.Result, error) {
	t.Helper()
	args, err := json.Marshal(map[string]any{"operations": operations})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return tool_.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "apply_patch", Arguments: args,
	})
}

func contents(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read back %s: %v", name, err)
	}
	return string(raw)
}

// The reason this is one call rather than several: a change that spans files
// lands together or not at all.
func TestAPatchChangesEveryFileTogether(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "caller.go", "x := oldName()\n")
	writeSource(t, root, "defn.go", "func oldName() int { return 1 }\n")
	read(t, reader, "caller.go")
	read(t, reader, "defn.go")

	if _, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "caller.go",
			"edits": []map[string]string{{"old_text": "oldName()", "new_text": "newName()"}}},
		map[string]any{"op": "update", "path": "defn.go",
			"edits": []map[string]string{{"old_text": "func oldName()", "new_text": "func newName()"}}},
		map[string]any{"op": "create", "path": "notes/why.md", "content": "renamed for clarity\n"},
	); err != nil {
		t.Fatalf("patch: %v", err)
	}

	if got := contents(t, root, "caller.go"); !strings.Contains(got, "newName()") {
		t.Errorf("caller.go was not updated: %q", got)
	}
	if got := contents(t, root, "defn.go"); !strings.Contains(got, "func newName()") {
		t.Errorf("defn.go was not updated: %q", got)
	}
	if got := contents(t, root, "notes/why.md"); got != "renamed for clarity\n" {
		t.Errorf("the new file is %q", got)
	}
}

// A patch that cannot apply cleanly must change nothing. Half of a rename is
// worse than none of it: the code no longer builds and the model's next read
// gets a state nobody intended.
func TestAPatchThatCannotApplyChangesNothing(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "caller.go", "x := oldName()\n")
	writeSource(t, root, "defn.go", "func oldName() int { return 1 }\n")
	read(t, reader, "caller.go")
	read(t, reader, "defn.go")

	_, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "caller.go",
			"edits": []map[string]string{{"old_text": "oldName()", "new_text": "newName()"}}},
		// This one cannot match.
		map[string]any{"op": "update", "path": "defn.go",
			"edits": []map[string]string{{"old_text": "func somethingElse()", "new_text": "func newName()"}}},
	)
	if err == nil {
		t.Fatal("a patch with an impossible edit reported success")
	}

	if got := contents(t, root, "caller.go"); got != "x := oldName()\n" {
		t.Errorf("the first file was changed by a patch that failed: %q", got)
	}
}

// The rule that makes editing safe at all: a replacement whose target is not
// unique fails rather than guessing.
func TestAnAmbiguousReplacementFailsInAPatchToo(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "twice.go", "return nil\nreturn nil\n")
	read(t, reader, "twice.go")

	if _, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "twice.go",
			"edits": []map[string]string{{"old_text": "return nil", "new_text": "return err"}}},
	); err == nil {
		t.Fatal("an ambiguous replacement was applied")
	}
}

// The same staleness rule edit_file has. A patch reasoned about against
// content that no longer exists applies a change to something else.
func TestAPatchRefusesAFileItHasNotRead(t *testing.T) {
	patcher, _, root := newPatchTools(t)
	writeSource(t, root, "unseen.go", "x := 1\n")

	if _, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "unseen.go",
			"edits": []map[string]string{{"old_text": "x := 1", "new_text": "x := 2"}}},
	); err == nil {
		t.Fatal("a file that was never read was patched")
	}
}

func TestAPatchRefusesAFileThatChangedSinceItWasRead(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "moving.go", "x := 1\n")
	read(t, reader, "moving.go")

	// Somebody else edits it.
	writeSource(t, root, "moving.go", "x := 99\n")

	if _, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "moving.go",
			"edits": []map[string]string{{"old_text": "x := 1", "new_text": "x := 2"}}},
	); err == nil {
		t.Fatal("a file that changed on disk was patched")
	}
}

// "Create" says the model believes the file is not there. Silently replacing
// somebody's work would be the worst kind of surprise: invisible, and
// attributed to a patch that said it was adding something.
func TestCreateRefusesAFileThatExists(t *testing.T) {
	patcher, _, root := newPatchTools(t)
	writeSource(t, root, "already.go", "important\n")

	if _, err := patch(t, patcher,
		map[string]any{"op": "create", "path": "already.go", "content": "replacement\n"},
	); err == nil {
		t.Fatal("create overwrote a file that exists")
	}
	if got := contents(t, root, "already.go"); got != "important\n" {
		t.Errorf("the file was changed: %q", got)
	}
}

// Deleting a file the agent has not read throws away work nothing here can
// recover.
func TestDeleteRefusesAFileItHasNotRead(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "doomed.go", "x := 1\n")

	if _, err := patch(t, patcher,
		map[string]any{"op": "delete", "path": "doomed.go"},
	); err == nil {
		t.Fatal("a file that was never read was deleted")
	}
	if _, err := os.Stat(filepath.Join(root, "doomed.go")); err != nil {
		t.Errorf("the file is gone: %v", err)
	}

	read(t, reader, "doomed.go")
	if _, err := patch(t, patcher,
		map[string]any{"op": "delete", "path": "doomed.go"},
	); err != nil {
		t.Fatalf("deleting a file that was read: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "doomed.go")); !os.IsNotExist(err) {
		t.Error("the file was not deleted")
	}
}

// Two operations on one file cannot both be planned against the same content.
// "Which one wins" is not a question the model meant to ask.
func TestOneFileCannotAppearTwice(t *testing.T) {
	patcher, reader, root := newPatchTools(t)
	writeSource(t, root, "once.go", "a\nb\n")
	read(t, reader, "once.go")

	if _, err := patch(t, patcher,
		map[string]any{"op": "update", "path": "once.go",
			"edits": []map[string]string{{"old_text": "a", "new_text": "A"}}},
		map[string]any{"op": "update", "path": "once.go",
			"edits": []map[string]string{{"old_text": "b", "new_text": "B"}}},
	); err == nil {
		t.Fatal("a file appearing twice in one patch was accepted")
	}
}

// A patch cannot reach outside the workspace, the same as everything else.
func TestAPatchCannotEscapeTheWorkspace(t *testing.T) {
	patcher, _, _ := newPatchTools(t)

	if _, err := patch(t, patcher,
		map[string]any{"op": "create", "path": "../escaped.txt", "content": "no"},
	); err == nil {
		t.Fatal("a patch wrote outside the workspace")
	}
}

// Somebody approving a patch has to be able to see it. This is the tool whose
// arguments are least readable as they stand.
func TestAPatchPreviewsAsADiffAcrossFiles(t *testing.T) {
	patcher, _, _ := newPatchTools(t)

	preview := patcher.Preview(json.RawMessage(`{
		"operations": [
			{"op": "update", "path": "caller.go",
			 "edits": [{"old_text": "oldName()", "new_text": "newName()"}]},
			{"op": "create", "path": "notes.md", "content": "why"},
			{"op": "delete", "path": "dead.go"}
		]
	}`))

	for _, want := range []string{
		"caller.go", "-oldName()", "+newName()",
		"notes.md", "(new)", "+why",
		"dead.go", "(deleted)",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not show %q:\n%s", want, preview)
		}
	}
}
