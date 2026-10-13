package daemon

import (
	"os"
	"path/filepath"
	"strings"
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

	if got := workspaceRoot(); got != dir.Workspace() {
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

	got := workspaceRoot()
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

// Asking where a deployment keeps things is a question, not the first step of
// setting one up. Somebody running this on a machine to find out what is there
// should be able to run it again and get the same answer.
func TestAskingWhereThingsAreCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	t.Setenv(home.EnvVar, root)

	if err := run([]string{"--print-paths"}); err != nil {
		t.Fatalf("--print-paths: %v", err)
	}

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		listing, _ := os.ReadDir(root)
		names := make([]string, 0, len(listing))
		for _, entry := range listing {
			names = append(names, entry.Name())
		}
		t.Errorf("asking created %s, containing %v", root, names)
	}
}

// The settings are not a file, and the answer to where they are says so.
//
// A line that named a path would send somebody looking for a file that nothing
// reads, and one that was missing would read as the program not knowing.
func TestAskingWhereThingsAreSaysTheSettingsAreInTheDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), home.DirName)
	t.Setenv(home.EnvVar, root)

	var said strings.Builder
	if err := reportPaths(&said, config.Defaults()); err != nil {
		t.Fatalf("report: %v", err)
	}

	var settings string
	for _, line := range strings.Split(said.String(), "\n") {
		if strings.HasPrefix(line, "settings") {
			settings = line
		}
	}
	if !strings.Contains(settings, "database") {
		t.Errorf("there is no line saying the settings are in the database:\n%s", said.String())
	}
	if strings.Contains(said.String(), "config.toml") {
		t.Errorf("a file the settings are not in is named:\n%s", said.String())
	}

	// And the rest is where it was.
	for _, want := range []string{
		filepath.Join(root, "data", "jingclaw.db"),
		filepath.Join(root, "workspace"),
		filepath.Join(root, "run", "daemon.json"),
	} {
		if !strings.Contains(said.String(), want) {
			t.Errorf("the report does not say %s:\n%s", want, said.String())
		}
	}
}
