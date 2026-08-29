package home_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// A daemon started from a subdirectory must be the same daemon as one started
// from the top. Otherwise there are two, with separate databases, differing by
// which directory somebody happened to be in.
func TestItIsFoundFromAnywhereBelowIt(t *testing.T) {
	root := t.TempDir()
	dir, err := home.Create(root)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	deep := filepath.Join(root, "core", "internal", "gateway")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, from := range []string{root, filepath.Join(root, "core"), deep} {
		found, ok := home.Find(from)
		if !ok {
			t.Errorf("not found from %s", from)
			continue
		}
		if found.Root != dir.Root {
			t.Errorf("from %s found %s, want %s", from, found.Root, dir.Root)
		}
	}
}

// Nothing above it either: a directory with none must fall through to the
// platform locations rather than adopting somebody else's.
func TestNothingIsFoundWhenThereIsNone(t *testing.T) {
	// t.TempDir is under the system temp directory, which has no .JingClaw
	// above it unless somebody put one there.
	if found, ok := home.Find(t.TempDir()); ok {
		t.Errorf("found %s where there should be none", found.Root)
	}
}

// Everything is inside it, and the workspace is inside it too: a deployment
// set up to try the thing out must not be able to reach the project it was set
// up in.
func TestEverythingIsInsideIt(t *testing.T) {
	root := t.TempDir()
	dir, err := home.Create(root)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	paths := map[string]string{
		"config":    dir.ConfigFile(),
		"workspace": dir.Workspace(),
		"data":      dir.Data(),
		"run":       dir.Run(),
		"secret":    dir.SecretFile("gemini.key"),
	}
	for name, path := range paths {
		relative, err := filepath.Rel(dir.Root, path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if relative == ".." || filepath.IsAbs(relative) || relative[:2] == ".." {
			t.Errorf("%s is at %s, outside the directory", name, path)
		}
	}

	// The directories exist, so nothing has to create them later at a moment
	// when failing is inconvenient.
	for _, path := range []string{dir.Workspace(), dir.Data(), dir.Run()} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not created", path)
		}
	}
}

// Owner-only, because it holds every conversation and the credentials.
func TestItIsNotReadableByAnybodyElse(t *testing.T) {
	dir, err := home.Create(t.TempDir())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	info, err := os.Stat(dir.Root)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("mode is %o, which lets somebody else in", mode)
	}
}

// Creating over an existing one is refused. A "create" that quietly adopts
// what was there is how a fresh deployment ends up pointed at another one's
// database.
func TestCreatingTwiceIsRefused(t *testing.T) {
	root := t.TempDir()
	if _, err := home.Create(root); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := home.Create(root); err == nil {
		t.Fatal("creating over an existing directory was allowed")
	}
}

// The environment names a directory outright, for running against a
// deployment without being inside it.
func TestTheEnvironmentCanNameIt(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "somewhere", home.DirName)
	t.Setenv(home.EnvVar, elsewhere)
	// From a directory that has its own, to prove the environment wins rather
	// than merely filling a gap.
	inside := t.TempDir()
	if _, err := home.Create(inside); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(inside)

	found, ok := home.FromWorkingDirectory()
	if !ok {
		t.Fatal("a named directory was not used")
	}
	if found.Root != elsewhere {
		t.Errorf("used %s, want the named %s", found.Root, elsewhere)
	}
}

// "none" says there is no directory, which is how a test asserts the
// behaviour of a machine that has never had one without depending on what
// happens to exist above the package it lives in.
func TestTheEnvironmentCanSayThereIsNone(t *testing.T) {
	inside := t.TempDir()
	if _, err := home.Create(inside); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(inside)

	t.Setenv(home.EnvVar, "none")
	if found, ok := home.FromWorkingDirectory(); ok {
		t.Errorf("found %s despite being told there is none", found.Root)
	}
}
