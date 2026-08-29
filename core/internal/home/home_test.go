package home_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// Where a deployment lives must not depend on where somebody typed the
// daemon's name. This is the whole point of the resolver, and the failure it
// prevents had happened three times in one day: settings edited in one place,
// a daemon reading another, and no sign that the two were different.
func TestTheWorkingDirectoryDoesNotDecide(t *testing.T) {
	t.Setenv(home.EnvVar, "")

	first, ok := home.Resolve()
	if !ok {
		t.Fatal("nothing resolved")
	}

	deep := filepath.Join(t.TempDir(), "some", "project")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory that looks exactly like a deployment, in the place the old
	// resolver would have walked up to and adopted.
	if _, err := home.Create(filepath.Join(deep, home.DirName)); err != nil {
		t.Fatal(err)
	}

	restore, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(restore) })
	if err := os.Chdir(deep); err != nil {
		t.Fatal(err)
	}

	second, ok := home.Resolve()
	if !ok {
		t.Fatal("nothing resolved from the other directory")
	}
	if second.Root != first.Root {
		t.Errorf("standing in %s changed the deployment to %s, want %s",
			deep, second.Root, first.Root)
	}
}

// The default is under the user's home and the same on every platform: an
// operator has to be able to say where it is without knowing which of three
// conventions applied.
func TestTheDefaultIsUnderTheUsersHome(t *testing.T) {
	dir, err := home.Default()
	if err != nil {
		t.Fatalf("default: %v", err)
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}

	want := filepath.Join(userHome, home.DirName)
	if dir.Root != want {
		t.Errorf("default is %s, want %s", dir.Root, want)
	}
}

// Everything is inside it, and the workspace is inside it too: a deployment
// set up to try the thing out must not be able to reach the project it was set
// up in.
func TestEverythingIsInsideIt(t *testing.T) {
	root := t.TempDir()
	dir, err := home.Create(filepath.Join(root, home.DirName))
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
	dir, err := home.Create(filepath.Join(t.TempDir(), home.DirName))
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
	if _, err := home.Create(filepath.Join(root, home.DirName)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := home.Create(filepath.Join(root, home.DirName)); err == nil {
		t.Fatal("creating over an existing directory was allowed")
	}
}

// The environment names a directory outright, for running against a
// deployment without being inside it.
func TestTheEnvironmentCanNameIt(t *testing.T) {
	elsewhere := filepath.Join(t.TempDir(), "somewhere", home.DirName)
	t.Setenv(home.EnvVar, elsewhere)
	// From a directory that has one of its own, to prove the environment wins
	// rather than merely filling a gap — and that the directory somebody is
	// standing in is not consulted at all.
	inside := t.TempDir()
	if _, err := home.Create(filepath.Join(inside, home.DirName)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(inside)

	found, ok := home.Resolve()
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
	if _, err := home.Create(filepath.Join(inside, home.DirName)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(inside)

	t.Setenv(home.EnvVar, "none")
	if found, ok := home.Resolve(); ok {
		t.Errorf("found %s despite being told there is none", found.Root)
	}
}
