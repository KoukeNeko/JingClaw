package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// seatbelt is the program that applies a profile. Undocumented and not a
// supported interface, which is recorded where somebody will read it: see the
// note on Available.
const seatbelt = "/usr/bin/sandbox-exec"

// SeatbeltEnv names the program to use instead, for a check that has to be a
// machine where confinement is unavailable.
//
// That case is the one this feature turns on and the one no ordinary machine
// can reach: every Mac has sandbox-exec, so "what happens when it is missing"
// is unreachable without saying where to look. Read only here, and never
// consulted for anything but the path.
const SeatbeltEnv = "JINGCLAW_SANDBOX_EXEC"

// applier is the program that will be run.
func applier() string {
	if named := os.Getenv(SeatbeltEnv); named != "" {
		return named
	}
	return seatbelt
}

// ErrUnavailable says this machine cannot confine anything.
//
// Returned rather than shrugged off. A sandbox that runs the command anyway
// when it cannot confine it is worse than no sandbox, because the operator
// believes there is one — so an unavailable backend is a refusal, and every
// caller has to decide what to do about it rather than being able to not
// notice.
var ErrUnavailable = errors.New("sandbox: not available on this machine")

// Available reports whether commands can be confined here.
//
// macOS only, for now, and through an interface Apple does not support: the
// documented one was deprecated and its replacement is not offered to
// programs outside the system. What that means in practice is that this can
// stop working at an OS update, and the answer to that is to find out at
// startup and to refuse rather than to quietly stop confining anything.
func Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	info, err := os.Stat(applier())
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// Wrap turns a command into the same command, confined.
//
// The profile is written to a file rather than passed on the command line.
// An inline one is an argument, and a deployment with enough writable
// directories produces an argument longer than the kernel will accept — a
// failure that arrives as "argument list too long" from a program nobody
// typed.
//
// The caller removes the file when the command has finished; the returned
// function does that.
func Wrap(policy Policy, program string, args []string) (
	wrapped string, wrappedArgs []string, cleanup func(), err error,
) {
	if !Available() {
		return "", nil, nil, ErrUnavailable
	}

	profile, err := Profile(policy)
	if err != nil {
		return "", nil, nil, err
	}

	file, err := os.CreateTemp("", "jingclaw-sandbox-*.sb")
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}
	remove := func() { _ = os.Remove(file.Name()) }

	if _, err := file.WriteString(profile); err != nil {
		_ = file.Close()
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}
	if err := file.Close(); err != nil {
		remove()
		return "", nil, nil, fmt.Errorf("sandbox: write the profile: %w", err)
	}

	return applier(), append([]string{"-f", file.Name(), program}, args...), remove, nil
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

// LooksConfined reports whether a program refused because of the sandbox.
//
// sandbox-exec reports a policy refusal on stderr and exits non-zero, which
// on its own is indistinguishable from the program having failed. Said here
// so the observation the model gets can name the cause rather than leaving it
// to guess why "touch" did not work.
func LooksConfined(output string) bool {
	for _, said := range []string{
		"Operation not permitted",
		"sandbox-exec",
		"deny file-write",
		"deny network",
	} {
		if strings.Contains(output, said) {
			return true
		}
	}
	return false
}
