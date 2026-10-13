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

// An address is written into installed.json, the staging record, errors and an
// approval. A password in it would be in all of them, and in the file anybody
// who can read the skills directory can read.
func TestACredentialInTheAddressIsRefused(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for _, one := range []struct {
		name string
		text string
	}{
		{"a password over https", "git:https://user:hunter2@example.com/x#" + commit},
		{"a password over git", "git://user:hunter2@example.com/x#" + commit},
		{"a token as the https user name", "git:https://hunter2@example.com/x#" + commit},
		{"a token as the git user name", "git://hunter2@example.com/x#" + commit},
		{"a password over ssh", "git:ssh://git:hunter2@example.com/x.git#" + commit},
		{"a password with an empty user", "git:https://:hunter2@example.com/x#" + commit},
		{"a password over plain http, which is refused for its own reason", "git:http://user:hunter2@example.com/x#" + commit},
		{"a password in an address that does not parse", "git:https://user:hunter2@example.com:notaport/x#" + commit},
		{"a password and no commit", "git:https://user:hunter2@example.com/x"},
		{"a password and a short commit", "git:https://user:hunter2@example.com/x#abc123"},
		{"a password and no repository", "https://user:hunter2@example.com/x#" + commit},
	} {
		t.Run(one.name, func(t *testing.T) {
			_, err := ParseSource(one.text)
			if err == nil {
				t.Fatal("accepted")
			}
			// Whichever way the text is wrong, the reason does not repeat it.
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal repeats the credential: %v", err)
			}
		})
	}

	t.Run("and the reason is the credential", func(t *testing.T) {
		_, err := ParseSource("git:https://user:hunter2@example.com/x#" + commit)
		if err == nil || !strings.Contains(err.Error(), "credential") {
			t.Errorf("the refusal does not say why: %v", err)
		}
	})
}

// The user name of an ssh address is how every ssh remote is written, and is
// not a secret.
func TestTheUserNameOfAnSSHAddressIsNotACredential(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	got, err := ParseSource("git:ssh://git@example.com/x.git#" + commit)
	if err != nil {
		t.Fatalf("an ssh address with a user name was refused: %v", err)
	}
	if got.Repository != "ssh://git@example.com/x.git" {
		t.Errorf("the user name was lost: %q", got.Repository)
	}
}

func TestCredentialsAreTakenOutOfAddressesInText(t *testing.T) {
	for _, one := range []struct{ in, want string }{
		{"nothing to take out", "nothing to take out"},
		{"https://example.com/x", "https://example.com/x"},
		{"https://user:hunter2@example.com/x", "https://[redacted]@example.com/x"},
		{"https://hunter2@example.com/x", "https://[redacted]@example.com/x"},
		{"git://u:p@example.com:9418/x.git", "git://[redacted]@example.com:9418/x.git"},
		// The login of an ssh address is let through by ParseSource and is
		// redacted here all the same: it cannot be told from a token put where
		// one goes.
		{"ssh://git@example.com/x.git", "ssh://[redacted]@example.com/x.git"},
		{"ssh://git:hunter2@example.com/x.git", "ssh://[redacted]@example.com/x.git"},
		// A password that has an @ in it: everything before the last one.
		{"https://user:p@ss@example.com/x", "https://[redacted]@example.com/x"},
		// All of them, and not only the first.
		{"fatal: 'https://a:b@one.example/x' then 'https://c@two.example/y'",
			"fatal: 'https://[redacted]@one.example/x' then 'https://[redacted]@two.example/y'"},
		// An @ further along is a path, not a credential.
		{"https://example.com/users/me@example.com/x", "https://example.com/users/me@example.com/x"},
		{"git:https://u:p@example.com/x#abc", "git:https://[redacted]@example.com/x#abc"},
	} {
		if got := Redact(one.in); got != one.want {
			t.Errorf("Redact(%q) = %q, want %q", one.in, got, one.want)
		}
	}
}

// A token is as often put in the query of an address as before the host, and
// git reads credentials out of an address that has no // after its scheme. Each
// is the same thing the refusal of a password is for, and the reason does not
// repeat either.
func TestAnAddressThatCarriesACredentialSomewhereElseIsRefused(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for _, one := range []struct {
		name string
		text string
	}{
		{"a token in the query", "git:https://example.com/x.git?private_token=hunter2#" + commit},
		{"a token in the query of an ssh address", "git:ssh://git@example.com/x.git?token=hunter2#" + commit},
		{"a user and password without the slashes", "git:https:user:hunter2@example.com/x#" + commit},
		{"a user and password without the slashes over ssh", "git:ssh:git:hunter2@example.com/x#" + commit},
		{"no host", "git:https:///x#" + commit},
		{"nothing but a scheme", "git:https:#" + commit},
	} {
		t.Run(one.name, func(t *testing.T) {
			_, err := ParseSource(one.text)
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal repeats the credential: %v", err)
			}
		})
	}
}

// What was accepted is said back exactly, so that it can be copied into the next
// request to stage it again, except for the login of an ssh address, which is
// let through and not repeated.
func TestASourceItAcceptedIsSaidBackExactlyExceptForAnSSHLogin(t *testing.T) {
	const commit = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"

	for text, want := range map[string]string{
		"git:https://example.com/x#" + commit:                    "git:https://example.com/x#" + commit,
		"git:git://example.com:9418/x.git#" + commit + ":a/b":    "git:git://example.com:9418/x.git#" + commit + ":a/b",
		"git:ssh://git@example.com/x.git#" + commit + ":release": "git:ssh://[redacted]@example.com/x.git#" + commit + ":release",
		"git:ssh://example.com/x.git#" + commit:                  "git:ssh://example.com/x.git#" + commit,
	} {
		got, err := ParseSource(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got.String() != want {
			t.Errorf("%s was said back as %q, want %q", text, got.String(), want)
		}
	}
}

// A source that was recorded before addresses with credentials were refused
// still says itself without one.
func TestASourceSaysItselfWithoutACredential(t *testing.T) {
	said := Source{Repository: "https://user:hunter2@example.com/x", Commit: strings.Repeat("a", 40), Path: "p"}.String()
	if strings.Contains(said, "hunter2") || !strings.Contains(said, "example.com/x") {
		t.Errorf("a source said %q", said)
	}
}
