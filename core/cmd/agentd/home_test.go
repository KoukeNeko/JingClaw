package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// With a .JingClaw directory, everything a deployment owns is inside it.
func TestTheDirectoryDecidesEveryPath(t *testing.T) {
	root := t.TempDir()
	dir, err := home.Create(root)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(root)

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

// Without one, nothing changes: the platform locations are used exactly as
// they were, so a deployment that already works keeps working.
func TestWithoutOneThePlatformLocationsAreUsed(t *testing.T) {
	// Somewhere with no .JingClaw above it.
	t.Chdir(t.TempDir())

	database, err := databasePath("")
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	if filepath.Base(filepath.Dir(database)) == home.DirName {
		t.Errorf("a directory was invented at %s", database)
	}

	if got := config.Defaults().Workspace.Root; got != "." {
		t.Errorf("the workspace is %q, want the working directory", got)
	}
}

// An explicit setting still wins. The directory is a default, not a rule.
func TestAnExplicitPathStillWins(t *testing.T) {
	root := t.TempDir()
	if _, err := home.Create(root); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Chdir(root)

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
