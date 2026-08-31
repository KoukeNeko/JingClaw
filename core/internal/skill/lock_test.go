package skill

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func aLocked(name, digest string) Locked {
	return Locked{
		Name:        name,
		From:        Source{Repository: "https://example.com/x", Commit: "a1b2", Path: name},
		Digest:      digest,
		InstalledAt: time.Unix(1000, 0).UTC(),
	}
}

func TestTheRecordSurvivesBeingWritten(t *testing.T) {
	root := t.TempDir()

	// Nothing installed is not an error: skills put there by hand are the
	// ordinary case, and a deployment with no record installed nothing.
	empty, err := ReadLock(root)
	if err != nil {
		t.Fatalf("reading nothing: %v", err)
	}
	if len(empty.Skills) != 0 {
		t.Errorf("a deployment that installed nothing has %d entries", len(empty.Skills))
	}

	lock := Lock{}.Record(aLocked("release", "sha256:aaa")).Record(aLocked("triage", "sha256:bbb"))
	if err := WriteLock(root, lock); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadLock(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Skills) != 2 {
		t.Fatalf("got %d entries", len(got.Skills))
	}

	// Sorted, so two installs in a different order do not produce a file that
	// differs only in arrangement.
	if got.Skills[0].Name != "release" || got.Skills[1].Name != "triage" {
		t.Errorf("out of order: %+v", got.Skills)
	}

	one, found := got.Entry("release")
	if !found {
		t.Fatal("release is not in its own record")
	}
	if one.Digest != "sha256:aaa" || one.From.Repository != "https://example.com/x" {
		t.Errorf("what came back is not what went in: %+v", one)
	}
}

func TestRecordingTheSameSkillTwiceReplacesIt(t *testing.T) {
	lock := Lock{}.Record(aLocked("release", "sha256:old")).Record(aLocked("release", "sha256:new"))

	if len(lock.Skills) != 1 {
		t.Fatalf("updating made %d entries", len(lock.Skills))
	}
	if lock.Skills[0].Digest != "sha256:new" {
		t.Errorf("the update did not take: %+v", lock.Skills[0])
	}

	lock, forgotten := lock.Forget("release")
	if !forgotten || len(lock.Skills) != 0 {
		t.Errorf("forgetting it left %d entries", len(lock.Skills))
	}
	if _, forgotten = lock.Forget("release"); forgotten {
		t.Error("forgetting it twice claimed to have found something")
	}
}

// TestASkillEditedSinceItArrivedCanBeFound is what the digest is for.
//
// Editing an installed skill is not wrong by itself — somebody may have meant
// to — but it is something they should be able to find out without diffing
// anything, because what is on disk is what goes in front of the model.
func TestASkillEditedSinceItArrivedCanBeFound(t *testing.T) {
	lock := Lock{}.
		Record(aLocked("release", "sha256:aaa")).
		Record(aLocked("triage", "sha256:bbb")).
		Record(aLocked("gone", "sha256:ccc"))

	changed := Changed("", lock, []Skill{
		{Name: "release", Digest: "sha256:aaa"},   // untouched
		{Name: "triage", Digest: "sha256:edited"}, // edited in place
		{Name: "placed", Digest: "sha256:ddd"},    // put there by hand
	})

	if len(changed) != 2 {
		t.Fatalf("want two, got %d: %v", len(changed), changed)
	}
	joined := changed[0] + " " + changed[1]
	for _, expected := range []string{"gone", "no longer there", "triage", "edited"} {
		if !contains(joined, expected) {
			t.Errorf("%q is not reported: %v", expected, changed)
		}
	}
	if contains(joined, "release") {
		t.Errorf("an untouched skill was reported as changed: %v", changed)
	}
	// A skill placed by hand was never installed, so there is nothing to
	// compare it against and nothing to say about it.
	if contains(joined, "placed") {
		t.Errorf("a skill nobody installed was reported: %v", changed)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// TestReplacingTheRecordIsNeverHalfDone matters because the next decision
// somebody makes is read out of this file.
func TestReplacingTheRecordIsNeverHalfDone(t *testing.T) {
	root := t.TempDir()

	for round := range 20 {
		lock := Lock{}.Record(aLocked("release", "sha256:"+string(rune('a'+round))))
		if err := WriteLock(root, lock); err != nil {
			t.Fatalf("write: %v", err)
		}

		got, err := ReadLock(root)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got.Skills) != 1 {
			t.Fatalf("a read saw %d entries", len(got.Skills))
		}
	}

	// And nothing left behind.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != LockName {
			t.Errorf("something was left behind: %s", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, LockName)); err != nil {
		t.Errorf("the record is not there: %v", err)
	}
}
