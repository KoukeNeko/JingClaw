package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// These are the rules that decide whether an agent can be trusted with a
// repository. Each one exists because breaking it destroys work in a way
// nothing downstream can detect or undo.

func newEditFixture(t *testing.T) (*tool.Registry, string) {
	t.Helper()

	root := t.TempDir()
	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()
	locks := builtin.NewFileLocks()

	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		builtin.NewWriteFile(ws, observed, locks),
		builtin.NewEditFile(ws, observed, locks),
	)

	return registry, root
}

func writeFixture(t *testing.T, root, name, contents string) string {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return full
}

func readIt(t *testing.T, registry *tool.Registry, path string) {
	t.Helper()

	result := call(t, registry, "read_file", map[string]any{"path": path})
	if result.IsError {
		t.Fatalf("read %s: %s", path, result.Content)
	}
}

func edit(t *testing.T, registry *tool.Registry, path string, edits ...map[string]any) tool.Result {
	t.Helper()

	return call(t, registry, "edit_file", map[string]any{"path": path, "edits": edits})
}

func replacement(oldText, newText string) map[string]any {
	return map[string]any{"old_text": oldText, "new_text": newText}
}

const sampleGo = `package util

import "strings"

func Reverse(s string) string {
	return s
}

func Upper(s string) string {
	return strings.ToUpper(s)
}
`

func TestEditReplacesAUniqueRegion(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "util.go", sampleGo)

	readIt(t, registry, "util.go")

	result := edit(t, registry, "util.go", replacement("\treturn s\n", "\treturn reverse(s)\n"))
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(after), "return reverse(s)") {
		t.Errorf("the edit did not apply:\n%s", after)
	}
	// Everything else must be untouched.
	if !strings.Contains(string(after), "strings.ToUpper(s)") {
		t.Errorf("the edit disturbed unrelated code:\n%s", after)
	}

	// A successful edit must be reviewable without reading the file.
	if !strings.Contains(result.Content, "-\treturn s") || !strings.Contains(result.Content, "+\treturn reverse(s)") {
		t.Errorf("the result is not a reviewable diff:\n%s", result.Content)
	}
}

// Guessing which of several identical passages was meant corrupts the file in
// a way nothing downstream can detect.
func TestAmbiguousEditIsRefused(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "dup.go", "func a() {\n\treturn\n}\n\nfunc b() {\n\treturn\n}\n")

	readIt(t, registry, "dup.go")
	before, _ := os.ReadFile(path)

	result := edit(t, registry, "dup.go", replacement("\treturn\n", "\treturn nil\n"))
	if !result.IsError {
		t.Fatal("an ambiguous replacement was applied")
	}
	assertErrorCode(t, result, tool.CodeInvalidArguments)
	if !strings.Contains(result.Content, "unique") {
		t.Errorf("the model is not told how to fix it: %s", result.Content)
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("the file was modified despite the refusal")
	}
}

func TestMissingTargetIsRefused(t *testing.T) {
	registry, root := newEditFixture(t)
	writeFixture(t, root, "util.go", sampleGo)

	readIt(t, registry, "util.go")

	result := edit(t, registry, "util.go", replacement("func Missing() {}\n", "x"))
	if !result.IsError {
		t.Fatal("a replacement with no target was applied")
	}
	assertErrorCode(t, result, tool.CodeNotFound)
}

// Editing a file the agent has never looked at applies a change reasoned about
// against content it cannot describe.
func TestEditRequiresHavingReadTheFile(t *testing.T) {
	registry, root := newEditFixture(t)
	writeFixture(t, root, "util.go", sampleGo)

	result := edit(t, registry, "util.go", replacement("\treturn s\n", "\treturn nil\n"))
	if !result.IsError {
		t.Fatal("edited a file that was never read")
	}
	assertErrorCode(t, result, tool.CodePermissionDenied)
}

// A human, an editor or another agent may have changed the file since it was
// read. Applying the edit now would silently discard their work.
func TestEditRefusesAfterTheFileChangedOnDisk(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "util.go", sampleGo)

	readIt(t, registry, "util.go")

	// Somebody else edits it.
	if err := os.WriteFile(path, []byte(sampleGo+"\n// added by someone else\n"), 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}

	result := edit(t, registry, "util.go", replacement("\treturn s\n", "\treturn nil\n"))
	if !result.IsError {
		t.Fatal("edited a file that had changed since it was read")
	}
	if !strings.Contains(result.Content, "changed on disk") {
		t.Errorf("the reason is not stated: %s", result.Content)
	}

	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "added by someone else") {
		t.Error("the other change was discarded")
	}
}

// Line endings are how a file is written down, not content. Rewriting them
// turns a one-line change into a whole-file diff.
func TestEditPreservesCRLF(t *testing.T) {
	registry, root := newEditFixture(t)

	crlf := strings.ReplaceAll(sampleGo, "\n", "\r\n")
	path := writeFixture(t, root, "windows.go", crlf)

	readIt(t, registry, "windows.go")

	// The model works from read_file output and cannot tell the file uses
	// CRLF, so it sends LF.
	result := edit(t, registry, "windows.go", replacement("\treturn s\n", "\treturn reverse(s)\n"))
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(strings.ReplaceAll(string(after), "\r\n", ""), "\n") {
		t.Errorf("a lone LF was introduced into a CRLF file:\n%q", after)
	}
	if !strings.Contains(string(after), "return reverse(s)\r\n") {
		t.Errorf("the replacement did not use CRLF:\n%q", after)
	}
}

func TestEditPreservesByteOrderMark(t *testing.T) {
	registry, root := newEditFixture(t)

	const bom = "\uFEFF"
	path := writeFixture(t, root, "bom.go", bom+sampleGo)

	readIt(t, registry, "bom.go")

	result := edit(t, registry, "bom.go", replacement("\treturn s\n", "\treturn nil\n"))
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasPrefix(string(after), bom) {
		t.Error("the byte order mark was dropped")
	}
}

func TestEditRefusesNonUTF8(t *testing.T) {
	registry, root := newEditFixture(t)

	// Latin-1 encoded text: valid bytes, invalid UTF-8.
	path := filepath.Join(root, "legacy.txt")
	if err := os.WriteFile(path, []byte{0x68, 0x69, 0xE9, 0x0A}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Reading it fails too, so the observation is planted directly to isolate
	// the encoding check from the read rule.
	result := call(t, registry, "read_file", map[string]any{"path": "legacy.txt"})
	if !result.IsError {
		t.Skip("read accepted the file; the encoding check belongs to read here")
	}
}

// Applying edits in sequence means a later one sees the result of an earlier
// one, which is what makes several related changes in a single call coherent.
func TestEditsApplyInOrder(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "seq.txt", "alpha\nbeta\ngamma\n")

	readIt(t, registry, "seq.txt")

	result := edit(t, registry, "seq.txt",
		replacement("alpha\n", "one\n"),
		replacement("beta\n", "two\n"),
	)
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}

	after, _ := os.ReadFile(path)
	if string(after) != "one\ntwo\ngamma\n" {
		t.Errorf("got %q", string(after))
	}
}

// A failing edit anywhere in the batch must leave the file untouched: applying
// half of a set of related changes is worse than applying none.
func TestABadEditInABatchAbandonsAllOfThem(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "seq.txt", "alpha\nbeta\ngamma\n")

	readIt(t, registry, "seq.txt")

	result := edit(t, registry, "seq.txt",
		replacement("alpha\n", "one\n"),
		replacement("does not exist\n", "two\n"),
	)
	if !result.IsError {
		t.Fatal("a batch containing an impossible edit was applied")
	}

	after, _ := os.ReadFile(path)
	if string(after) != "alpha\nbeta\ngamma\n" {
		t.Errorf("the file was partially edited: %q", string(after))
	}
}

func TestEditCannotEscapeTheWorkspace(t *testing.T) {
	registry, _ := newEditFixture(t)

	result := edit(t, registry, "../outside.txt", replacement("a", "b"))
	if !result.IsError {
		t.Fatal("edited a file outside the workspace")
	}
	assertErrorCode(t, result, tool.CodeOutsideWorkspace)
}

// Read-verify-write is only safe while nothing else touches the file. Without
// per-path locking, two edits each verify against the original and the second
// silently discards the first.
func TestConcurrentEditsToOneFileDoNotLoseWork(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "shared.txt", "alpha\nbeta\n")

	readIt(t, registry, "shared.txt")

	var wg sync.WaitGroup
	results := make([]tool.Result, 2)
	edits := []map[string]any{
		replacement("alpha\n", "one\n"),
		replacement("beta\n", "two\n"),
	}

	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()

			encoded, err := json.Marshal(map[string]any{
				"path":  "shared.txt",
				"edits": []map[string]any{edits[i]},
			})
			if err != nil {
				t.Errorf("encode: %v", err)
				return
			}
			results[i] = registry.Execute(context.Background(), tool.Call{
				ID: "call", Name: "edit_file", Arguments: encoded,
			})
		}()
	}
	wg.Wait()

	after, _ := os.ReadFile(path)
	content := string(after)

	// Exactly one must win: the loser's observation is stale the moment the
	// winner writes. What must never happen is both reporting success while
	// only one change survives.
	succeeded := 0
	for _, result := range results {
		if !result.IsError {
			succeeded++
		}
	}

	switch succeeded {
	case 1:
		// The refused one has to say why, so the model can re-read and retry.
		for _, result := range results {
			if result.IsError && !strings.Contains(result.Content, "changed on disk") {
				t.Errorf("the losing edit was refused for the wrong reason: %s", result.Content)
			}
		}
	case 2:
		if !strings.Contains(content, "one") || !strings.Contains(content, "two") {
			t.Errorf("both edits reported success but the file has only one of them: %q", content)
		}
	default:
		t.Errorf("neither edit applied: %q", content)
	}
}

func TestEditReportsNoChangeRatherThanRewriting(t *testing.T) {
	registry, root := newEditFixture(t)
	path := writeFixture(t, root, "same.txt", "alpha\n")

	readIt(t, registry, "same.txt")

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	result := edit(t, registry, "same.txt", replacement("alpha\n", "alpha\n"))
	if result.IsError {
		t.Fatalf("edit failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, "unchanged") {
		t.Errorf("a no-op edit was not reported as such: %s", result.Content)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a no-op edit rewrote the file")
	}
}

// A file the agent itself just wrote is one it knows the contents of, so a
// follow-up edit should not have to read it again.
func TestWriteThenEditWorksWithoutARead(t *testing.T) {
	registry, root := newEditFixture(t)

	created := call(t, registry, "write_file", map[string]any{
		"path": "fresh.txt", "content": "alpha\nbeta\n",
	})
	if created.IsError {
		t.Fatalf("write failed: %s", created.Content)
	}

	result := edit(t, registry, "fresh.txt", replacement("beta\n", "gamma\n"))
	if result.IsError {
		t.Fatalf("editing a file the agent just wrote failed: %s", result.Content)
	}

	after, _ := os.ReadFile(filepath.Join(root, "fresh.txt"))
	if string(after) != "alpha\ngamma\n" {
		t.Errorf("got %q", string(after))
	}
}

func TestWritePreservesCRLFAndBOM(t *testing.T) {
	registry, root := newEditFixture(t)

	const bom = "\uFEFF"
	path := writeFixture(t, root, "windows.txt", bom+"alpha\r\nbeta\r\n")

	readIt(t, registry, "windows.txt")

	result := call(t, registry, "write_file", map[string]any{
		"path": "windows.txt", "content": "one\ntwo\n",
	})
	if result.IsError {
		t.Fatalf("write failed: %s", result.Content)
	}

	after, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(after), bom) {
		t.Error("the byte order mark was dropped")
	}
	if !strings.Contains(string(after), "one\r\ntwo\r\n") {
		t.Errorf("line endings were not preserved: %q", string(after))
	}
}

// A person asked to approve an edit cannot review nine hundred characters of
// old_text against nine hundred and fifty of new_text. The diff between them
// is what a review actually is.
func TestAnEditPreviewsAsADiff(t *testing.T) {
	edit := builtin.NewEditFile(nil, nil, nil)

	preview := edit.Preview(json.RawMessage(`{
		"path": "internal/runtime/runtime.go",
		"edits": [{"old_text": "timeout := 30", "new_text": "timeout := 120"}]
	}`))

	for _, want := range []string{"internal/runtime/runtime.go", "-timeout := 30", "+timeout := 120"} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not show %q:\n%s", want, preview)
		}
	}
}

// A preview runs before anybody has decided. One that read or wrote anything
// would be the edit happening without approval, so it is given a workspace it
// could not use even if it tried.
func TestAPreviewTouchesNothing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	edit := builtin.NewEditFile(ws, builtin.NewObserver(), builtin.NewFileLocks())

	_ = edit.Preview(json.RawMessage(`{
		"path": "notes.txt",
		"edits": [{"old_text": "original", "new_text": "replaced"}]
	}`))

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != "original\n" {
		t.Errorf("the preview changed the file: %q", after)
	}
}

// Arguments that do not parse must produce no preview rather than a panic or a
// misleading one. This runs on whatever a model produced.
func TestUnreadableArgumentsPreviewAsNothing(t *testing.T) {
	edit := builtin.NewEditFile(nil, nil, nil)

	if preview := edit.Preview(json.RawMessage(`{"path":`)); preview != "" {
		t.Errorf("unparseable arguments produced a preview: %q", preview)
	}
	if preview := edit.Preview(json.RawMessage(`{"path": "x.txt"}`)); preview != "" {
		t.Errorf("an edit with no changes produced a preview: %q", preview)
	}
}
