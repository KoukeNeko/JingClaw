package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// newFixture builds a small workspace that looks enough like a real project to
// exercise the parts that matter: nested directories, a skipped vendor tree, a
// binary file, and something to find.
func newFixture(t *testing.T) (*workspace.Workspace, *tool.Registry, string) {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		"README.md":                 "# Fixture\n\nA test workspace.\n",
		"src/main.go":               "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n",
		"src/util/strings.go":       "package util\n\nfunc Reverse(s string) string {\n\treturn s\n}\n",
		"src/util/strings_test.go":  "package util\n\nfunc TestReverse(t *testing.T) {}\n",
		"node_modules/pkg/index.js": "module.exports = function reverse() {}\n",
		"docs/design.md":            "Reverse is described here.\n",
	}
	for path, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// A file with a NUL byte, which must never be read into a prompt.
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	observed := builtin.NewObserver()

	registry := tool.NewRegistry()
	registry.MustRegister(
		&builtin.ReadFile{Workspace: ws, Observer: observed},
		&builtin.GlobFiles{Workspace: ws},
		&builtin.Grep{Workspace: ws},
		&builtin.WriteFile{Workspace: ws, Observer: observed},
	)

	return ws, registry, root
}

func call(t *testing.T, registry *tool.Registry, name string, args map[string]any) tool.Result {
	t.Helper()

	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}

	return registry.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: name, Arguments: encoded,
	})
}

func TestReadFileReturnsNumberedLines(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "read_file", map[string]any{"path": "src/main.go"})
	if result.IsError {
		t.Fatalf("read failed: %s", result.Content)
	}

	if !strings.Contains(result.Content, "package main") {
		t.Errorf("content missing the file body:\n%s", result.Content)
	}
	// Line numbers are what let a model turn a grep hit into a read range.
	if !strings.Contains(result.Content, "     1\t") {
		t.Errorf("content is not line-numbered:\n%s", result.Content)
	}
}

func TestReadFileHonoursLineRange(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "read_file", map[string]any{
		"path": "src/main.go", "start_line": 3, "end_line": 4,
	})
	if result.IsError {
		t.Fatalf("read failed: %s", result.Content)
	}

	if strings.Contains(result.Content, "package main") {
		t.Errorf("range included line 1:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "func main()") {
		t.Errorf("range missing line 3:\n%s", result.Content)
	}
}

// A model that believes it saw a whole file will reason confidently about code
// it was never shown, so truncation has to be stated.
func TestReadFileMarksTruncation(t *testing.T) {
	_, registry, root := newFixture(t)

	var builder strings.Builder
	for i := range 5000 {
		builder.WriteString("this is a reasonably long line of filler text ")
		builder.WriteString(strings.Repeat("x", 40))
		builder.WriteString("\n")
		_ = i
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(builder.String()), 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	result := call(t, registry, "read_file", map[string]any{"path": "big.txt"})
	if result.IsError {
		t.Fatalf("read failed: %s", result.Content)
	}
	if !result.Truncated {
		t.Error("a file well over the read limit was not reported as truncated")
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Errorf("the model is not told the content was cut:\n%s", result.Content[max(0, len(result.Content)-200):])
	}
}

func TestReadFileRejectsBinary(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "read_file", map[string]any{"path": "binary.dat"})
	if !result.IsError {
		t.Fatal("a binary file was read into the model's context")
	}
	assertErrorCode(t, result, tool.CodeUnsupported)
}

// The workspace boundary is the security property of this whole layer, so it
// is asserted at the tool surface too, not only in the workspace package.
func TestToolsRefuseToEscapeTheWorkspace(t *testing.T) {
	_, registry, _ := newFixture(t)

	for _, path := range []string{"../outside.txt", "/etc/passwd", "src/../../outside.txt"} {
		result := call(t, registry, "read_file", map[string]any{"path": path})
		if !result.IsError {
			t.Errorf("read_file(%q) succeeded; it must not reach outside the workspace", path)
			continue
		}
		assertErrorCode(t, result, tool.CodeOutsideWorkspace)
	}
}

func TestReadFileReportsMissingPath(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "read_file", map[string]any{"path": "src/absent.go"})
	if !result.IsError {
		t.Fatal("reading a missing file reported success")
	}
	assertErrorCode(t, result, tool.CodeNotFound)
}

func TestGlobFindsFilesAndSkipsNoise(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "glob_files", map[string]any{"pattern": "**/*.go"})
	if result.IsError {
		t.Fatalf("glob failed: %s", result.Content)
	}

	for _, want := range []string{"src/main.go", "src/util/strings.go"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("glob missed %s:\n%s", want, result.Content)
		}
	}

	// node_modules dominates the results of any search that walks it.
	result = call(t, registry, "glob_files", map[string]any{"pattern": "**/*.js"})
	if strings.Contains(result.Content, "node_modules") {
		t.Errorf("glob walked node_modules:\n%s", result.Content)
	}
}

func TestGrepFindsMatchesWithLocations(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "grep", map[string]any{"query": "Reverse"})
	if result.IsError {
		t.Fatalf("grep failed: %s", result.Content)
	}

	// path:line is what makes a hit directly usable as a read_file range.
	if !strings.Contains(result.Content, "src/util/strings.go:3") {
		t.Errorf("grep did not report path and line:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "node_modules") {
		t.Errorf("grep searched node_modules:\n%s", result.Content)
	}
}

func TestGrepCaseSensitivityAndInclude(t *testing.T) {
	_, registry, _ := newFixture(t)

	insensitive := call(t, registry, "grep", map[string]any{"query": "reverse"})
	if !strings.Contains(insensitive.Content, "strings.go") {
		t.Errorf("case-insensitive search missed a match:\n%s", insensitive.Content)
	}

	sensitive := call(t, registry, "grep", map[string]any{
		"query": "reverse", "case_sensitive": true,
	})
	if strings.Contains(sensitive.Content, "src/util/strings.go") {
		t.Errorf("case-sensitive search matched the wrong case:\n%s", sensitive.Content)
	}

	scoped := call(t, registry, "grep", map[string]any{
		"query": "Reverse", "include": "**/*.md",
	})
	if strings.Contains(scoped.Content, ".go:") {
		t.Errorf("include filter was ignored:\n%s", scoped.Content)
	}
	if !strings.Contains(scoped.Content, "docs/design.md") {
		t.Errorf("include filter excluded a real match:\n%s", scoped.Content)
	}
}

func TestGrepReportsBadRegexAsAnObservation(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := call(t, registry, "grep", map[string]any{"query": "([unclosed", "regex": true})
	if !result.IsError {
		t.Fatal("an invalid regular expression was accepted")
	}
	assertErrorCode(t, result, tool.CodeInvalidArguments)

	// Without a suggested action a model tends to retry the identical call.
	if !strings.Contains(result.Content, "suggested_action") {
		t.Errorf("no recovery hint given to the model:\n%s", result.Content)
	}
}

// Schema violations must come back as something the model can read and fix,
// never as a failure that aborts the run.
func TestSchemaViolationsBecomeModelVisibleErrors(t *testing.T) {
	_, registry, _ := newFixture(t)

	cases := map[string]map[string]any{
		"missing required field": {},
		"wrong type":             {"path": 42},
		"unknown field":          {"path": "README.md", "encoding": "utf-8"},
		"out of range":           {"path": "README.md", "start_line": 0},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			result := call(t, registry, "read_file", args)
			if !result.IsError {
				t.Fatalf("invalid arguments were accepted: %s", result.Content)
			}
			assertErrorCode(t, result, tool.CodeInvalidArguments)
		})
	}
}

func TestUnknownToolIsAnObservation(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := registry.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "delete_everything", Arguments: json.RawMessage(`{}`),
	})
	if !result.IsError {
		t.Fatal("an unknown tool reported success")
	}
	assertErrorCode(t, result, tool.CodeNotFound)
}

func TestMalformedArgumentJSONIsAnObservation(t *testing.T) {
	_, registry, _ := newFixture(t)

	result := registry.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path": `),
	})
	if !result.IsError {
		t.Fatal("malformed JSON was accepted")
	}
	assertErrorCode(t, result, tool.CodeInvalidArguments)
}

// Specs feed the prompt prefix, which has to stay byte-identical between runs
// for prompt caching to work at all.
func TestSpecsAreStablyOrdered(t *testing.T) {
	_, registry, _ := newFixture(t)

	first := registry.Specs()
	for range 5 {
		next := registry.Specs()
		for i := range first {
			if first[i].Name != next[i].Name {
				t.Fatalf("spec order changed between calls: %s then %s", first[i].Name, next[i].Name)
			}
		}
	}

	for i := 1; i < len(first); i++ {
		if first[i-1].Name > first[i].Name {
			t.Errorf("specs are not sorted: %s before %s", first[i-1].Name, first[i].Name)
		}
	}
}

func TestSymlinkEscapeIsRefusedAtTheToolSurface(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}

	_, registry, root := newFixture(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	result := call(t, registry, "read_file", map[string]any{"path": "link.txt"})
	if !result.IsError {
		t.Fatalf("read a file outside the workspace through a symlink: %s", result.Content)
	}
	if strings.Contains(result.Content, "secret") {
		t.Error("the contents of the escaped file leaked into the result")
	}
}

func assertErrorCode(t *testing.T, result tool.Result, want tool.ErrorCode) {
	t.Helper()

	var payload struct {
		Code tool.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("error result is not JSON the model can parse: %s", result.Content)
	}
	if payload.Code != want {
		t.Errorf("got code %q, want %q (%s)", payload.Code, want, result.Content)
	}
}
