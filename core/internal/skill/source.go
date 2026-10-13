package skill

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Source is where a skill was installed from.
//
// One form, deliberately: a git repository, at a commit somebody named, and a
// path inside it. There is no registry and no version range, because both of
// those answer "which one should I get" with something other than an exact
// answer — and the whole reason to record a source is to be able to say
// afterwards exactly what was read.
//
//	git:https://github.com/someone/skills#a1b2c3d:release
type Source struct {
	// Repository is the URL git is given.
	Repository string

	// Commit is the revision, written out. Not a branch and not a tag: both
	// of those are names that move, and a skill that changed under a name
	// somebody trusted is the thing this exists to make visible.
	Commit string

	// Path is the directory inside the repository holding SKILL.md. Empty
	// means the repository root is the skill.
	Path string
}

// String is the form somebody types, so an error can quote it back.
//
// With any credential in the address taken out, because the one thing this is
// for is being put in front of somebody: an error, an approval, a listing.
// ParseSource refuses an address that has one, so for a source it parsed this
// is the address exactly, except for the login of an ssh address. That is let
// through because it is how every ssh remote is written, and is redacted all the
// same because it cannot be told from a token put in its place; the address
// recorded for the install has it, and this does not.
func (s Source) String() string {
	said := "git:" + Redact(s.Repository) + "#" + s.Commit
	if s.Path != "" {
		said += ":" + s.Path
	}
	return said
}

// OverSSH is whether the repository is fetched over ssh, which is with the keys,
// the configuration and the known hosts of whoever runs the daemon.
func (s Source) OverSSH() bool {
	parsed, err := url.Parse(s.Repository)
	return err == nil && parsed.Scheme == "ssh"
}

// fullCommit is what a revision has to look like.
//
// Full, not abbreviated. A short hash is a prefix that was unique when
// somebody copied it, and a repository that grows a colliding object turns a
// lock file into a lie. Git itself has moved this way for the same reason.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// credentialedAddress is the user-info part of every address in a piece of
// text that has one: a scheme, then whatever precedes the last @ before the
// path.
var credentialedAddress = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#'"<>]*@`)

// Redact takes the credentials out of every address in a piece of text.
//
// For anything that leaves for a client: a source recorded before addresses
// with credentials were refused, a line git printed, what somebody typed. It
// works on text and finds all of them, which web.RedactAddress — one address,
// the first — does not; the text it is given here is an error or a git
// transcript, and either can say an address more than once.
//
// Everything before the @ goes, the user name with the password: a token is as
// often passed as a user name as in the password's place. That includes the
// login of an ssh address, which ParseSource lets stay: a login is not told
// apart from a token put where one goes, so none is repeated.
func Redact(text string) string {
	return credentialedAddress.ReplaceAllString(text, "${1}[redacted]@")
}

// ParseSource reads the one form, and says what is wrong with anything else.
//
// Nothing it says repeats a credential from the text it was given, whichever
// way the text is wrong.
func ParseSource(text string) (Source, error) {
	text = strings.TrimSpace(text)
	shown := Redact(text)

	// The prefix and the git:// scheme are the same four characters, so
	// stripping one from the other would leave "//host/path" with no scheme
	// at all — read as a path on this machine, and refused for being one.
	// A source whose address is itself a git URL keeps the whole of it.
	rest := text
	found := strings.HasPrefix(text, "git://")
	if !found {
		rest, found = strings.CutPrefix(text, "git:")
	}
	if !found {
		return Source{}, fmt.Errorf(
			"skill: %q is not a source; the one form is "+
				"git:<url>#<commit>[:<path>], as in "+
				"git:https://github.com/someone/skills#<40-character commit>:release",
			shown)
	}

	// The path is after the commit, so the last colon of a URL — the one in
	// https:// — cannot be mistaken for it.
	address, revision, found := strings.Cut(rest, "#")
	if !found {
		return Source{}, fmt.Errorf(
			"skill: %q names no commit; add #<commit> so what is installed can be "+
				"said exactly afterwards", shown)
	}

	commit, path, _ := strings.Cut(revision, ":")

	source := Source{
		Repository: strings.TrimSpace(address),
		Commit:     strings.ToLower(strings.TrimSpace(commit)),
		Path:       strings.Trim(strings.TrimSpace(path), "/"),
	}

	if source.Repository == "" {
		return Source{}, fmt.Errorf("skill: %q names no repository", shown)
	}
	if !fullCommit.MatchString(source.Commit) {
		return Source{}, fmt.Errorf(
			"skill: %q is not a full commit; a branch or a tag is a name that "+
				"moves, and an abbreviated hash is a prefix that was unique when "+
				"it was copied", commit)
	}
	if err := checkRepository(source.Repository); err != nil {
		return Source{}, err
	}
	if strings.Contains(source.Path, "..") {
		return Source{}, fmt.Errorf(
			"skill: %q reaches outside the repository", source.Path)
	}

	return source, nil
}

// checkRepository refuses addresses that would make git do something other
// than fetch.
//
// Not a security boundary — the clone below is what actually bounds this —
// but the useful place to say no to the obvious ones. A local path is refused
// because installing from one records a source nobody else can resolve, which
// defeats the point of writing it down.
//
// An address with a credential in it is refused too, for what happens to it
// afterwards: the address is written to installed.json and the staging record,
// quoted in errors and shown on an approval, and a password in any of those is
// a password in a file anybody who can read the skills directory can read. The
// one thing allowed before the @ is the user name of an ssh address, which is
// how every ssh remote is written and is not a secret.
func checkRepository(address string) error {
	shown := Redact(address)

	// A leading dash is read by git as an option rather than an address.
	if strings.HasPrefix(address, "-") {
		return fmt.Errorf("skill: %q starts with a dash, which git reads as an option", shown)
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("skill: %q is not an address git can be given: %s", shown, Redact(err.Error()))
	}

	switch parsed.Scheme {
	case "https", "ssh", "git":
		// Neither of these quotes the address back: what is wrong with it is where
		// a credential is put.
		if parsed.Host == "" || parsed.Opaque != "" {
			return errors.New("skill: the address is not written as scheme://host/path. Text that is " +
				"not is read by git as something else, such as a path or an scp-style ssh address, " +
				"and can hold a credential that this cannot find to refuse")
		}
		if parsed.RawQuery != "" {
			return errors.New("skill: the address has a query string, which is where a token is " +
				"put and is not part of any repository's address; take it off, and give git the " +
				"credential some other way, such as an ssh key")
		}
		if parsed.User != nil {
			if _, hasPassword := parsed.User.Password(); hasPassword || parsed.Scheme != "ssh" {
				return fmt.Errorf(
					"skill: %q has a credential in the address; it would be written into the "+
						"record of what was installed. Give git the credential some other way, "+
						"such as an ssh key", shown)
			}
		}
		return nil
	case "http":
		return fmt.Errorf(
			"skill: %q is not encrypted; what comes back becomes instructions this "+
				"agent reads, and anybody between here and there could write them",
			shown)
	case "file", "":
		return fmt.Errorf(
			"skill: %q is a path on this machine; a source has to be somewhere the "+
				"install can be repeated from, or there is no point recording it",
			shown)
	default:
		return fmt.Errorf("skill: %q uses %q, which is not a way to reach a repository",
			shown, parsed.Scheme)
	}
}
