package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// LockName is the file recording what was installed and from where.
const LockName = "installed.json"

// Locked is one installed skill, as the record of it.
//
// The record matters more here than it would for a dependency, and for a
// reason particular to skills: a skill is text that goes in front of the
// model and asks it to do things. Nothing in the running system can tell an
// instruction somebody wrote from one that arrived in a repository — the
// design is explicit that a skill grants nothing, so the enforcement is
// elsewhere — and this file is the only place that says which is which.
type Locked struct {
	// Name is what the skill calls itself, which is also its directory.
	Name string `json:"name"`

	// From is where it was fetched, exactly enough to fetch it again.
	From Source `json:"from"`

	// Digest is the hash of the SKILL.md that was installed.
	//
	// The point of the file. Everything else says where it came from; this
	// says what arrived, and it is what a later check compares against to
	// answer "is what I am reading what I agreed to".
	Digest string `json:"digest"`

	// TreeDigest is the hash of the whole installed directory, not only
	// SKILL.md. A skill can read the files beside its instructions, so what
	// arrived is the directory and not one file in it; a check that hashed
	// SKILL.md alone would call a skill unchanged while a sibling it reads had
	// been rewritten.
	TreeDigest string `json:"tree_digest,omitempty"`

	// Version is what the skill claimed, for somebody reading a listing. Not
	// identity: a file edited without touching this line is a different skill
	// wearing the same number.
	Version string `json:"version,omitempty"`

	InstalledAt time.Time `json:"installed_at"`
}

// Lock is the whole record, one entry per installed skill.
type Lock struct {
	Skills []Locked `json:"skills"`
}

// ReadLock loads the record, which is empty when nothing was installed.
func ReadLock(root string) (Lock, error) {
	raw, err := os.ReadFile(filepath.Join(root, LockName))
	if os.IsNotExist(err) {
		// Not an error. Skills put there by hand are the ordinary case, and
		// a deployment with no record is one where nothing was installed
		// from anywhere.
		return Lock{}, nil
	}
	if err != nil {
		return Lock{}, fmt.Errorf("skill: reading %s: %w", LockName, err)
	}

	var lock Lock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return Lock{}, fmt.Errorf("skill: %s is not readable: %w", LockName, err)
	}
	return lock, nil
}

// WriteLock replaces the record.
//
// Written whole and renamed into place, like every other file here that
// somebody's next decision depends on: a reader during a replacement sees the
// old record or the new one, never half of either.
func WriteLock(root string, lock Lock) error {
	sort.Slice(lock.Skills, func(a, b int) bool {
		return lock.Skills[a].Name < lock.Skills[b].Name
	})

	encoded, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	encoded = append(encoded, '\n')

	temp, err := os.CreateTemp(root, "."+LockName+"-*")
	if err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("skill: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("skill: %w", err)
	}
	return os.Rename(temp.Name(), filepath.Join(root, LockName))
}

// Record adds or replaces one skill's entry.
func (l Lock) Record(one Locked) Lock {
	for index, existing := range l.Skills {
		if existing.Name == one.Name {
			l.Skills[index] = one
			return l
		}
	}
	l.Skills = append(l.Skills, one)
	return l
}

// Forget removes one skill's entry, and says whether there was one.
func (l Lock) Forget(name string) (Lock, bool) {
	for index, existing := range l.Skills {
		if existing.Name == name {
			l.Skills = append(l.Skills[:index], l.Skills[index+1:]...)
			return l, true
		}
	}
	return l, false
}

// Entry is the record for one skill, if it was installed rather than placed.
func (l Lock) Entry(name string) (Locked, bool) {
	for _, existing := range l.Skills {
		if existing.Name == name {
			return existing, true
		}
	}
	return Locked{}, false
}

// Drift is how an installed skill differs from the record of installing it.
type Drift string

const (
	// DriftNone is a skill that is what was recorded. It is also what a skill
	// placed by hand is, because there is nothing to compare it with.
	DriftNone Drift = "NONE"

	// DriftEdited is a SKILL.md that is not the one that was installed.
	DriftEdited Drift = "EDITED"

	// DriftTreeEdited is a SKILL.md that is the one that was installed, in a
	// directory that is not: a file beside it was changed, added, removed, or
	// replaced by something that is not a plain file. A skill can read the
	// files beside its instructions, so this is a different skill wearing the
	// same SKILL.md.
	DriftTreeEdited Drift = "TREE_EDITED"

	// DriftMissing is a recorded skill that is no longer among the installed
	// ones: its directory is gone, or what is there no longer reads as a skill.
	DriftMissing Drift = "MISSING"
)

// Drifts says, for every skill the record has, how it differs from the record.
//
// Asked rather than assumed, because the alternative is a lock file that
// describes an intention. A skill edited in place since it was installed is
// not a problem by itself — somebody may have meant to — but it is something
// they should be able to find out without diffing anything.
//
// The tree is hashed from the skill's own directory, which Installed fills in.
// A record from before trees were hashed has no tree digest, and nothing is
// said about the tree of such a skill; one whose tree cannot be hashed at all
// is called edited, because it cannot be shown to be what was installed.
func Drifts(lock Lock, installed []Skill) map[string]Drift {
	found := make(map[string]Skill, len(installed))
	for _, one := range installed {
		found[one.Name] = one
	}

	drifts := make(map[string]Drift, len(lock.Skills))
	for _, recorded := range lock.Skills {
		one, present := found[recorded.Name]
		switch {
		case !present:
			drifts[recorded.Name] = DriftMissing
		case one.Digest != recorded.Digest:
			drifts[recorded.Name] = DriftEdited
		case recorded.TreeDigest != "":
			if tree, _, err := TreeDigest(one.Dir); err != nil || tree != recorded.TreeDigest {
				drifts[recorded.Name] = DriftTreeEdited
				continue
			}
			drifts[recorded.Name] = DriftNone
		default:
			drifts[recorded.Name] = DriftNone
		}
	}
	return drifts
}

// Changed is every installed skill that is no longer what was recorded, said in
// words for a person reading a listing.
func Changed(root string, lock Lock, installed []Skill) []string {
	var changed []string
	for name, drift := range Drifts(lock, installed) {
		switch drift {
		case DriftMissing:
			changed = append(changed, name+" (installed, and no longer there)")
		case DriftEdited:
			changed = append(changed, name+" (edited since it was installed)")
		case DriftTreeEdited:
			changed = append(changed, name+" (a file beside SKILL.md was changed since it was installed)")
		}
	}
	sort.Strings(changed)
	return changed
}
