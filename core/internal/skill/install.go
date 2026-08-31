package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// fetchTimeout bounds a clone. A repository that cannot be reached in this
// long is one to be told about rather than waited on.
const fetchTimeout = 3 * time.Minute

// Installer fetches skills and keeps the record of what it fetched.
type Installer struct {
	// Root is the skills directory.
	Root string

	// Now is the clock, for the record.
	Now func() time.Time
}

// Install fetches one skill and writes down exactly what arrived.
//
// The directory is named by what the skill calls itself, not by anything in
// the source: two sources offering a skill of the same name is a collision to
// refuse rather than to resolve by path, and a repository that could choose
// its own directory could choose one already taken.
func (i *Installer) Install(ctx context.Context, source Source) (Locked, error) {
	if err := os.MkdirAll(i.Root, 0o755); err != nil {
		return Locked{}, fmt.Errorf("skill: %w", err)
	}

	// Fetched somewhere else first, so a repository that turns out not to
	// hold a skill leaves nothing behind — and so that a failed install
	// cannot half-replace one that was working.
	staged, err := os.MkdirTemp(i.Root, ".fetching-*")
	if err != nil {
		return Locked{}, fmt.Errorf("skill: %w", err)
	}
	defer os.RemoveAll(staged)

	if err := fetch(ctx, source, staged); err != nil {
		return Locked{}, err
	}

	from := staged
	if source.Path != "" {
		from = filepath.Join(staged, filepath.FromSlash(source.Path))
		// Checked after joining rather than before: a path that climbs out
		// through a symlink in the repository would pass a check on the text
		// alone.
		if err := within(staged, from); err != nil {
			return Locked{}, err
		}
	}

	// Read before it is installed, so a directory that is not a skill is
	// refused with the same reason the catalogue would have given.
	one, err := Read(from)
	if err != nil {
		return Locked{}, fmt.Errorf("skill: %s holds no skill this can read: %w", source, err)
	}

	// The git metadata is not part of a skill and is the largest thing in the
	// clone. Removed before the move so what lands is only what was asked for.
	if err := os.RemoveAll(filepath.Join(staged, ".git")); err != nil {
		return Locked{}, fmt.Errorf("skill: %w", err)
	}

	if err := i.replace(from, one.Name); err != nil {
		return Locked{}, err
	}

	return Locked{
		Name:        one.Name,
		From:        source,
		Digest:      one.Digest,
		Version:     one.Version,
		InstalledAt: i.now(),
	}, nil
}

// replace moves a fetched skill into place, over whatever was there.
//
// The old one is moved aside rather than deleted first, so a failed rename
// leaves the working skill where it was rather than nothing at all.
func (i *Installer) replace(from, name string) error {
	to := filepath.Join(i.Root, name)

	replaced := ""
	if _, err := os.Stat(to); err == nil {
		replaced = to + ".replacing"
		if err := os.RemoveAll(replaced); err != nil {
			return fmt.Errorf("skill: %w", err)
		}
		if err := os.Rename(to, replaced); err != nil {
			return fmt.Errorf("skill: %w", err)
		}
	}

	if err := os.Rename(from, to); err != nil {
		if replaced != "" {
			// Put back what was working. The install failed either way; what
			// this decides is whether the deployment still has the skill it
			// had this morning.
			_ = os.Rename(replaced, to)
		}
		return fmt.Errorf("skill: installing %s: %w", name, err)
	}

	if replaced != "" {
		_ = os.RemoveAll(replaced)
	}
	return nil
}

// Remove deletes an installed skill.
func (i *Installer) Remove(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("skill: %q cannot name a skill", name)
	}

	at := filepath.Join(i.Root, name)
	if _, err := os.Stat(at); os.IsNotExist(err) {
		return fmt.Errorf("skill: there is no skill named %q installed", name)
	}
	return os.RemoveAll(at)
}

func (i *Installer) now() time.Time {
	if i.Now == nil {
		return time.Now()
	}
	return i.Now()
}

// fetch clones one commit into a directory.
//
// Shallow and by revision, so what arrives is the one commit that was named
// rather than a history somebody could later rewrite around it. Written as
// three plumbing commands instead of `git clone` because clone has no way to
// take a bare revision — only a branch or tag, which are the names this
// refuses on purpose.
func fetch(ctx context.Context, source Source, into string) error {
	timed, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", source.Repository},
		{"fetch", "--quiet", "--depth", "1", "origin", source.Commit},
		{"checkout", "--quiet", "FETCH_HEAD"},
	}

	for _, step := range steps {
		if err := run(timed, into, step); err != nil {
			return fmt.Errorf("skill: fetching %s: %w", source, err)
		}
	}
	return nil
}

// run is one git command, told to ask nobody anything.
//
// An install that stopped for a password would stop in a place nobody is
// watching — inside a command, behind whatever the caller is doing — so a
// repository needing credentials fails instead, with git's own words.
func run(ctx context.Context, in string, args []string) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = in

	// Nothing inherited. The daemon's environment holds provider credentials,
	// and a repository address somebody typed is not a reason to hand them
	// over.
	command.Env = []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + in,
	}

	var said bytes.Buffer
	command.Stderr = &said
	command.Stdout = &said

	if err := command.Run(); err != nil {
		reason := strings.TrimSpace(said.String())
		if reason == "" {
			reason = err.Error()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("it did not finish within %s: %s", fetchTimeout, reason)
		}
		return errors.New(reason)
	}
	return nil
}

// within refuses a path that leaves the directory it should be inside.
//
// Resolved rather than compared as text, because a repository can contain a
// symlink and the path it points at is what would actually be read.
func within(root, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("skill: %s is not in the repository: %w", path, err)
	}

	if realPath != realRoot && !strings.HasPrefix(realPath, realRoot+string(filepath.Separator)) {
		return fmt.Errorf("skill: that path leads outside the repository")
	}
	return nil
}
