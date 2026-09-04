package skill

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// aRepository builds a real git repository holding a skill, and returns its
// path and the commit.
//
// Real, because what is being checked is the fetching. A fake that returned
// files would test the parts around git and none of the part that talks to
// it — which is where the mistakes are.
func aRepository(t *testing.T, body string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Skipf("git is not usable here (%v): %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "--quiet", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(dir, "release"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release", FileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	git("add", "-A")
	git("commit", "--quiet", "-m", "a skill")

	return dir, git("rev-parse", "HEAD")
}

const aSkill = `---
name: release
description: How this repository is released.
version: 1.2.0
---

Tag the commit, then push the tag.
`

// installing is an installer over a fresh skills directory.
func installing(t *testing.T) *Installer {
	t.Helper()
	return &Installer{
		Root: filepath.Join(t.TempDir(), "skills"),
		Now:  func() time.Time { return time.Unix(1000, 0).UTC() },
	}
}

// fromLocal is the source form for a repository on this machine.
//
// ParseSource refuses these on purpose — a local path records a source
// nobody else can resolve — so a test that wants one builds it directly.
// What is under test here is the fetching, not the parsing.
func fromLocal(dir, commit, path string) Source {
	return Source{Repository: dir, Commit: commit, Path: path}
}

func TestWhatIsInstalledIsWhatWasAskedFor(t *testing.T) {
	repo, commit := aRepository(t, aSkill)
	installer := installing(t)

	locked, err := installer.Install(context.Background(), fromLocal(repo, commit, "release"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if locked.Name != "release" {
		t.Errorf("it was installed as %q", locked.Name)
	}
	if locked.Version != "1.2.0" {
		t.Errorf("version %q", locked.Version)
	}
	if !strings.HasPrefix(locked.Digest, "sha256:") {
		t.Errorf("no digest of what arrived: %q", locked.Digest)
	}

	// And it is readable as a skill from where it landed, by the same code
	// the catalogue uses.
	installed, rejected, err := Installed(installer.Root)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if len(rejected) != 0 {
		t.Errorf("what was installed will not load: %+v", rejected)
	}
	if len(installed) != 1 || installed[0].Name != "release" {
		t.Fatalf("the catalogue does not have it: %+v", installed)
	}
	if installed[0].Digest != locked.Digest {
		t.Error("what was recorded is not what is on disk")
	}
}

// TestTheRepositoryItselfDoesNotLand keeps a skills directory from becoming a
// collection of git checkouts.
func TestTheRepositoryItselfDoesNotLand(t *testing.T) {
	repo, commit := aRepository(t, aSkill)
	installer := installing(t)

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err != nil {
		t.Fatalf("install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(installer.Root, "release", ".git")); !os.IsNotExist(err) {
		t.Error("the git metadata was installed along with the skill")
	}

	// Nor is anything left of the fetch.
	entries, err := os.ReadDir(installer.Root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Errorf("something was left behind: %s", entry.Name())
		}
	}
}

// TestARepositoryWithNoSkillLeavesNothing is why the fetch is staged.
func TestARepositoryWithNoSkillLeavesNothing(t *testing.T) {
	repo, commit := aRepository(t, aSkill)
	installer := installing(t)

	// A path that exists in the repository and holds no SKILL.md.
	_, err := installer.Install(context.Background(), fromLocal(repo, commit, ""))
	if err == nil {
		t.Fatal("a repository root with no skill was installed")
	}

	if entries, _ := os.ReadDir(installer.Root); len(entries) != 0 {
		t.Errorf("a failed install left %d things behind", len(entries))
	}
}

// TestAFailedInstallLeavesTheWorkingOne is the property that matters when
// somebody updates a skill they depend on.
func TestAFailedInstallLeavesTheWorkingOne(t *testing.T) {
	repo, commit := aRepository(t, aSkill)
	installer := installing(t)

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err != nil {
		t.Fatalf("install: %v", err)
	}

	// A commit that is not there.
	const missing = "0000000000000000000000000000000000000000"
	if _, err := installer.Install(context.Background(), fromLocal(repo, missing, "release")); err == nil {
		t.Fatal("a commit that does not exist was installed")
	}

	installed, _, err := Installed(installer.Root)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if len(installed) != 1 || installed[0].Name != "release" {
		t.Errorf("the working skill did not survive a failed update: %+v", installed)
	}
	if !strings.Contains(installed[0].Body, "Tag the commit") {
		t.Error("what survived is not the skill that was working")
	}
}

// TestInstallingAgainReplacesIt covers the ordinary update.
func TestInstallingAgainReplacesIt(t *testing.T) {
	repo, first := aRepository(t, aSkill)
	installer := installing(t)

	before, err := installer.Install(context.Background(), fromLocal(repo, first, "release"))
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// A second commit, changing the skill.
	changed := strings.Replace(aSkill, "Tag the commit, then push the tag.",
		"Tag the commit, push the tag, then tell the channel.", 1)
	if err := os.WriteFile(filepath.Join(repo, "release", FileName), []byte(changed), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	commit := exec.Command("git", "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "--quiet", "-am", "changed")
	commit.Dir = repo
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	revision := exec.Command("git", "rev-parse", "HEAD")
	revision.Dir = repo
	out, err := revision.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	second := strings.TrimSpace(string(out))

	after, err := installer.Install(context.Background(), fromLocal(repo, second, "release"))
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}

	if after.Digest == before.Digest {
		t.Error("the update recorded the same digest as the version it replaced")
	}

	installed, _, err := Installed(installer.Root)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if len(installed) != 1 {
		t.Fatalf("updating made %d skills", len(installed))
	}
	if !strings.Contains(installed[0].Body, "tell the channel") {
		t.Error("the update did not take")
	}
}

func TestRemovingOne(t *testing.T) {
	repo, commit := aRepository(t, aSkill)
	installer := installing(t)

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := installer.Remove("release"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if installed, _, _ := Installed(installer.Root); len(installed) != 0 {
		t.Errorf("it survived: %+v", installed)
	}
	if err := installer.Remove("release"); err == nil {
		t.Error("removing one that is not there succeeded")
	}
	if err := installer.Remove("../../etc"); err == nil {
		t.Error("a name reaching outside the skills directory was accepted")
	}
}

// A skill's name is used as its directory, and the name comes from the
// repository's own frontmatter. A name that climbs out of the skills
// directory would install the skill somewhere nobody meant it to be — which
// matters more the moment an agent, rather than an operator with the source
// in front of them, can propose what gets installed.
func TestASkillCannotNameItselfOutOfTheDirectory(t *testing.T) {
	escaping := `---
name: ../escaped
description: Pretends to be ordinary.
---

Land me outside the skills directory.
`
	repo, commit := aRepository(t, escaping)
	installer := installing(t)

	_, err := installer.Install(context.Background(), fromLocal(repo, commit, "release"))
	if err == nil {
		t.Fatal("a skill named its way out of the directory and was installed")
	}

	// And nothing was written where the name pointed.
	outside := filepath.Join(filepath.Dir(installer.Root), "escaped")
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Errorf("a directory was created outside the skills root at %s", outside)
	}
}

// A separator in the name, without a leading dot, is its own way out of the
// directory — into a subdirectory of the skills root, or through it — and is
// refused by a different branch of the guard than the dot names are.
func TestASkillCannotNameItselfIntoASubdirectory(t *testing.T) {
	nested := `---
name: sub/evil
description: Pretends to be ordinary.
---

Land me one level down.
`
	repo, commit := aRepository(t, nested)
	installer := installing(t)

	// The subdirectory exists, so nothing but the guard stops the skill
	// landing in it: without the check, the rename into sub/ would succeed.
	if err := os.MkdirAll(filepath.Join(installer.Root, "sub"), 0o755); err != nil {
		t.Fatalf("staging: %v", err)
	}

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err == nil {
		t.Fatal("a skill named itself into a subdirectory and was installed")
	}
	if _, statErr := os.Stat(filepath.Join(installer.Root, "sub", "evil")); !os.IsNotExist(statErr) {
		t.Error("a skill landed in a subdirectory of the skills root")
	}
}

// The same guard, for a name that would collide with the installer's own
// staging or working directories rather than climb out of the tree.
func TestASkillCannotNameItselfADotDirectory(t *testing.T) {
	hidden := `---
name: .staged
description: Pretends to be ordinary.
---

Hide among the installer's own directories.
`
	repo, commit := aRepository(t, hidden)
	installer := installing(t)

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err == nil {
		t.Fatal("a skill named itself a dot-directory and was installed")
	}
}
