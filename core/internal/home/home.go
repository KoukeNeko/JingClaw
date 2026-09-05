// Package home locates the directory a deployment keeps everything in.
//
// Without one, the parts of a running agent live in four places: the
// configuration under the platform's config directory, the database under its
// data directory, the discovery file under its runtime directory, and the
// workspace wherever the daemon happened to be started. Each is defensible and
// together they are impossible to hold in your head, back up, or move.
//
// One directory collects them, and it is always the same one. Where somebody
// happens to be standing when they type the name does not take part: a daemon
// whose database depended on that is one that quietly becomes two, and then
// the settings you edited are not the settings that ran.
package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

// DirName is what the directory is called, under the user's home.
//
// Lower case because it is a private state tree rather than a document: it is
// typed at a shell far more often than it is read in a file listing.
const DirName = ".jingclaw"

// Names of what lives inside it.
const (
	ConfigName    = "config.toml"
	WorkspaceName = "workspace"
	DataName      = "data"
	RunName       = "run"
	LogName       = "log"
	BinName       = "bin"
	SkillsName    = "skills"
)

// Dir is a resolved JingClaw directory.
type Dir struct {
	// Root is the directory itself.
	Root string
}

// Default is the one place a deployment lives, unless the environment names
// another.
//
// Under the user's home and the same on every platform. The platform-native
// location is not used: this is a daemon's private state, not a document
// somebody opens in Finder, and an operator has to be able to predict where it
// is without knowing which of three conventions applied.
func Default() (Dir, error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return Dir{}, fmt.Errorf("home: locate user home: %w", err)
	}
	return Dir{Root: filepath.Join(userHome, DirName)}, nil
}

// Create makes the directory at root, with the places things go.
//
// Refuses an existing one rather than merging into it: "create" that quietly
// adopts whatever was already there is how somebody ends up pointing a fresh
// deployment at another one's database.
func Create(root string) (Dir, error) {
	if _, err := os.Stat(root); err == nil {
		return Dir{}, fmt.Errorf("home: %s already exists", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Dir{}, err
	}

	dir := Dir{Root: root}
	// Owner-only throughout: the database holds every conversation and the
	// directory holds credentials, and neither is anybody else's business.
	// MkdirAll's mode is only advisory on Windows, so each directory is locked
	// down explicitly after it is made.
	for _, path := range []string{dir.Root, dir.Workspace(), dir.Data(), dir.Run()} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return Dir{}, fmt.Errorf("home: create %s: %w", path, err)
		}
		if err := fsperm.Restrict(path); err != nil {
			return Dir{}, fmt.Errorf("home: %w", err)
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

// Skills is where the instruction packs an operator has installed live.
//
// Beside AGENTS.md and PERSONA.md rather than in the workspace: they are
// things the operator put here, and the workspace is what the agent may
// change. Kept in there, a skill would be a file the agent could rewrite
// while doing a job.
func (d Dir) Skills() string { return filepath.Join(d.Root, SkillsName) }

// Log is where a deployment that nobody is watching writes what it says.
//
// Only a service needs this. Run from a terminal the parts write to that
// terminal, and a second copy on disk would be one more thing to remember to
// look at.
func (d Dir) Log() string { return filepath.Join(d.Root, LogName) }

// Bin is where the service keeps its own copy of the program.
//
// A copy, because launchd cannot open a program that lives inside a folder
// macOS protects — Documents, Desktop, Downloads — and a checkout is usually
// in one. The service hangs in the loader before main, forever, with nothing
// written anywhere to say so. A copy here is also a program that `go build`
// does not replace underneath a running service.
func (d Dir) Bin() string { return filepath.Join(d.Root, BinName) }

// SecretFile is where a named credential lives.
func (d Dir) SecretFile(name string) string { return filepath.Join(d.Root, name) }

// EnvVar names a directory outright, skipping the search.
//
// For running against a deployment without being inside it, and for a test
// that must not pick up whatever happens to exist above the package it lives
// in. Set to None to say there is no directory at all, which is how a test
// asserts the behaviour of a machine that has never had one.
const EnvVar = "JINGCLAW_HOME"

// None is the value of EnvVar that means there is no deployment directory.
const None = "none"

// Resolve settles which deployment this process belongs to.
//
// The environment names one, or it is the default. The working directory does
// not take part, and that is the point: a daemon whose database depended on
// where somebody happened to type its name is one that quietly becomes two,
// and then the settings you edited are not the settings that ran.
//
// The directory need not exist. Whether to create one is the caller's
// decision, and asking where a deployment lives must not make it.
func Resolve() (Dir, bool) {
	switch named := strings.TrimSpace(os.Getenv(EnvVar)); named {
	case "":
	case None:
		return Dir{}, false
	default:
		absolute, err := filepath.Abs(named)
		if err != nil {
			return Dir{}, false
		}
		return Dir{Root: absolute}, true
	}

	dir, err := Default()
	if err != nil {
		return Dir{}, false
	}
	return dir, true
}
