// Package sandbox confines what an approved command can reach.
//
// A human approval and a sandbox answer different questions:
//
//	approval  authorises intent   — did somebody mean to run npm install?
//	sandbox   authorises effects  — and what will its 1,200 dependencies do?
//
// The first is answerable by a person. The second is not: approving a build
// is approving the execution of a great deal of code nobody has read, and no
// amount of care at the approval makes that untrue.
//
// So the gain is uneven and worth stating plainly. For "git status" this is
// defence in depth and little more. For "make", "./configure", "go generate"
// or "npm install" it is most of the protection there is.
//
// What it does not do: a command allowed the network and able to read a
// secret can still send it; a workspace is writable and so can be spoiled;
// and anything written now can be run outside this later.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Policy is what one execution is allowed to reach.
type Policy struct {
	// Writable are the directories the command may change. Everything else
	// on the machine is readable and not writable.
	Writable []string

	// Unreadable are directories to hide, whatever else is allowed.
	//
	// Confinement of writes says nothing about reads: a sandboxed command can
	// still open ~/.ssh, and "it has no network" only means it cannot send
	// what it found today. So the places worth not showing it are named.
	Unreadable []string

	// Network allows outbound connections. Off is the ordinary case: most of
	// what an agent runs is a build or a test, and neither needs one.
	Network bool
}

// Directories is where a confined command may write, beside the workspace.
//
// Its own, rather than the caller's real ones. A build cache is a directory a
// compiler must write to, and the usual answer is to allow ~/.cache — which
// is the first hole in a policy that then acquires one for every tool. These
// belong to the deployment, and pointing the tools at them is what makes
// hiding the real home practical rather than merely strict.
type Directories struct {
	Home  string
	Temp  string
	Cache string
}

// Under lays out the sandbox's own directories beneath a root, making them.
func Under(root string) (Directories, error) {
	dirs := Directories{
		Home:  filepath.Join(root, "home"),
		Temp:  filepath.Join(root, "tmp"),
		Cache: filepath.Join(root, "cache"),
	}

	for _, dir := range []string{dirs.Home, dirs.Temp, dirs.Cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Directories{}, fmt.Errorf("sandbox: create %s: %w", dir, err)
		}
	}
	return dirs, nil
}

// Environment is what a confined command is told about where things are.
//
// Every one of these has a default under the real home, and left alone a
// compiler writes there and a package manager reads credentials from beside
// it. Pointing them into the sandbox is what makes the confinement something
// other than a list of exceptions.
func (d Directories) Environment() []string {
	return []string{
		"HOME=" + d.Home,
		"TMPDIR=" + d.Temp,
		"XDG_CACHE_HOME=" + d.Cache,
		"GOCACHE=" + filepath.Join(d.Cache, "go-build"),
		"GOMODCACHE=" + filepath.Join(d.Cache, "go-mod"),
		"npm_config_cache=" + filepath.Join(d.Cache, "npm"),
	}
}

// Writable is where a confined command may write.
func (d Directories) Writable() []string {
	return []string{d.Home, d.Temp, d.Cache}
}

// ErrUnavailable says this machine cannot confine anything.
//
// Returned rather than shrugged off. A sandbox that runs the command anyway
// when it cannot confine it is worse than no sandbox, because the operator
// believes there is one — so an unavailable backend is a refusal, and every
// caller has to decide what to do about it rather than being able to not
// notice.
var ErrUnavailable = errors.New("sandbox: not available on this machine")

// resolve is the path the kernel will see.
//
// Symlinks followed, because the sandbox matches on what a path resolves to
// and not on what was written. On macOS this is not an edge case: /tmp and
// /var are both links into /private, so a workspace under either is a
// workspace the profile permits and the kernel does not recognise — and what
// that looks like is a command that cannot write to the directory it was
// given.
//
// A path that does not exist yet is kept as it was written. There is nothing
// to resolve, and refusing would mean a directory could not be permitted
// before it was made.
func resolve(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve %s: %w", dir, err)
	}

	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return absolute, nil
	}
	return real, nil
}

// Describe says what this machine will actually enforce.
//
// For the line an operator reads at startup. "Confinement: on" is not enough
// on Linux, where what is available depends on the kernel: the filesystem
// rules and the network rules arrived four versions apart, and a deployment
// that asked for both and got one should be able to see that before it
// matters.
func Describe() string {
	if !Available() {
		return "not available here"
	}
	return describeBackend()
}
