// Package home locates the directory a deployment keeps everything in.
//
// Without one, the parts of a running agent live in four places: the
// configuration under the platform's config directory, the database under its
// data directory, the discovery file under its runtime directory, and the
// workspace wherever the daemon happened to be started. Each is defensible and
// together they are impossible to hold in your head, back up, or move.
//
// A .JingClaw directory collects them. When one exists it decides every path;
// when none does, the platform locations are used exactly as before, so
// nothing that already works changes.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DirName is what the directory is called.
//
// Dotted and capitalised the way the project is: it sits beside .git and is
// meant to be as easy to ignore and as obvious when listed.
const DirName = ".JingClaw"

// Names of what lives inside it.
const (
	ConfigName    = "config.toml"
	WorkspaceName = "workspace"
	DataName      = "data"
	RunName       = "run"
)

// Dir is a resolved JingClaw directory.
type Dir struct {
	// Root is the .JingClaw directory itself.
	Root string
}

// Find looks for a .JingClaw directory at start and in each directory above
// it.
//
// Walking up rather than checking only the current directory, because a daemon
// started from a subdirectory of a project should be the same daemon as one
// started from its top. Not doing so means two of them, with separate
// databases, differing by which directory somebody happened to be in.
func Find(start string) (Dir, bool) {
	current, err := filepath.Abs(start)
	if err != nil {
		return Dir{}, false
	}

	for {
		candidate := filepath.Join(current, DirName)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return Dir{Root: candidate}, true
		}

		parent := filepath.Dir(current)
		if parent == current {
			// The filesystem root, reached without finding one.
			return Dir{}, false
		}
		current = parent
	}
}

// Create makes a .JingClaw directory in at, with the places things go.
//
// Refuses an existing one rather than merging into it: "create" that quietly
// adopts whatever was already there is how somebody ends up pointing a fresh
// deployment at another one's database.
func Create(at string) (Dir, error) {
	root := filepath.Join(at, DirName)

	if _, err := os.Stat(root); err == nil {
		return Dir{}, fmt.Errorf("home: %s already exists", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Dir{}, err
	}

	dir := Dir{Root: root}
	// 0700 throughout: the database holds every conversation and the
	// directory holds credentials, and neither is anybody else's business.
	for _, path := range []string{dir.Root, dir.Workspace(), dir.Data(), dir.Run()} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return Dir{}, fmt.Errorf("home: create %s: %w", path, err)
		}
	}

	return dir, nil
}

// ConfigFile is where the configuration lives.
func (d Dir) ConfigFile() string { return filepath.Join(d.Root, ConfigName) }

// Workspace is the default workspace: what the agent may read and change when
// nothing else says otherwise.
//
// Inside the directory rather than beside it, so that a deployment somebody
// set up to try the thing out cannot reach the project it was set up in. An
// operator who wants it pointed at their code says so.
func (d Dir) Workspace() string { return filepath.Join(d.Root, WorkspaceName) }

// Data holds the database and the artifact store.
func (d Dir) Data() string { return filepath.Join(d.Root, DataName) }

// Run holds the discovery file, which is how a client finds the daemon.
func (d Dir) Run() string { return filepath.Join(d.Root, RunName) }

// SecretFile is where a named credential lives.
func (d Dir) SecretFile(name string) string { return filepath.Join(d.Root, name) }

// FromWorkingDirectory finds the directory for the process's own location.
func FromWorkingDirectory() (Dir, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return Dir{}, false
	}
	return Find(cwd)
}
