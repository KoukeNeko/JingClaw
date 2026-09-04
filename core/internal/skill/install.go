package skill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// Staged is a fetched, verified skill that is not yet in front of the model.
//
// The point of the split. Fetching bytes into a directory that steers nothing
// is a small act; putting standing instructions in front of the model for
// every future session is the large one. A skill lands here first, described
// by what actually arrived rather than by what the source claimed, so the
// decision to activate is made against the real thing.
type Staged struct {
	Name        string `json:"name"`
	Source      Source `json:"source"`
	Description string `json:"description"`
	Version     string `json:"version,omitempty"`

	// Digest is the SKILL.md hash; TreeDigest is the whole directory's.
	Digest     string `json:"digest"`
	TreeDigest string `json:"tree_digest"`
	Size       int64  `json:"size"`

	// Dir is where it is staged, derived from the root and not stored.
	Dir string `json:"-"`
}

// Stage fetches and verifies a skill without installing it.
//
// What arrives is read and hashed here, so a caller — an approval a person is
// about to decide — describes the skill by its own bytes rather than by what
// the agent proposing it said. Nothing is put where the catalogue reads until
// Activate.
func (i *Installer) Stage(ctx context.Context, source Source) (Staged, error) {
	if err := os.MkdirAll(i.Root, 0o755); err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}

	// Fetched somewhere else first, so a repository that turns out not to
	// hold a skill leaves nothing behind.
	fetched, err := os.MkdirTemp(i.Root, ".fetching-*")
	if err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}
	defer os.RemoveAll(fetched)

	if err := fetch(ctx, source, fetched); err != nil {
		return Staged{}, err
	}

	from := fetched
	if source.Path != "" {
		from = filepath.Join(fetched, filepath.FromSlash(source.Path))
		if err := within(fetched, from); err != nil {
			return Staged{}, err
		}
	}

	one, err := Read(from)
	if err != nil {
		return Staged{}, fmt.Errorf("skill: %s holds no skill this can read: %w", source, err)
	}
	if err := safeName(one.Name); err != nil {
		return Staged{}, err
	}

	// The git metadata is not part of a skill and is the largest thing in the
	// clone. Removed before the digest so what is hashed is only what lands.
	if err := os.RemoveAll(filepath.Join(fetched, ".git")); err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}

	tree, size, err := treeDigest(from)
	if err != nil {
		return Staged{}, err
	}

	staged := Staged{
		Name:        one.Name,
		Source:      source,
		Description: one.Description,
		Version:     one.Version,
		Digest:      one.Digest,
		TreeDigest:  tree,
		Size:        size,
		Dir:         filepath.Join(i.staging(), one.Name),
	}

	if err := os.MkdirAll(i.staging(), 0o755); err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}
	if err := i.moveInto(from, staged.Dir); err != nil {
		return Staged{}, err
	}
	// The source is not in the skill's own files, and Activate needs it to
	// write the lock. Beside the staged directory rather than inside it, so it
	// is not part of what lands and not part of the tree digest.
	if err := writeManifest(i.staging(), staged); err != nil {
		return Staged{}, err
	}
	return staged, nil
}

// Activate installs a staged skill, checking it is still the bytes that were
// staged.
//
// The tree is hashed again and compared to what Stage recorded: a decision to
// trust a skill was made against particular bytes, and content swapped between
// the decision and the install would otherwise land unnoticed.
func (i *Installer) Activate(name string) (Locked, error) {
	if err := safeName(name); err != nil {
		return Locked{}, err
	}

	manifest, err := readManifest(i.staging(), name)
	if err != nil {
		return Locked{}, err
	}

	stagedDir := filepath.Join(i.staging(), name)
	one, err := Read(stagedDir)
	if err != nil {
		return Locked{}, fmt.Errorf("skill: staged %q is not readable: %w", name, err)
	}
	if one.Name != name {
		return Locked{}, fmt.Errorf("skill: staged as %q but calls itself %q", name, one.Name)
	}

	tree, _, err := treeDigest(stagedDir)
	if err != nil {
		return Locked{}, err
	}
	if tree != manifest.TreeDigest {
		return Locked{}, fmt.Errorf(
			"skill: %q changed after it was staged; stage it again", name)
	}

	if err := i.moveInto(stagedDir, filepath.Join(i.Root, name)); err != nil {
		return Locked{}, err
	}
	_ = os.Remove(manifestPath(i.staging(), name))
	// Only if empty: os.Remove refuses a directory that still holds other
	// staged skills, which is exactly the right condition.
	_ = os.Remove(i.staging())

	return Locked{
		Name:        one.Name,
		From:        manifest.Source,
		Digest:      one.Digest,
		TreeDigest:  tree,
		Version:     one.Version,
		InstalledAt: i.now(),
	}, nil
}

// Install fetches one skill and installs it in a single step.
//
// Stage then Activate, for the operator at the CLI who has the source in front
// of them and is deciding by looking at it rather than at an approval.
func (i *Installer) Install(ctx context.Context, source Source) (Locked, error) {
	staged, err := i.Stage(ctx, source)
	if err != nil {
		return Locked{}, err
	}
	return i.Activate(staged.Name)
}

// staging is where fetched-but-not-installed skills wait. A dot-directory, so
// the catalogue never reads it as a skill.
func (i *Installer) staging() string {
	return filepath.Join(i.Root, ".staged")
}

// moveInto moves a directory into place, over whatever was there.
//
// The old one is moved aside rather than deleted first, so a failed rename
// leaves what was working where it was rather than nothing at all.
func (i *Installer) moveInto(from, to string) error {
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
		return fmt.Errorf("skill: installing into %s: %w", to, err)
	}

	if replaced != "" {
		_ = os.RemoveAll(replaced)
	}
	return nil
}

// safeName refuses a skill name that could not be a directory beside the
// others: it becomes one, and it comes from frontmatter the installer did not
// write. A separator or a climb would put the skill outside the tree, and a
// leading dot would hide it among the installer's own working directories or
// collide with one.
func safeName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("skill: %q cannot name a skill", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("skill: %q cannot name a skill: it has a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("skill: %q cannot name a skill: it begins with a dot", name)
	}
	return nil
}

// Remove deletes an installed skill.
func (i *Installer) Remove(name string) error {
	if err := safeName(name); err != nil {
		return err
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

// treeDigest hashes every file under a directory, so what is compared later is
// the whole skill and not one file in it.
//
// Deterministic: files are hashed in sorted path order, each preceded by its
// path and length, so a rename cannot pass for an edit and a file cannot be
// confused with the one after it.
func treeDigest(dir string) (digest string, size int64, err error) {
	var files []string
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return "", 0, fmt.Errorf("skill: hashing %s: %w", dir, walkErr)
	}
	sort.Strings(files)

	sum := sha256.New()
	for _, rel := range files {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", 0, fmt.Errorf("skill: hashing %s: %w", dir, err)
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", rel, len(content))
		sum.Write(content)
		size += int64(len(content))
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil)), size, nil
}

// manifestPath is where a staged skill's source is recorded, beside the staged
// directory rather than inside it so it is not part of what lands.
func manifestPath(staging, name string) string {
	return filepath.Join(staging, name+".json")
}

func writeManifest(staging string, staged Staged) error {
	raw, err := json.MarshalIndent(staged, "", "  ")
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	if err := os.WriteFile(manifestPath(staging, staged.Name), raw, 0o600); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return nil
}

// readManifest loads what Stage recorded, and says plainly when nothing is
// staged under that name.
func readManifest(staging, name string) (Staged, error) {
	raw, err := os.ReadFile(manifestPath(staging, name))
	if os.IsNotExist(err) {
		return Staged{}, fmt.Errorf("skill: no skill named %q is staged", name)
	}
	if err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}
	var staged Staged
	if err := json.Unmarshal(raw, &staged); err != nil {
		return Staged{}, fmt.Errorf("skill: the staging record for %q is unreadable: %w", name, err)
	}
	staged.Dir = filepath.Join(staging, name)
	return staged, nil
}
