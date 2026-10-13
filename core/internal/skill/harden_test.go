package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitIn runs git in a directory, and skips the test where git cannot be used,
// as aRepository does.
func gitIn(t *testing.T, dir string, args ...string) string {
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

// commitAll commits whatever is in a repository and says what the commit is.
func commitAll(t *testing.T, dir, message string) string {
	t.Helper()

	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--quiet", "-m", message)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// skillCalled is a skill with a name and a note, so that two of them differ in
// more than a name.
func skillCalled(name, note string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: The %s skill.\n---\n\n%s\n", name, name, note)
}

// staging stages a skill from a fresh repository and says what it staged.
func staging(t *testing.T, installer *Installer, name, note string) (Staged, string, string) {
	t.Helper()

	repo, commit := aRepository(t, skillCalled(name, note))
	staged, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
	if err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
	return staged, repo, commit
}

// restagedAs stages a different commit of a repository under the name it
// already staged.
func restagedAs(t *testing.T, installer *Installer, repo, name, note string) Staged {
	t.Helper()

	if err := os.WriteFile(filepath.Join(repo, "release", FileName), []byte(skillCalled(name, note)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	commit := commitAll(t, repo, note)
	staged, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
	if err != nil {
		t.Fatalf("stage again: %v", err)
	}
	return staged
}

// Everything that changes what is staged, installed or recorded takes the one
// lock, and waits for it.
//
// Held here by the test, so that the check does not depend on two calls
// happening to overlap: each of these has everything it needs to finish, and
// the only thing in its way is the lock.
func TestEverythingThatChangesTheSkillsDirectoryWaitsForTheLock(t *testing.T) {
	installer := installing(t)

	toActivate, _, _ := staging(t, installer, "to-activate", "one")
	toDiscard, _, _ := staging(t, installer, "to-discard", "two")
	toRemove, _, _ := staging(t, installer, "to-remove", "three")
	if _, err := installer.Activate(toRemove.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}
	repo, commit := aRepository(t, skillCalled("to-stage", "four"))

	operations := map[string]func() error{
		"Activate": func() error {
			_, err := installer.ActivateExpecting(toActivate.Name, toActivate.TreeDigest, false)
			return err
		},
		"Discard": func() error { return installer.Discard(toDiscard.Name) },
		"Remove":  func() error { return installer.Remove(toRemove.Name) },
		// The fetch is not under the lock; the move into the staging area is.
		"Stage": func() error {
			_, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
			return err
		},
	}

	mutating.Lock()
	finished := make(chan string, len(operations))
	for name, operation := range operations {
		go func() {
			if err := operation(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			finished <- name
		}()
	}

	select {
	case name := <-finished:
		mutating.Unlock()
		t.Fatalf("%s went ahead while the lock was held", name)
	case <-time.After(300 * time.Millisecond):
	}
	mutating.Unlock()

	for range operations {
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Fatal("an operation never finished after the lock was released")
		}
	}
}

// The record is read, changed and written back inside the lock, and not read
// before it is taken: eight activations at once leave eight entries, and an
// install that read the record outside the lock would write back one that
// forgot the others.
func TestActivatingAtOnceLeavesEveryoneInTheRecord(t *testing.T) {
	installer := installing(t)

	const count = 8
	var staged []Staged
	for index := range count {
		one, _, _ := staging(t, installer, fmt.Sprintf("skill-%d", index), "note")
		staged = append(staged, one)
	}

	var wait sync.WaitGroup
	for _, one := range staged {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := installer.ActivateExpecting(one.Name, one.TreeDigest, false); err != nil {
				t.Errorf("activate %s: %v", one.Name, err)
			}
		}()
	}
	wait.Wait()

	lock, err := ReadLock(installer.Root)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if len(lock.Skills) != count {
		t.Errorf("the record has %d entries after %d activations", len(lock.Skills), count)
	}
	if found, _, _ := Installed(installer.Root); len(found) != count {
		t.Errorf("%d skills are installed after %d activations", len(found), count)
	}
}

// aRemoteThatNeverAnswers is a git:// address that accepts a connection and
// says nothing, so that a fetch from it is under way for as long as the test
// wants one to be. The channel receives when a fetch has connected.
func aRemoteThatNeverAnswers(t *testing.T) (string, <-chan struct{}) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback to listen on: %v", err)
	}

	var (
		held      sync.Mutex
		connected = make(chan struct{}, 8)
		open      []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			held.Lock()
			open = append(open, conn)
			held.Unlock()
			connected <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		held.Lock()
		defer held.Unlock()
		for _, conn := range open {
			_ = conn.Close()
		}
	})

	return "git://" + listener.Addr().String() + "/skills.git", connected
}

// A second Stage while one is fetching is refused at once, and not made to wait
// for a clone that can take minutes.
func TestASecondStageWhileOneIsFetchingIsRefused(t *testing.T) {
	address, connected := aRemoteThatNeverAnswers(t)
	installer := installing(t)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := installer.Stage(ctx, Source{Repository: address, Commit: strings.Repeat("a", 40)})
		first <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-first
	})

	select {
	case <-connected:
	case err := <-first:
		t.Fatalf("the first fetch ended before it could be raced: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the first fetch never connected")
	}

	repo, commit := aRepository(t, aSkill)
	began := time.Now()
	_, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a second stage while one fetches got %v, want ErrBusy", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the refusal took %s; it should not have waited for anything", took)
	}

	// And the gate is let go of when the fetch ends, whichever way.
	cancel()
	if err := <-first; err == nil {
		t.Error("a fetch from a remote that never answered succeeded")
	}
	first <- nil
	if _, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release")); err != nil {
		t.Errorf("a stage after the fetch ended: %v", err)
	}
}

// A staged skill can be activated by the digest its reviewer was shown, and by
// no other.
func TestActivationIsBoundToTheDigestThatWasShown(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "reviewed", "as reviewed")

	for _, wrong := range []string{"", "sha256:" + strings.Repeat("0", 64), staged.Digest} {
		_, err := installer.ActivateExpecting(staged.Name, wrong, false)
		if err == nil {
			t.Fatalf("activated with the digest %q", wrong)
		}
		if wrong != "" && !errors.Is(err, ErrStale) {
			t.Errorf("a digest that is not the staged tree's got %v, want ErrStale", err)
		}
	}
	if found, _, _ := Installed(installer.Root); len(found) != 0 {
		t.Fatalf("a refused activation installed %+v", found)
	}
	if _, err := os.Stat(filepath.Join(installer.staging(), "reviewed", FileName)); err != nil {
		t.Errorf("a refused activation took the staged skill away: %v", err)
	}

	locked, err := installer.ActivateExpecting(staged.Name, staged.TreeDigest, false)
	if err != nil {
		t.Fatalf("activate with the digest that was shown: %v", err)
	}
	if locked.TreeDigest != staged.TreeDigest {
		t.Errorf("installed %q, reviewed %q", locked.TreeDigest, staged.TreeDigest)
	}
}

// What is staged under a name can be staged again between the review and the
// click. The digest that was reviewed is of the first; the one on disk is the
// second.
func TestAnActivationRefusesWhatWasStagedAgainSinceTheReview(t *testing.T) {
	installer := installing(t)
	reviewed, repo, _ := staging(t, installer, "swapped", "what the reviewer read")
	restagedAs(t, installer, repo, "swapped", "something else entirely")

	_, err := installer.ActivateExpecting("swapped", reviewed.TreeDigest, false)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
	if found, _, _ := Installed(installer.Root); len(found) != 0 {
		t.Errorf("what was staged afterwards was installed under a decision about the first: %+v", found)
	}
}

// And the staged files can be edited in place, with the record untouched. The
// digest that was shown is also the record's, so only the tree on disk tells.
func TestAnActivationRefusesStagedFilesEditedAfterTheReview(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "edited", "what the reviewer read")

	if err := os.WriteFile(filepath.Join(staged.Dir, FileName),
		[]byte(skillCalled("edited", "swapped in afterwards")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.ActivateExpecting("edited", staged.TreeDigest, false); !errors.Is(err, ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
}

// Silently replacing a skill is how one skill becomes another. Installed or
// placed by hand, it is the person's say.
func TestAnActivationDoesNotReplaceASkillUnlessTold(t *testing.T) {
	t.Run("an installed one", func(t *testing.T) {
		installer := installing(t)
		first, repo, _ := staging(t, installer, "replaced", "the first")
		if _, err := installer.ActivateExpecting(first.Name, first.TreeDigest, false); err != nil {
			t.Fatalf("activate: %v", err)
		}
		second := restagedAs(t, installer, repo, "replaced", "the second")

		_, err := installer.ActivateExpecting("replaced", second.TreeDigest, false)
		if !errors.Is(err, ErrCollision) {
			t.Fatalf("got %v, want ErrCollision", err)
		}
		found, _, _ := Installed(installer.Root)
		if len(found) != 1 || !strings.Contains(found[0].Body, "the first") {
			t.Errorf("a refused replacement changed what is installed: %+v", found)
		}
		if _, err := installer.Activate("replaced"); err != nil {
			// Staged still: a refusal is not a discard.
			t.Fatalf("the refused skill was not left staged: %v", err)
		}
	})

	t.Run("one placed by hand", func(t *testing.T) {
		installer := installing(t)
		install(t, installer.Root, "mine", skillCalled("mine", "written here"))
		staged, _, _ := staging(t, installer, "mine", "from a repository")

		if _, err := installer.ActivateExpecting("mine", staged.TreeDigest, false); !errors.Is(err, ErrCollision) {
			t.Fatalf("got %v, want ErrCollision", err)
		}
		found, _, _ := Installed(installer.Root)
		if len(found) != 1 || !strings.Contains(found[0].Body, "written here") {
			t.Errorf("a skill placed by hand was replaced: %+v", found)
		}
		if lock, _ := ReadLock(installer.Root); len(lock.Skills) != 0 {
			t.Errorf("a refused activation was recorded: %+v", lock)
		}

		locked, err := installer.ActivateExpecting("mine", staged.TreeDigest, true)
		if err != nil {
			t.Fatalf("replacing when told it may: %v", err)
		}
		found, _, _ = Installed(installer.Root)
		if len(found) != 1 || !strings.Contains(found[0].Body, "from a repository") || locked.Name != "mine" {
			t.Errorf("the replacement did not take: %+v", found)
		}
	})
}

// Whoever activates a skill, the record says where it came from. It used to be
// written by the command line alone, so a skill an agent installed looked like
// one somebody had put there by hand.
func TestAnActivationRecordsWhereTheSkillCameFrom(t *testing.T) {
	for name, activate := range map[string]func(*Installer, Staged) error{
		"by name, as the agent's tool does": func(installer *Installer, staged Staged) error {
			_, err := installer.Activate(staged.Name)
			return err
		},
		"by digest, as the service does": func(installer *Installer, staged Staged) error {
			_, err := installer.ActivateExpecting(staged.Name, staged.TreeDigest, false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			installer := installing(t)
			staged, repo, commit := staging(t, installer, "recorded", "note")

			if err := activate(installer, staged); err != nil {
				t.Fatalf("activate: %v", err)
			}

			lock, err := ReadLock(installer.Root)
			if err != nil {
				t.Fatalf("read the record: %v", err)
			}
			entry, found := lock.Entry("recorded")
			if !found {
				t.Fatalf("a skill that was activated is not in the record: %+v", lock)
			}
			if entry.From != (Source{Repository: repo, Commit: commit, Path: "release"}) {
				t.Errorf("the record says it came from %+v", entry.From)
			}
			if entry.TreeDigest != staged.TreeDigest || entry.Digest != staged.Digest {
				t.Errorf("the record's digests are %q and %q", entry.Digest, entry.TreeDigest)
			}
			if !entry.InstalledAt.Equal(time.Unix(1000, 0).UTC()) {
				t.Errorf("installed at %v", entry.InstalledAt)
			}
		})
	}
}

// A record that cannot be read is found out before anything is moved.
func TestAnUnreadableRecordStopsAnActivationBeforeItChangesAnything(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "unrecorded", "note")
	if err := os.WriteFile(filepath.Join(installer.Root, LockName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := installer.ActivateExpecting(staged.Name, staged.TreeDigest, false); err == nil {
		t.Fatal("activated into a record that cannot be read")
	}
	if found, _, _ := Installed(installer.Root); len(found) != 0 {
		t.Errorf("a skill was installed with nothing to record it in: %+v", found)
	}
	if _, err := os.Stat(filepath.Join(installer.staging(), "unrecorded", FileName)); err != nil {
		t.Errorf("the staged skill was taken: %v", err)
	}
}

// Removing is for skills. Whatever else is in the skills directory is not one,
// and the record of installs above all is not.
func TestRemovingRefusesWhatIsNotASkill(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "kept", "note")
	if _, err := installer.Activate(staged.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}
	staged, _, _ = staging(t, installer, "also-staged", "note")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installer.Root, "notes.txt"), []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(installer.Root, "linked")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}

	recorded, err := os.ReadFile(filepath.Join(installer.Root, LockName))
	if err != nil {
		t.Fatalf("the record: %v", err)
	}

	for _, name := range []string{LockName, "notes.txt", "linked", ".staged", ".fetching-1", "..", ".", "", "a/b", "../x"} {
		if err := installer.Remove(name); err == nil {
			t.Errorf("Remove(%q) succeeded", name)
		}
	}

	if now, _ := os.ReadFile(filepath.Join(installer.Root, LockName)); string(now) != string(recorded) {
		t.Errorf("the record of installs was changed or removed")
	}
	for _, left := range []string{"notes.txt", "linked", filepath.Join(".staged", staged.Name, FileName),
		filepath.Join("kept", FileName)} {
		if _, err := os.Lstat(filepath.Join(installer.Root, left)); err != nil {
			t.Errorf("%s did not survive the refusals: %v", left, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Errorf("what a link pointed at was removed: %v", err)
	}
}

// Nothing can be installed over the record of installs by being named after it.
func TestASkillCannotBeCalledTheRecordOfInstalls(t *testing.T) {
	repo, commit := aRepository(t, skillCalled(LockName, "I am the record now"))
	installer := installing(t)

	if _, err := installer.Install(context.Background(), fromLocal(repo, commit, "release")); err == nil {
		t.Fatal("a skill called installed.json was installed")
	}
	if entries, _ := os.ReadDir(installer.Root); len(entries) != 0 {
		t.Errorf("a refused install left %d things behind", len(entries))
	}
}

// Removing a skill forgets that it was installed, in the same step; and a skill
// placed by hand has nothing to forget.
func TestRemovingASkillForgetsItsRecord(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "recorded", "note")
	if _, err := installer.Activate(staged.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}
	other, _, _ := staging(t, installer, "other", "note")
	if _, err := installer.Activate(other.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}
	install(t, installer.Root, "mine", skillCalled("mine", "by hand"))

	if err := installer.Remove("recorded"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	lock, err := ReadLock(installer.Root)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if _, found := lock.Entry("recorded"); found {
		t.Error("the record still says a removed skill is installed")
	}
	if _, found := lock.Entry("other"); !found {
		t.Error("removing one skill forgot another")
	}

	if err := installer.Remove("mine"); err != nil {
		t.Fatalf("remove one placed by hand: %v", err)
	}
	if lock, _ := ReadLock(installer.Root); len(lock.Skills) != 1 {
		t.Errorf("removing one placed by hand changed the record: %+v", lock)
	}
}

// A refusal to remove says which kind it is, for a service to answer with.
func TestRemovingWhatIsNotThereOrNotASkillSaysSo(t *testing.T) {
	installer := installing(t)
	if err := os.MkdirAll(installer.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installer.Root, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]error{
		"absent":         ErrNotInstalled,
		"notes.txt":      ErrNotInstalled,
		LockName:         ErrBadName,
		".staged":        ErrBadName,
		"a/b":            ErrBadName,
		"../../etc/pass": ErrBadName,
	} {
		if err := installer.Remove(name); !errors.Is(err, want) {
			t.Errorf("Remove(%q) = %v, want %v", name, err, want)
		}
	}
}

// A staged skill can be thrown away without installing it, and nothing else is
// touched.
func TestDiscardingAStagedSkill(t *testing.T) {
	installer := installing(t)
	kept, _, _ := staging(t, installer, "kept", "note")
	if _, err := installer.Activate(kept.Name); err != nil {
		t.Fatalf("activate: %v", err)
	}
	staged, _, _ := staging(t, installer, "thrown-away", "note")
	other, _, _ := staging(t, installer, "still-waiting", "note")

	if err := installer.Discard(staged.Name); err != nil {
		t.Fatalf("discard: %v", err)
	}

	waiting, err := StagedSkills(installer.Root)
	if err != nil || len(waiting) != 1 || waiting[0].Name != other.Name {
		t.Errorf("staged afterwards: %+v (%v), want only %s", waiting, err, other.Name)
	}
	if _, err := os.Lstat(staged.Dir); !os.IsNotExist(err) {
		t.Errorf("the staged directory is still there: %v", err)
	}
	if found, _, _ := Installed(installer.Root); len(found) != 1 || found[0].Name != "kept" {
		t.Errorf("discarding changed what is installed: %+v", found)
	}
	if _, err := installer.Activate(staged.Name); !errors.Is(err, ErrNotStaged) {
		t.Errorf("a discarded skill could be activated: %v", err)
	}

	if err := installer.Discard(staged.Name); !errors.Is(err, ErrNotStaged) {
		t.Errorf("discarding it twice got %v, want ErrNotStaged", err)
	}
	if err := installer.Discard("../kept"); !errors.Is(err, ErrBadName) {
		t.Errorf("discarding a name that leaves the staging area got %v", err)
	}

	// The last one takes the staging area with it, like activating does.
	if err := installer.Discard(other.Name); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Lstat(installer.staging()); !os.IsNotExist(err) {
		t.Errorf("an empty staging area was left behind: %v", err)
	}
}

// A staged directory that never got its record is not listed as staged, and
// is still something to be able to get rid of.
func TestDiscardingWhatNeverGotItsRecord(t *testing.T) {
	installer := installing(t)
	half := filepath.Join(installer.staging(), "half")
	if err := os.MkdirAll(half, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(half, FileName), []byte(skillCalled("half", "x")), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := installer.Discard("half"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Lstat(half); !os.IsNotExist(err) {
		t.Errorf("it is still there: %v", err)
	}
}

// The record of a staged skill is replaced whole. A reader at any moment sees
// the old one or the new one.
//
// A reader that never stops, against a writer that does it many times over a
// record big enough to take more than one write to put down: a file written in
// place is seen empty or cut off by one of them.
func TestAStagedSkillsRecordIsNeverSeenHalfWritten(t *testing.T) {
	staging := t.TempDir()
	one := Staged{Name: "big", Description: strings.Repeat("d", 1<<18), TreeDigest: "sha256:x"}
	if err := writeManifest(staging, one); err != nil {
		t.Fatalf("write: %v", err)
	}

	stop := make(chan struct{})
	torn := make(chan error, 1)
	go func() {
		defer close(torn)
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(manifestPath(staging, "big"))
			var read Staged
			if err == nil {
				err = json.Unmarshal(raw, &read)
			}
			if err != nil {
				torn <- err
				return
			}
		}
	}()

	for range 100 {
		if err := writeManifest(staging, one); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	close(stop)

	if err := <-torn; err != nil {
		t.Errorf("a reader saw the record half-written: %v", err)
	}
	entries, _ := os.ReadDir(staging)
	if len(entries) != 1 {
		t.Errorf("%d things are in the staging area, want only the record", len(entries))
	}
}

// The copy of a skill that is put aside while its replacement is moved in is
// named so that Installed cannot read it as a second skill. It has the first
// one's name in its SKILL.md.
func TestTheCopyPutAsideDuringAReplacementIsNotASkill(t *testing.T) {
	root := t.TempDir()
	aside := asideOf(filepath.Join(root, "release"))

	if base := filepath.Base(aside); !strings.HasPrefix(base, ".") {
		t.Fatalf("the copy put aside is called %q, which Installed reads", base)
	}
	if filepath.Dir(aside) != root {
		t.Errorf("the copy put aside is in %s, not beside the original", filepath.Dir(aside))
	}

	install(t, root, "release", aSkill)
	install(t, root, filepath.Base(aside), aSkill)
	found, rejected, err := Installed(root)
	if err != nil || len(found) != 1 || len(rejected) != 0 {
		t.Errorf("a leftover aside copy made %d skills and %d rejections (%v)", len(found), len(rejected), err)
	}
}

// A symbolic link in a fetched tree is the repository choosing what a read of
// it lands on. Refused wherever it is, and nothing is left behind.
func TestAFetchedLinkIsRefused(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(secret, []byte("---\nname: leaked\ndescription: from outside the repository\n---\nnot yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, arrange := range map[string]func(t *testing.T, repo string){
		"a link beside SKILL.md": func(t *testing.T, repo string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(repo, "release", "passwd")); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
		"a link inside the tree to something inside the tree": func(t *testing.T, repo string) {
			if err := os.Symlink(FileName, filepath.Join(repo, "release", "alias.md")); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
		"SKILL.md itself": func(t *testing.T, repo string) {
			path := filepath.Join(repo, "release", FileName)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, path); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
		"a link deep in a directory": func(t *testing.T, repo string) {
			deep := filepath.Join(repo, "release", "a", "b")
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../..", filepath.Join(deep, "up")); err != nil {
				t.Skipf("no symbolic links here: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo, _ := aRepository(t, aSkill)
			arrange(t, repo)
			commit := commitAll(t, repo, "with a link")
			installer := installing(t)

			_, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
			if err == nil {
				t.Fatal("a tree with a link in it was staged")
			}
			if !errors.Is(err, ErrSource) {
				t.Errorf("got %v, want ErrSource", err)
			}
			if !strings.Contains(err.Error(), "link") {
				t.Errorf("the refusal does not say why: %v", err)
			}
			if strings.Contains(err.Error(), "not yours") || strings.Contains(err.Error(), "leaked") {
				t.Errorf("what a link pointed at was read: %v", err)
			}
			if entries, _ := os.ReadDir(installer.Root); len(entries) != 0 {
				t.Errorf("a refused stage left %d things behind", len(entries))
			}
		})
	}
}

// The skill's own directory may not be a link either: it is moved as the link,
// and the directory it points at is deleted with the rest of the fetch.
func TestAFetchedSkillDirectoryThatIsALinkIsRefused(t *testing.T) {
	repo, _ := aRepository(t, aSkill)
	if err := os.Symlink("release", filepath.Join(repo, "alias")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	commit := commitAll(t, repo, "alias")
	installer := installing(t)

	if _, err := installer.Stage(context.Background(), fromLocal(repo, commit, "alias")); err == nil {
		t.Fatal("a skill directory that is a link was staged")
	}
}

// Whatever git is run with is exactly this. Nothing inherited, no prompt of any
// kind: not git's, not ssh's.
func TestGitIsGivenExactlyThisEnvironment(t *testing.T) {
	t.Setenv("PATH", "/a/bin:/b/bin")

	want := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=30",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"PATH=/a/bin:/b/bin",
		"HOME=/the/fetch",
	}
	got := gitEnvironment("/the/fetch")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("git is run with\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// aGitThatSays puts a program called git first on the path, for a test of what
// run does with a git that misbehaves.
func aGitThatSays(t *testing.T, script string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for git is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// What run hands to git is gitEnvironment and nothing of the daemon's own.
func TestTheDaemonsEnvironmentDoesNotReachGit(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "a-provider-key")
	aGitThatSays(t, `for name in GIT_TERMINAL_PROMPT GIT_ASKPASS GIT_SSH_COMMAND GIT_CONFIG_GLOBAL GIT_CONFIG_SYSTEM HOME GEMINI_API_KEY; do
	eval "value=\${$name-unset}"
	echo "$name=$value" >&2
done
exit 1
`)

	dir := t.TempDir()
	err := run(context.Background(), dir, []string{"fetch"})
	if err == nil {
		t.Fatal("the stand-in for git exits 1")
	}
	for _, line := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=30",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME=" + dir,
		"GEMINI_API_KEY=unset",
	} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("git was run without %q:\n%v", line, err)
		}
	}
}

// A git that is killed on a timeout can leave a helper holding its output
// open. The command gives up waiting for that, after a grace, and does not
// wait for the helper to finish.
func TestAGitThatLeavesAHelperBehindIsNotWaitedFor(t *testing.T) {
	aGitThatSays(t, "sleep 5 &\nsleep 5\n")

	was := fetchWaitDelay
	fetchWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { fetchWaitDelay = was })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	began := time.Now()
	err := run(ctx, t.TempDir(), []string{"fetch"})
	if err == nil {
		t.Fatal("a git that was killed reported success")
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("run waited %s for a helper that outlived git", took)
	}
}

// What git says is passed on short, from the end, and with no credential in it.
func TestWhatGitSaysIsShortenedAndRedacted(t *testing.T) {
	t.Run("a short message is kept whole", func(t *testing.T) {
		if got := gitSaid("  fatal: repository not found\n"); got != "fatal: repository not found" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a long one keeps the end, where git's own message is", func(t *testing.T) {
		long := strings.Repeat("remote: chosen by the remote\n", 500) + "fatal: the part that matters"
		got := gitSaid(long)
		if len(got) > maxGitSaid+8 {
			t.Errorf("%d bytes went into an error", len(got))
		}
		if !strings.HasSuffix(got, "fatal: the part that matters") {
			t.Errorf("the end was lost: %q", got[max(0, len(got)-60):])
		}
	})

	t.Run("a cut through a character does not leave half of one", func(t *testing.T) {
		long := strings.Repeat("經", maxGitSaid)
		got := gitSaid(long)
		if !strings.HasSuffix(got, "經") || strings.ContainsRune(got, '\ufffd') {
			t.Errorf("the cut left a broken character: %q", got[:min(20, len(got))])
		}
		for _, r := range strings.TrimPrefix(got, "... ") {
			if r != '經' {
				t.Fatalf("a rune that is not 經: %q", r)
			}
		}
	})

	t.Run("an address git quoted is redacted", func(t *testing.T) {
		got := gitSaid("fatal: unable to access 'https://user:hunter2@example.com/x.git/': 'https://tok@example.com/y' too")
		if strings.Contains(got, "hunter2") || strings.Contains(got, "tok@") {
			t.Errorf("a credential survived: %q", got)
		}
		if !strings.Contains(got, "example.com/x.git") {
			t.Errorf("the address was lost with the credential: %q", got)
		}
	})
}

// What a remote prints is the remote's to choose, and an error carries only the
// end of it. So only the end of it is kept: a remote that streams without stopping
// is a command that waits for the timeout, and what it printed in that time is
// not held in memory to be cut afterwards.
func TestWhatGitPrintsIsKeptFromTheEndAndNeverInFull(t *testing.T) {
	kept := &tailWriter{limit: keptOfGit}

	chunk := []byte(strings.Repeat("remote: chosen by the remote\n", 1000))
	for range 400 {
		if n, err := kept.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("a write of %d bytes returned %d, %v", len(chunk), n, err)
		}
	}
	if _, err := kept.Write([]byte("fatal: the part that matters")); err != nil {
		t.Fatal(err)
	}

	if got := len(kept.String()); got != keptOfGit {
		t.Errorf("%d bytes are kept, want the last %d", got, keptOfGit)
	}
	if capacity := cap(kept.kept); capacity > 1<<20 {
		t.Errorf("%d bytes are held for a limit of %d", capacity, keptOfGit)
	}
	if !strings.HasSuffix(kept.String(), "fatal: the part that matters") {
		t.Errorf("the end was lost: %q", kept.String()[len(kept.String())-60:])
	}

	// And what is less than the limit is kept whole, across writes.
	short := &tailWriter{limit: keptOfGit}
	_, _ = short.Write([]byte("fatal: "))
	_, _ = short.Write([]byte("repository not found\n"))
	if short.String() != "fatal: repository not found\n" {
		t.Errorf("a short message became %q", short.String())
	}
}

// The same through the command: a git that prints without limit on both of its
// outputs fails with the end of what it said, and short.
func TestAGitThatPrintsWithoutLimitFailsWithTheEndOfWhatItSaid(t *testing.T) {
	aGitThatSays(t, `i=0
while [ $i -lt 4000 ]; do
	echo "remote: line $i of what the remote chose to say about itself, at length"
	echo "remote: and on the other output, line $i, also at length" >&2
	i=$((i + 1))
done
echo "fatal: the end of it" >&2
exit 1
`)

	err := run(context.Background(), t.TempDir(), []string{"fetch"})
	if err == nil {
		t.Fatal("the stand-in for git exits 1")
	}
	if len(err.Error()) > maxGitSaid+8 {
		t.Errorf("%d bytes went into an error", len(err.Error()))
	}
	if !strings.HasSuffix(err.Error(), "fatal: the end of it") {
		t.Errorf("the end was lost: %q", err.Error()[max(0, len(err.Error())-80):])
	}
}

// A staged skill's record is kept beside its directory as <name>.json, and the
// skills directory keeps installed.json. A skill with such a name would be a
// directory where another skill's record is, and a request to discard it would
// delete that record.
func TestASkillCannotBeCalledAfterARecordTheInstallerKeeps(t *testing.T) {
	installer := installing(t)
	alpha, _, _ := staging(t, installer, "alpha", "the first")

	for _, name := range []string{"alpha.json", "ALPHA.JSON", LockName, "INSTALLED.json", "x.Json"} {
		repo, commit := aRepository(t, skillCalled(name, "I am a record now"))
		if _, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release")); !errors.Is(err, ErrBadName) || !errors.Is(err, ErrSource) {
			t.Errorf("a skill called %q was staged: %v", name, err)
		}

		// And none of the calls that take a name will take it for a record.
		if err := installer.Discard(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Discard(%q) = %v, want ErrBadName", name, err)
		}
		if _, err := installer.Activate(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Activate(%q) = %v, want ErrBadName", name, err)
		}
		if err := installer.Remove(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Remove(%q) = %v, want ErrBadName", name, err)
		}
		if _, _, err := StagedSkill(installer.Root, name); !errors.Is(err, ErrBadName) {
			t.Errorf("StagedSkill(%q) = %v, want ErrBadName", name, err)
		}
	}

	waiting, err := StagedSkills(installer.Root)
	if err != nil || len(waiting) != 1 || waiting[0].Name != "alpha" {
		t.Fatalf("staged afterwards: %+v (%v), want only alpha", waiting, err)
	}
	if _, err := installer.ActivateExpecting("alpha", alpha.TreeDigest, false); err != nil {
		t.Errorf("alpha, whose record was in the way, cannot be activated: %v", err)
	}
}

// What a hostile repository can make a stage read is bounded before any of it is
// read: one file, the whole of it, how many, and the instructions themselves.
func TestAStageRefusesWhatIsTooBigToBeASkillAndLeavesNothing(t *testing.T) {
	big := func(t *testing.T, repo, name string, size int64) {
		t.Helper()
		file, err := os.Create(filepath.Join(repo, "release", name))
		if err != nil {
			t.Fatal(err)
		}
		// Sparse: the size is what is asked about, and nothing is written.
		if err := file.Truncate(size); err != nil {
			_ = file.Close()
			t.Skipf("no sparse files here: %v", err)
		}
		_ = file.Close()
	}

	for name, arrange := range map[string]func(t *testing.T, repo string){
		"a file bigger than any a skill needs": func(t *testing.T, repo string) { big(t, repo, "data.bin", MaxFileBytes+1) },
		"files that together are too many bytes": func(t *testing.T, repo string) {
			for index := range MaxTreeBytes/MaxFileBytes + 1 {
				big(t, repo, fmt.Sprintf("data-%d.bin", index), MaxFileBytes)
			}
		},
		"too many files": func(t *testing.T, repo string) {
			for index := range MaxFiles + 1 {
				put(t, repo, fmt.Sprintf("release/many/f%d", index), "x")
			}
		},
		"instructions bigger than a reply carries": func(t *testing.T, repo string) {
			put(t, repo, "release/"+FileName, skillCalled("release", strings.Repeat("a line of instructions\n", MaxSkillMDBytes/20)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo, _ := aRepository(t, aSkill)
			arrange(t, repo)
			commit := commitAll(t, repo, "too much")

			installer := installing(t)
			_, err := installer.Stage(context.Background(), fromLocal(repo, commit, "release"))
			if !errors.Is(err, ErrSource) {
				t.Fatalf("got %v, want a refusal of the source", err)
			}
			if entries, _ := os.ReadDir(installer.Root); len(entries) != 0 {
				t.Errorf("a refused stage left %d things behind", len(entries))
			}
		})
	}
}

// Staging a skill that fits shows what the limits are.
func TestAStageAcceptsAFileAndInstructionsAtExactlyTheLimits(t *testing.T) {
	repo, _ := aRepository(t, aSkill)

	file, err := os.Create(filepath.Join(repo, "release", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxFileBytes); err != nil {
		_ = file.Close()
		t.Skipf("no sparse files here: %v", err)
	}
	_ = file.Close()

	padding := MaxSkillMDBytes - len(skillCalled("release", ""))
	put(t, repo, "release/"+FileName, skillCalled("release", strings.Repeat("x", padding)))
	if info, err := os.Stat(filepath.Join(repo, "release", FileName)); err != nil || info.Size() != MaxSkillMDBytes {
		t.Fatalf("the instructions are %v bytes (%v), want exactly %d", info, err, MaxSkillMDBytes)
	}
	commit := commitAll(t, repo, "at the limits")

	if _, err := installing(t).Stage(context.Background(), fromLocal(repo, commit, "release")); err != nil {
		t.Errorf("a skill at the limits was refused: %v", err)
	}
}

// A review is one look at a staged skill. Re-staging the same name replaces the
// directory, and a review that read the listing, the digest and the instructions
// one after the other could show one version's instructions under another's
// digest, which a person would then approve by the digest.
func TestReviewingAStagedSkillIsOneLookAndWaitsForTheLock(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "reviewed", "as staged")

	mutating.Lock()
	done := make(chan Review, 1)
	go func() {
		review, err := ReviewStaged(installer.Root, staged.Name)
		if err != nil {
			t.Errorf("review: %v", err)
		}
		done <- review
	}()

	select {
	case <-done:
		mutating.Unlock()
		t.Fatal("a review went ahead while the lock was held")
	case <-time.After(300 * time.Millisecond):
	}
	mutating.Unlock()

	var review Review
	select {
	case review = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the review never finished after the lock was released")
	}

	if review.Staged.Name != "reviewed" || !review.TreeMatches {
		t.Errorf("the review: %+v", review.Staged)
	}
	raw, _ := os.ReadFile(filepath.Join(staged.Dir, FileName))
	if string(review.SkillMD) != string(raw) || len(review.Files) != 1 || review.Files[0].Path != FileName {
		t.Errorf("the review holds %q and %+v", review.SkillMD, review.Files)
	}
	if want := "sha256:" + review.Files[0].SHA256; want != staged.Digest {
		t.Errorf("the listed hash of the instructions is %s, the digest is %s", want, staged.Digest)
	}
}

func TestAReviewSaysWhenWhatIsStagedIsNotWhatWasRecorded(t *testing.T) {
	installer := installing(t)
	staged, _, _ := staging(t, installer, "edited", "as staged")
	put(t, staged.Dir, "extra.txt", "added since")

	review, err := ReviewStaged(installer.Root, staged.Name)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if review.TreeMatches || len(review.Files) != 2 {
		t.Errorf("a staged tree with a file added matches its record: %+v", review)
	}

	if _, err := ReviewStaged(installer.Root, "nothing-here"); !errors.Is(err, ErrNotStaged) {
		t.Errorf("a review of nothing: %v", err)
	}
	if _, err := ReviewStaged(installer.Root, "../escape"); !errors.Is(err, ErrBadName) {
		t.Errorf("a review of a name that leaves the staging area: %v", err)
	}
}

// A skill is plain files, and a file whose name is not text cannot be shown in
// a reply that is made of text: the skill would be listed and could be activated
// without being reviewed.
func TestAStageOfAFileNamedWithBytesThatAreNotTextIsRefused(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, FileName, aSkill)
	if err := os.WriteFile(filepath.Join(dir, "bad\xff.txt"), []byte("x"), 0o644); err != nil {
		t.Skipf("this file system will not hold a name that is not UTF-8: %v", err)
	}

	if _, _, err := TreeDigest(dir); !errors.Is(err, ErrSource) {
		t.Errorf("a digest of a tree with such a name: %v", err)
	}
	if _, err := Files(dir); !errors.Is(err, ErrSource) {
		t.Errorf("a listing of a tree with such a name: %v", err)
	}
}

// Whether activating would put a skill over another is read off where the staged
// skill is, as the approval of the agent's tool reads it, so it has to follow the
// layout of the skills directory and not a field somebody remembered to set.
func TestAStagedSkillSaysWhetherActivatingItWouldReplaceSomething(t *testing.T) {
	installer := installing(t)
	staged, repo, _ := staging(t, installer, "alpha", "first")

	read, _, err := StagedSkill(installer.Root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if read.Replaces() || staged.Replaces() {
		t.Error("nothing is installed, and a staged skill says it would replace something")
	}

	if _, err := installer.Activate("alpha"); err != nil {
		t.Fatal(err)
	}
	restagedAs(t, installer, repo, "alpha", "second")
	read, _, err = StagedSkill(installer.Root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !read.Replaces() {
		t.Error("an installed skill has the name, and the staged one does not say it would replace it")
	}
	if (Staged{Name: "alpha"}).Replaces() {
		t.Error("a skill that is not anywhere says it would replace something")
	}
}
