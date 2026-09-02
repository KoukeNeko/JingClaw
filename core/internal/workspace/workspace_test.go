package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/workspace"
)

// The workspace boundary is the only thing standing between a model-supplied
// path and the rest of the filesystem, so these cases are the security tests
// for the whole tool layer.
func newWorkspace(t *testing.T) (*workspace.Workspace, string) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "nested"), 0o755); err != nil {
		t.Fatalf("create tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	return ws, root
}

func TestResolveAcceptsPathsInsideTheRoot(t *testing.T) {
	ws, _ := newWorkspace(t)

	for _, path := range []string{
		"src/main.go",
		"./src/main.go",
		"src/nested",
		"src/../src/main.go",
		".",
		// A file that does not exist yet is still a legitimate target.
		"src/new-file.go",
	} {
		if _, err := ws.Resolve(path); err != nil {
			t.Errorf("Resolve(%q) rejected a path inside the workspace: %v", path, err)
		}
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	ws, _ := newWorkspace(t)

	for _, path := range []string{
		"../outside.txt",
		"../../etc/passwd",
		"src/../../outside.txt",
		"src/nested/../../../outside.txt",
	} {
		_, err := ws.Resolve(path)
		if !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideWorkspace", path, err)
		}
	}
}

// Accepting an absolute path would make the workspace root stop describing
// what is reachable, even when the path happens to be harmless.
func TestResolveRejectsAbsolutePaths(t *testing.T) {
	ws, root := newWorkspace(t)

	// An absolute path outside the root, spelled the way the running platform
	// spells one: "/etc/passwd" is not absolute on Windows (it has no drive),
	// and hard-coding a drive would assume one the machine may not have. The
	// root's own volume is absolute on either platform — empty on Unix, so this
	// is "/outside.txt", and the actual drive of the temp dir on Windows.
	absoluteOutside := filepath.VolumeName(root) + string(filepath.Separator) + "outside.txt"

	for _, path := range []string{
		absoluteOutside,
		filepath.Join(root, "src", "main.go"),
	} {
		_, err := ws.Resolve(path)
		if !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideWorkspace", path, err)
		}
	}
}

// On Windows a component can reach outside without any traversal: a reserved
// device name opens the device from any directory, and a colon names a
// drive-relative path or an alternate data stream rather than a file the join
// placed inside the root.
func TestResolveRejectsWindowsDeviceAndStreamNames(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("device names and colons are only special on Windows")
	}

	ws, _ := newWorkspace(t)

	for _, path := range []string{
		"CON", "NUL", "COM1", "nul.txt", "sub/CON",
		`C:relative`, "notes.txt:hidden",
	} {
		if _, err := ws.Resolve(path); !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideWorkspace", path, err)
		}
	}
}

// A symlink is the case lexical cleaning cannot catch: the path looks entirely
// innocent and still lands outside.
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}

	ws, root := newWorkspace(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if _, err := ws.Resolve("escape"); !errors.Is(err, workspace.ErrOutsideWorkspace) {
		t.Errorf("Resolve through a symlink = %v, want ErrOutsideWorkspace", err)
	}
}

func TestResolveRejectsSymlinkedDirectoryEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}

	ws, root := newWorkspace(t)

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	if err := os.Symlink(outsideDir, filepath.Join(root, "linkdir")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// Reaching through a symlinked directory must fail even for a file that
	// does not exist yet, or a write tool could create files anywhere.
	for _, path := range []string{"linkdir/secret.txt", "linkdir/new.txt"} {
		if _, err := ws.Resolve(path); !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideWorkspace", path, err)
		}
	}
}

// A symlink that stays inside the workspace is ordinary and must keep working.
func TestResolveAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}

	ws, root := newWorkspace(t)

	if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "alias")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if _, err := ws.Resolve("alias/main.go"); err != nil {
		t.Errorf("Resolve through an internal symlink failed: %v", err)
	}
}

// A sibling directory whose name merely starts with the root's name must not
// be treated as inside it.
func TestSiblingPrefixIsNotInsideTheWorkspace(t *testing.T) {
	parent := t.TempDir()

	root := filepath.Join(parent, "work")
	sibling := filepath.Join(parent, "work-elsewhere")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	ws, err := workspace.Open(root)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	if _, err := ws.RelativeTo(filepath.Join(sibling, "file.txt")); !errors.Is(err, workspace.ErrOutsideWorkspace) {
		t.Errorf("a sibling with a shared prefix was treated as inside: %v", err)
	}
}

func TestRelativeToProducesSlashPaths(t *testing.T) {
	ws, root := newWorkspace(t)

	relative, err := ws.RelativeTo(filepath.Join(root, "src", "main.go"))
	if err != nil {
		t.Fatalf("RelativeTo: %v", err)
	}
	// Models and users read forward slashes regardless of platform.
	if relative != "src/main.go" {
		t.Errorf("got %q, want %q", relative, "src/main.go")
	}
}

func TestOpenRejectsMissingOrNonDirectoryRoots(t *testing.T) {
	root := t.TempDir()

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := workspace.Open(filepath.Join(root, "absent")); err == nil {
		t.Error("opened a workspace at a path that does not exist")
	}
	if _, err := workspace.Open(file); err == nil {
		t.Error("opened a workspace at a regular file")
	}
	if _, err := workspace.Open(""); err == nil {
		t.Error("opened a workspace with no root")
	}
}
