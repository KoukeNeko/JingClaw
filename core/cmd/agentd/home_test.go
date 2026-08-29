package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// Everything a deployment owns is inside its own directory.
func TestTheDirectoryDecidesEveryPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	dir, err := home.Create(root)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(home.EnvVar, root)

	database, err := databasePath("")
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if filepath.Dir(database) != dir.Data() {
		t.Errorf("the database is at %s, want it under %s", database, dir.Data())
	}

	configFile, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if configFile != dir.ConfigFile() {
		t.Errorf("the configuration is at %s, want %s", configFile, dir.ConfigFile())
	}

	if got := config.Defaults().Workspace.Root; got != dir.Workspace() {
		t.Errorf("the workspace is %s, want %s", got, dir.Workspace())
	}
}

// The working directory decides nothing. Standing inside something that looks
// exactly like a deployment must not make it the one that answers: that is how
// the settings somebody edits stop being the settings that run.
func TestTheWorkingDirectoryDecidesNothing(t *testing.T) {
	named := filepath.Join(t.TempDir(), home.DirName)
	if _, err := home.Create(named); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(home.EnvVar, named)

	decoy := t.TempDir()
	if _, err := home.Create(filepath.Join(decoy, home.DirName)); err != nil {
		t.Fatalf("create decoy: %v", err)
	}
	t.Chdir(decoy)

	database, err := databasePath("")
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if want := filepath.Join(named, "data", "jingclaw.db"); database != want {
		t.Errorf("standing in %s put the database at %s, want %s", decoy, database, want)
	}
}

// The workspace is never the working directory. "Whatever you happened to be
// standing in" is not a setting, and as a default it hands a fresh install the
// contents of the first project somebody starts it from.
func TestTheWorkspaceIsNeverTheWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	if _, err := home.Create(root); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(home.EnvVar, root)
	t.Chdir(t.TempDir())

	got := config.Defaults().Workspace.Root
	if got == "." {
		t.Fatal("the workspace defaulted to the working directory")
	}
	if want := filepath.Join(root, "workspace"); got != want {
		t.Errorf("the workspace is %q, want %q", got, want)
	}
}

// An explicit setting still wins. The directory is a default, not a rule.
func TestAnExplicitPathStillWins(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	if _, err := home.Create(root); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv(home.EnvVar, root)

	elsewhere := filepath.Join(t.TempDir(), "somewhere-else")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}

	database, err := databasePath(elsewhere)
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if filepath.Dir(database) != elsewhere {
		t.Errorf("the database is at %s, want it under the configured %s", database, elsewhere)
	}
}
