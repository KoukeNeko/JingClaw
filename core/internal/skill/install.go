package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// fetchTimeout bounds a clone. A repository that cannot be reached in this
// long is one to be told about rather than waited on.
const fetchTimeout = 3 * time.Minute

// fetchWaitDelay is how long a git command that has been told to stop gets to
// close its output before it is abandoned. Without it, a git that spawned a
// helper which outlives it keeps the command waiting on a pipe long after the
// timeout above fired. A variable only so that a test need not wait it out.
var fetchWaitDelay = 5 * time.Second

// maxGitSaid is how much of what git said is passed on in an error. A remote
// chooses what it prints, and an error is read by a person and sent to
// whatever asked.
const maxGitSaid = 2048

// keptOfGit is how much of what git prints is held while it runs: twice what an
// error carries, so that the cut made while it prints falls in the half that is
// dropped afterwards, and an address that straddles it is never half of one in
// what is passed on.
const keptOfGit = 2 * maxGitSaid

var (
	// mutating is held by everything that changes what is staged, installed or
	// recorded, from reading installed.json to writing it back: Stage's last
	// step, Activate, Remove and Discard. Two of those interleaving would have
	// one of them write a record that forgot the other's.
	//
	// A mutex in this process and nothing more. The jingclaw skills command is
	// another process and does not share it; a lock file in the skills
	// directory would, but that directory has to be empty after an install
	// that failed, and a lock file is not nothing.
	mutating sync.Mutex

	// fetching is held for the whole of a Stage and is only ever tried. A
	// second Stage while one is running is answered with ErrBusy, because
	// waiting for it would be waiting for a clone.
	fetching sync.Mutex
)

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

// Replaces is whether activating this skill would put it over something that is
// already in the skills directory under its name, an installed skill or a
// directory somebody placed there. Activate does not ask, and the old one is gone
// afterwards.
func (s Staged) Replaces() bool {
	if s.Dir == "" {
		return false
	}
	// Dir is <skills>/.staged/<name>.
	_, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(s.Dir)), s.Name))
	return err == nil
}

// Stage fetches and verifies a skill without installing it.
//
// What arrives is read and hashed here, so a caller — an approval a person is
// about to decide — describes the skill by its own bytes rather than by what
// the agent proposing it said. Nothing is put where the catalogue reads until
// Activate.
//
// One at a time: a Stage while another is still fetching is refused with
// ErrBusy. Everything up to the last step happens in a directory of its own,
// and only the move into the staging area and the record of it are done where
// Activate, Remove and Discard cannot interleave.
func (i *Installer) Stage(ctx context.Context, source Source) (Staged, error) {
	if !fetching.TryLock() {
		return Staged{}, ErrBusy
	}
	defer fetching.Unlock()

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
		return Staged{}, because(ErrSource, err)
	}

	from := fetched
	if source.Path != "" {
		from = filepath.Join(fetched, filepath.FromSlash(source.Path))
		if err := within(fetched, from); err != nil {
			return Staged{}, because(ErrSource, err)
		}
	}

	// The git metadata is not part of a skill and is the largest thing in the
	// clone. Removed before the digest so what is hashed is only what lands.
	if err := os.RemoveAll(filepath.Join(fetched, ".git")); err != nil {
		return Staged{}, fmt.Errorf("skill: %w", err)
	}

	// Before anything in the tree is read as a skill: a SKILL.md that is a link
	// would be read from wherever it points. This is also the refusal of a link
	// anywhere else in what lands.
	tree, size, err := TreeDigest(from)
	if err != nil {
		return Staged{}, err
	}

	// Before it is read as one: the walk above has bounded every file, and the
	// instructions have to fit in the reply a person decides on.
	if info, err := os.Stat(filepath.Join(from, FileName)); err == nil && info.Size() > MaxSkillMDBytes {
		return Staged{}, because(ErrSource, fmt.Errorf(
			"skill: %s is %d bytes; the instructions of a skill are at most %d", FileName, info.Size(), MaxSkillMDBytes))
	}

	one, err := Read(from)
	if err != nil {
		return Staged{}, because(ErrSource, fmt.Errorf("skill: %s holds no skill this can read: %w", source, err))
	}
	if err := safeName(one.Name); err != nil {
		return Staged{}, because(ErrSource, err)
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

	mutating.Lock()
	defer mutating.Unlock()

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
// staged, and records where it came from.
//
// The tree is hashed again and compared to what Stage recorded: a decision to
// trust a skill was made against particular bytes, and content swapped between
// the decision and the install would otherwise land unnoticed.
//
// Replaces a skill of the same name without asking, and does not check what
// anybody was shown. A caller that has a person's decision to honour uses
// ActivateExpecting.
func (i *Installer) Activate(name string) (Locked, error) {
	return i.activate(name, "", true)
}

// ActivateExpecting installs a staged skill only if it is the one that was
// reviewed, and only over nothing unless told it may replace.
//
// treeDigest is the digest the person deciding was shown. It is compared to
// the staged skill's record and to the tree as it is on disk now, inside the
// critical section the install happens in, so nothing can change between the
// comparison and the move: a skill staged again under the same name since, or
// a staged file edited, is refused with ErrStale rather than installed under a
// decision made about something else.
//
// A directory already at the skill's name — installed, or placed there by hand
// — is ErrCollision unless replace is true. The record of the install is
// written before the lock is released.
func (i *Installer) ActivateExpecting(name, treeDigest string, replace bool) (Locked, error) {
	if treeDigest == "" {
		return Locked{}, errors.New("skill: activating needs the tree digest that was reviewed")
	}
	return i.activate(name, treeDigest, replace)
}

// activate is Activate and ActivateExpecting. An empty expect checks nothing
// but that the staged skill is the one that was staged.
func (i *Installer) activate(name, expect string, replace bool) (Locked, error) {
	if err := safeName(name); err != nil {
		return Locked{}, err
	}

	mutating.Lock()
	defer mutating.Unlock()

	manifest, err := readManifest(i.staging(), name)
	if err != nil {
		return Locked{}, err
	}

	// The tree first, so that SKILL.md is not read through a link that was put
	// there after it was staged.
	stagedDir := filepath.Join(i.staging(), name)
	tree, _, err := TreeDigest(stagedDir)
	if err != nil {
		return Locked{}, because(ErrStale, err)
	}
	one, err := Read(stagedDir)
	if err != nil {
		return Locked{}, because(ErrStale, fmt.Errorf("skill: staged %q is not readable: %w", name, err))
	}
	if one.Name != name {
		return Locked{}, because(ErrStale, fmt.Errorf("skill: staged as %q but calls itself %q", name, one.Name))
	}
	if tree != manifest.TreeDigest {
		return Locked{}, because(ErrStale, fmt.Errorf(
			"skill: %q changed after it was staged; stage it again", name))
	}
	if expect != "" && expect != tree {
		return Locked{}, because(ErrStale, fmt.Errorf(
			"skill: %q is not the skill that was reviewed: it is %s now, and the one looked at was %s",
			name, tree, expect))
	}

	at := filepath.Join(i.Root, name)
	if _, err := os.Lstat(at); err == nil && !replace {
		return Locked{}, because(ErrCollision, fmt.Errorf(
			"skill: there is already something at %q in the skills directory, installed or placed by hand; "+
				"activate it again allowing a replacement if that is meant", name))
	} else if err != nil && !os.IsNotExist(err) {
		return Locked{}, fmt.Errorf("skill: %w", err)
	}

	// Read before anything moves, so a record that cannot be read is found out
	// while nothing has changed.
	lock, err := ReadLock(i.Root)
	if err != nil {
		return Locked{}, err
	}

	if err := i.moveInto(stagedDir, at); err != nil {
		return Locked{}, err
	}
	_ = os.Remove(manifestPath(i.staging(), name))
	// Only if empty: os.Remove refuses a directory that still holds other
	// staged skills, which is exactly the right condition.
	_ = os.Remove(i.staging())

	locked := Locked{
		Name:        one.Name,
		From:        manifest.Source,
		Digest:      one.Digest,
		TreeDigest:  tree,
		Version:     one.Version,
		InstalledAt: i.now(),
	}
	if err := WriteLock(i.Root, lock.Record(locked)); err != nil {
		return Locked{}, fmt.Errorf(
			"skill: %q is installed, but the record of it could not be written: %w", name, err)
	}
	return locked, nil
}

// Discard throws away a staged skill that nobody is going to activate.
func (i *Installer) Discard(name string) error {
	if err := safeName(name); err != nil {
		return err
	}

	mutating.Lock()
	defer mutating.Unlock()

	// Either half is enough to be there: a staged directory whose manifest was
	// never written is not listed as staged, and is exactly what somebody
	// asking to get rid of it is looking at.
	manifest, dir := manifestPath(i.staging(), name), filepath.Join(i.staging(), name)
	_, manifestErr := os.Lstat(manifest)
	_, dirErr := os.Lstat(dir)
	if os.IsNotExist(manifestErr) && os.IsNotExist(dirErr) {
		return because(ErrNotStaged, fmt.Errorf("skill: no skill named %q is staged", name))
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	if err := os.Remove(manifest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("skill: %w", err)
	}
	_ = os.Remove(i.staging())
	return nil
}

// Install fetches one skill and installs it in a single step.
//
// Stage then Activate, for the operator at the CLI who has the source in front
// of them and is deciding by looking at it rather than at an approval. It
// replaces an installed skill of the same name, which an operator reinstalling
// a skill means.
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
	if _, err := os.Lstat(to); err == nil {
		replaced = asideOf(to)
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

// asideOf is where a directory is put while its replacement is moved in.
//
// A dot name, beside the original: should the process die between the two
// renames, what is left must not be read by Installed as a second skill
// claiming the first one's name.
func asideOf(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".replacing")
}

// safeName refuses a skill name that could not be a directory beside the
// others: it becomes one, and it comes from frontmatter the installer did not
// write. A separator or a climb would put the skill outside the tree, and a
// leading dot would hide it among the installer's own working directories or
// collide with one.
func safeName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return because(ErrBadName, fmt.Errorf("skill: %q cannot name a skill", name))
	case strings.ContainsAny(name, `/\`):
		return because(ErrBadName, fmt.Errorf("skill: %q cannot name a skill: it has a path separator", name))
	case strings.HasPrefix(name, "."):
		return because(ErrBadName, fmt.Errorf("skill: %q cannot name a skill: it begins with a dot", name))
	case strings.HasSuffix(strings.ToLower(name), ".json"):
		// The installs are recorded in installed.json beside the skills, and a
		// staged skill's record is <name>.json beside its directory. A skill with
		// such a name would be installed over the one or stand where the other is,
		// and discarding it would delete a record that is not its own. Compared
		// without regard to case, because the directory may be on a file system
		// that does not tell INSTALLED.json from installed.json.
		return because(ErrBadName, fmt.Errorf(
			"skill: %q cannot name a skill: a name ending in .json is where the installer keeps its records", name))
	}
	return nil
}

// Remove deletes an installed skill, and the record of its having been
// installed if there is one.
//
// Only a directory: a file in the skills directory, installed.json above all,
// is not a skill however it is named, and a link is not followed, because
// Installed does not read one either.
func (i *Installer) Remove(name string) error {
	if err := safeName(name); err != nil {
		return err
	}

	mutating.Lock()
	defer mutating.Unlock()

	at := filepath.Join(i.Root, name)
	info, err := os.Lstat(at)
	if os.IsNotExist(err) {
		return because(ErrNotInstalled, fmt.Errorf("skill: there is no skill named %q installed", name))
	}
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	if !info.IsDir() {
		return because(ErrNotInstalled, fmt.Errorf("skill: %q is not a skill directory", name))
	}

	// Read before anything is deleted, like Activate: a record that cannot be
	// read is found out while there is still something to keep.
	lock, err := ReadLock(i.Root)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(at); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	if lock, forgotten := lock.Forget(name); forgotten {
		if err := WriteLock(i.Root, lock); err != nil {
			return fmt.Errorf("skill: %q is removed, but the record of it could not be updated: %w", name, err)
		}
	}
	return nil
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
	command.Env = gitEnvironment(in)

	// A git that has been killed can leave a helper holding its output open.
	// Without this the command is still waiting on that pipe after the process
	// it was waiting for is gone.
	command.WaitDelay = fetchWaitDelay

	// Both outputs into one writer that keeps only the end. A remote chooses what
	// it prints, for as long as the timeout lets it, and everything it printed
	// was held in memory to be cut afterwards.
	said := &tailWriter{limit: keptOfGit}
	command.Stderr = said
	command.Stdout = said

	if err := command.Run(); err != nil {
		reason := gitSaid(said.String())
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

// gitEnvironment is the whole environment a git command is given.
//
// Nothing inherited. The daemon's environment holds provider credentials, and
// a repository address somebody typed is not a reason to hand them over.
//
// ssh is told not to ask either: git's own prompts are off, but ssh opens the
// terminal itself for a host key or a passphrase, and there is nobody at one.
// It is told to give up on a server that does not answer, too, because killing
// git on the timeout does not kill the ssh it started.
//
// HOME does not keep ssh to itself. OpenSSH takes the home directory from the
// account's entry, not from the variable, so an ssh address is fetched with the
// keys, the configuration and the known hosts of whoever runs the daemon. That
// is what an ssh remote is for; it is also why the agent's own tool does not
// accept one.
func gitEnvironment(home string) []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=30",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
	}
}

// gitSaid is what git printed, fit to be put in an error: the end of it, which
// is where git's own message is after whatever the remote chose to say first,
// with any credential in an address taken out.
func gitSaid(output string) string {
	output = Redact(strings.TrimSpace(output))
	if len(output) <= maxGitSaid {
		return output
	}

	tail := output[len(output)-maxGitSaid:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return "... " + strings.TrimSpace(tail)
}

// tailWriter keeps the last limit bytes written to it, and nothing else.
//
// One value is given to both of a command's outputs; os/exec calls Write from
// one goroutine at a time when the two are the same writer.
type tailWriter struct {
	limit int
	kept  []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= w.limit {
		w.kept = append(w.kept[:0], p[len(p)-w.limit:]...)
		return len(p), nil
	}

	w.kept = append(w.kept, p...)
	if over := len(w.kept) - w.limit; over > 0 {
		w.kept = append(w.kept[:0], w.kept[over:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.kept) }

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

// manifestPath is where a staged skill's source is recorded, beside the staged
// directory rather than inside it so it is not part of what lands.
func manifestPath(staging, name string) string {
	return filepath.Join(staging, name+".json")
}

// writeManifest records a staged skill's source, whole and renamed into place:
// a reader during a replacement sees the old record or the new one, never half
// of either, and a crash leaves one of them.
func writeManifest(staging string, staged Staged) error {
	raw, err := json.MarshalIndent(staged, "", "  ")
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}

	// A dot name that does not end in .json, so StagedSkills does not take it
	// for a record while it is being written.
	temp, err := os.CreateTemp(staging, "."+staged.Name+".json-*")
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("skill: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	if err := os.Rename(temp.Name(), manifestPath(staging, staged.Name)); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return nil
}

// readManifest loads what Stage recorded, and says plainly when nothing is
// staged under that name.
func readManifest(staging, name string) (Staged, error) {
	raw, err := os.ReadFile(manifestPath(staging, name))
	if os.IsNotExist(err) {
		return Staged{}, because(ErrNotStaged, fmt.Errorf("skill: no skill named %q is staged", name))
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

// StagedSkill reads back one staged skill: what Stage recorded about where it
// came from, and the skill as it now sits on disk.
//
// For an approval a person is about to decide. The manifest carries the source
// and the digests; the skill carries the description and the instructions
// themselves, so the decision is made against the real bytes.
func StagedSkill(root, name string) (Staged, Skill, error) {
	if err := safeName(name); err != nil {
		return Staged{}, Skill{}, err
	}
	staging := filepath.Join(root, ".staged")
	manifest, err := readManifest(staging, name)
	if err != nil {
		return Staged{}, Skill{}, err
	}
	one, err := Read(filepath.Join(staging, name))
	if err != nil {
		return Staged{}, Skill{}, because(ErrStale, fmt.Errorf("skill: staged %q is not readable: %w", name, err))
	}
	return manifest, one, nil
}

// Review is a staged skill as the person deciding on it is shown it, all from
// one look.
type Review struct {
	Staged Staged

	// Files is every file with its hash, in the order of the tree digest.
	Files []File

	// TreeMatches is whether the staged directory hashes, now, to the digest that
	// was recorded when it was staged.
	TreeMatches bool

	// SkillMD is SKILL.md, the bytes its digest is of.
	SkillMD []byte
}

// ReviewStaged reads a staged skill whole, without anything about it changing
// while it is read.
//
// Staging a name again replaces its directory, and the listing, the digest and
// the instructions are separate reads of it. Read one after another with nothing
// between them, a replacement landing in the gap makes a reply of two skills:
// one's digest over the other's instructions, which a person then approves by
// the digest. The lock is the one every change to what is staged takes, so there
// is no gap in this process. Another process, such as jingclaw skills install,
// does not share it.
func ReviewStaged(root, name string) (Review, error) {
	if err := safeName(name); err != nil {
		return Review{}, err
	}

	mutating.Lock()
	defer mutating.Unlock()

	staged, err := readManifest(filepath.Join(root, ".staged"), name)
	if err != nil {
		return Review{}, err
	}

	// The listing goes first: its walk is what refuses a link, and a file that
	// has grown past the limits since, before anything is read whole.
	files, err := Files(staged.Dir)
	if err != nil {
		return Review{}, err
	}
	tree, _, err := TreeDigest(staged.Dir)
	if err != nil {
		return Review{}, err
	}
	if _, err := Read(staged.Dir); err != nil {
		return Review{}, because(ErrStale, fmt.Errorf("skill: staged %q is not readable: %w", name, err))
	}

	// Read with a limit, though the walk has bounded it: SKILL.md is returned
	// whole, and has to be small enough to be.
	file, err := os.Open(filepath.Join(staged.Dir, FileName))
	if err != nil {
		return Review{}, fmt.Errorf("skill: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxSkillMDBytes+1))
	if err != nil {
		return Review{}, fmt.Errorf("skill: %w", err)
	}
	if len(raw) > MaxSkillMDBytes {
		return Review{}, because(ErrStale, fmt.Errorf(
			"skill: staged %q has a %s of more than %d bytes now", name, FileName, MaxSkillMDBytes))
	}

	return Review{Staged: staged, Files: files, TreeMatches: tree == staged.TreeDigest, SkillMD: raw}, nil
}

// StagedSkills lists what has been fetched and is waiting to be activated.
//
// For an operator's window onto what the agent proposed but nobody has yet
// approved. Read from the manifests beside the staged directories, so a
// half-written fetch with no manifest is not counted as staged.
func StagedSkills(root string) ([]Staged, error) {
	staging := filepath.Join(root, ".staged")
	entries, err := os.ReadDir(staging)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill: read %s: %w", staging, err)
	}

	var staged []Staged
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || name == entry.Name() {
			continue
		}
		manifest, err := readManifest(staging, name)
		if err != nil {
			continue
		}
		staged = append(staged, manifest)
	}
	sort.Slice(staged, func(a, b int) bool { return staged[a].Name < staged[b].Name })
	return staged, nil
}
