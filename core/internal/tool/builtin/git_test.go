package builtin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/tool"
	"github.com/KoukeNeko/JingClaw/core/internal/tool/builtin"
	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// newRepository makes a real repository to read.
//
// A real one rather than a fake: what is being checked is that this parses
// what git actually prints, and a fixture written from the documentation is
// exactly the thing that has been wrong here before.
func newRepository(t *testing.T) (*builtin.GitStatus, *builtin.GitDiff, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=check", "GIT_AUTHOR_EMAIL=check@example.com",
			"GIT_COMMITTER_NAME=check", "GIT_COMMITTER_EMAIL=check@example.com")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "kept.txt")
	run("commit", "-m", "first")

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	return &builtin.GitStatus{Workspace: ws}, &builtin.GitDiff{Workspace: ws}, root
}

func status(t *testing.T, reader *builtin.GitStatus) tool.Result {
	t.Helper()
	result, err := reader.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "git_status", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("git_status: %v", err)
	}
	return result
}

func diff(t *testing.T, reader *builtin.GitDiff, args string) (tool.Result, error) {
	t.Helper()
	return reader.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "git_diff", Arguments: json.RawMessage(args),
	})
}

// A clean repository has to say so rather than printing nothing, which reads
// as a tool that failed.
func TestACleanRepositorySaysSo(t *testing.T) {
	reader, _, _ := newRepository(t)

	result := status(t, reader)
	if !strings.Contains(result.Content, "on main") {
		t.Errorf("the branch is not named: %q", result.Content)
	}
	if !strings.Contains(result.Content, "nothing has changed") {
		t.Errorf("a clean repository does not say so: %q", result.Content)
	}
}

// Staged, unstaged and untracked are three different things, and a file can
// be in two of them at once.
func TestStatusTellsTheThreeKindsOfChangeApart(t *testing.T) {
	reader, _, root := newRepository(t)

	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "added.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("loose\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	staged := exec.Command("git", "add", "added.txt")
	staged.Dir = root
	if out, err := staged.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	result := status(t, reader)
	for _, want := range []string{"staged", "added.txt", "changed, not staged", "kept.txt",
		"untracked", "untracked.txt"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("the status does not mention %q:\n%s", want, result.Content)
		}
	}
	if !strings.Contains(result.Summary, "main") {
		t.Errorf("the summary does not name the branch: %q", result.Summary)
	}
}

// The diff is the change, not a description of it.
func TestDiffShowsWhatChanged(t *testing.T) {
	_, differ, root := newRepository(t)

	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := diff(t, differ, `{}`)
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	if !strings.Contains(result.Content, "-one") || !strings.Contains(result.Content, "+two") {
		t.Errorf("the diff does not show the change:\n%s", result.Content)
	}
}

// Nothing to show has to say so. An empty result reads as a tool that failed.
func TestAnEmptyDiffSaysSo(t *testing.T) {
	_, differ, _ := newRepository(t)

	result, err := diff(t, differ, `{}`)
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	if !strings.Contains(result.Content, "Nothing") {
		t.Errorf("an empty diff does not say so: %q", result.Content)
	}
}

// Staged and unstaged are different questions, and a tool that answered one
// when asked the other would have somebody committing what they did not mean
// to.
func TestStagedAndUnstagedAreDifferentDiffs(t *testing.T) {
	_, differ, root := newRepository(t)

	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	add := exec.Command("git", "add", "kept.txt")
	add.Dir = root
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("staged\nand more\n"), 0o644); err != nil {
		t.Fatalf("write again: %v", err)
	}

	unstaged, err := diff(t, differ, `{}`)
	if err != nil {
		t.Fatalf("git_diff: %v", err)
	}
	if !strings.Contains(unstaged.Content, "+and more") {
		t.Errorf("the unstaged diff is wrong:\n%s", unstaged.Content)
	}

	inIndex, err := diff(t, differ, `{"staged": true}`)
	if err != nil {
		t.Fatalf("git_diff staged: %v", err)
	}
	if !strings.Contains(inIndex.Content, "+staged") {
		t.Errorf("the staged diff is wrong:\n%s", inIndex.Content)
	}
	if strings.Contains(inIndex.Content, "and more") {
		t.Error("the staged diff includes what is not staged")
	}
}

// A path cannot reach outside the workspace, the same as everywhere else.
func TestDiffCannotEscapeTheWorkspace(t *testing.T) {
	_, differ, _ := newRepository(t)

	if _, err := diff(t, differ, `{"path": "../../etc"}`); err == nil {
		t.Error("a diff was taken outside the workspace")
	}
}

// A path that starts with a dash must be a path, not a flag. This is the
// whole reason the arguments are fixed here rather than passed through.
func TestAPathIsNeverReadAsAFlag(t *testing.T) {
	_, differ, root := newRepository(t)

	// A real file with an alarming name.
	if err := os.WriteFile(filepath.Join(root, "--output=escaped"), []byte("x\n"), 0o644); err != nil {
		t.Skipf("this filesystem will not hold the name: %v", err)
	}

	// It should be treated as a path: either a diff of that file or nothing
	// to show, never git acting on a flag.
	result, err := diff(t, differ, `{"path": "--output=escaped"}`)
	if err != nil {
		// Refused is also correct; what must not happen is git reading it as
		// an instruction.
		return
	}
	if strings.Contains(result.Content, "usage:") {
		t.Errorf("git read the path as a flag: %q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); err == nil {
		t.Error("git wrote a file the path named as a flag")
	}
}

// A workspace that is not a repository must say that, rather than an opaque
// exit code the model cannot act on.
func TestSomewhereThatIsNotARepositorySaysSo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	reader := &builtin.GitStatus{Workspace: ws}

	_, err = reader.Execute(context.Background(), tool.Call{
		ID: "call_1", Name: "git_status", Arguments: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("a directory that is not a repository reported a status")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("the reason is unclear: %v", err)
	}
}

// Reading a repository must not run programs the repository chose. A diff can
// otherwise invoke an external driver configured inside the repository being
// read, which would make a tool that needs no approval a way to run anything.
func TestReadingARepositoryRunsNothingItConfigured(t *testing.T) {
	_, differ, root := newRepository(t)

	marker := filepath.Join(root, "external-diff-ran")
	script := filepath.Join(root, "pretend-diff.sh")
	if err := os.WriteFile(script,
		[]byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	// Configured the way a hostile repository would: in its own config, and
	// applied to every file.
	for _, args := range [][]string{
		{"config", "diff.external", script},
		{"config", "diff.pretend.textconv", script},
	} {
		command := exec.Command("git", args...)
		command.Dir = root
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"),
		[]byte("* diff=pretend\n"), 0o644); err != nil {
		t.Fatalf("write attributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := diff(t, differ, `{}`); err != nil {
		t.Fatalf("git_diff: %v", err)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("reading the repository ran a program the repository named")
	}
}
