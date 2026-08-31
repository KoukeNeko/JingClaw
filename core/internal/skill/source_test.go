package skill

import (
	"strings"
	"testing"
)

func TestTheOneFormIsRead(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for _, one := range []struct {
		text string
		want Source
	}{
		{
			"git:https://github.com/someone/skills#" + commit + ":release",
			Source{"https://github.com/someone/skills", commit, "release"},
		},
		{
			// No path: the repository root is the skill.
			"git:https://github.com/someone/one-skill#" + commit,
			Source{"https://github.com/someone/one-skill", commit, ""},
		},
		{
			// A nested path, and slashes trimmed off the ends.
			"git:ssh://git@example.com/x.git#" + commit + ":/skills/release/",
			Source{"ssh://git@example.com/x.git", commit, "skills/release"},
		},
		{
			// The colon in https:// is not the path separator, because the
			// path is looked for after the commit.
			"git:https://example.com:8443/x#" + commit,
			Source{"https://example.com:8443/x", commit, ""},
		},
	} {
		got, err := ParseSource(one.text)
		if err != nil {
			t.Errorf("ParseSource(%q): %v", one.text, err)
			continue
		}
		if got != one.want {
			t.Errorf("ParseSource(%q) = %+v, want %+v", one.text, got, one.want)
		}
		// And it says itself back the way it was written, so an error can
		// quote it.
		if !strings.Contains(got.String(), got.Commit) {
			t.Errorf("%+v does not say its own commit: %q", got, got.String())
		}
	}
}

// TestANameThatMovesIsRefused is the reason a commit is required.
//
// A branch or a tag is a name somebody else can repoint, and a skill is text
// that goes in front of the model asking it to do things. "I installed the
// release skill" has to mean one exact file.
func TestANameThatMovesIsRefused(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for _, one := range []struct {
		text string
		says string
	}{
		{"git:https://example.com/x#main", "moves"},
		{"git:https://example.com/x#v1.2.0", "moves"},
		{"git:https://example.com/x#a1b2c3d", "abbreviated"},
		{"git:https://example.com/x", "names no commit"},
		{"https://example.com/x#" + commit, "not a source"},
		{"git:#" + commit, "names no repository"},
	} {
		_, err := ParseSource(one.text)
		if err == nil {
			t.Errorf("ParseSource(%q) was accepted", one.text)
			continue
		}
		if !strings.Contains(err.Error(), one.says) {
			t.Errorf("ParseSource(%q) does not say %q: %v", one.text, one.says, err)
		}
	}
}

// TestAnAddressThatCannotBeRepeatedIsRefused covers the two that would make
// the record useless or unsafe.
func TestAnAddressThatCannotBeRepeatedIsRefused(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for _, one := range []struct {
		text string
		says string
	}{
		// Whatever comes back becomes instructions this agent reads.
		{"git:http://example.com/x#" + commit, "not encrypted"},
		// A path on this machine records a source nobody else can resolve.
		{"git:/home/me/skills#" + commit, "path on this machine"},
		{"git:file:///home/me/skills#" + commit, "path on this machine"},
		// git would read this as an option rather than an address.
		{"git:--upload-pack=touch#" + commit, "option"},
	} {
		if _, err := ParseSource(one.text); err == nil {
			t.Errorf("ParseSource(%q) was accepted", one.text)
		} else if !strings.Contains(err.Error(), one.says) {
			t.Errorf("ParseSource(%q) does not say %q: %v", one.text, one.says, err)
		}
	}
}

func TestAPathThatClimbsOutIsRefused(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	if _, err := ParseSource("git:https://example.com/x#" + commit + ":../../etc"); err == nil {
		t.Error("a path reaching outside the repository was accepted")
	}
}

// TestTheSchemeAndThePrefixAreTheSameFourCharacters is a collision in the
// format itself.
//
// The prefix is "git:" and one of the schemes git accepts is "git://". Taking
// the prefix off the second leaves "//host/path", which has no scheme and is
// read as a path on this machine — refused for being one, with an error about
// something the person did not write.
func TestTheSchemeAndThePrefixAreTheSameFourCharacters(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	got, err := ParseSource("git://example.com/skills#" + commit + ":release")
	if err != nil {
		t.Fatalf("a git:// address was refused: %v", err)
	}
	if got.Repository != "git://example.com/skills" {
		t.Errorf("the scheme was eaten: %q", got.Repository)
	}
	if got.Path != "release" {
		t.Errorf("path %q", got.Path)
	}

	// And the ordinary form still works, which is what the prefix is for.
	got, err = ParseSource("git:https://example.com/skills#" + commit)
	if err != nil {
		t.Fatalf("the prefixed form was refused: %v", err)
	}
	if got.Repository != "https://example.com/skills" {
		t.Errorf("the prefix was not taken off: %q", got.Repository)
	}
}
