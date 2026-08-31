package skill

import (
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
func (s Source) String() string {
	said := "git:" + s.Repository + "#" + s.Commit
	if s.Path != "" {
		said += ":" + s.Path
	}
	return said
}

// fullCommit is what a revision has to look like.
//
// Full, not abbreviated. A short hash is a prefix that was unique when
// somebody copied it, and a repository that grows a colliding object turns a
// lock file into a lie. Git itself has moved this way for the same reason.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParseSource reads the one form, and says what is wrong with anything else.
func ParseSource(text string) (Source, error) {
	text = strings.TrimSpace(text)

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
			text)
	}

	// The path is after the commit, so the last colon of a URL — the one in
	// https:// — cannot be mistaken for it.
	address, revision, found := strings.Cut(rest, "#")
	if !found {
		return Source{}, fmt.Errorf(
			"skill: %q names no commit; add #<commit> so what is installed can be "+
				"said exactly afterwards", text)
	}

	commit, path, _ := strings.Cut(revision, ":")

	source := Source{
		Repository: strings.TrimSpace(address),
		Commit:     strings.ToLower(strings.TrimSpace(commit)),
		Path:       strings.Trim(strings.TrimSpace(path), "/"),
	}

	if source.Repository == "" {
		return Source{}, fmt.Errorf("skill: %q names no repository", text)
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
func checkRepository(address string) error {
	// A leading dash is read by git as an option rather than an address.
	if strings.HasPrefix(address, "-") {
		return fmt.Errorf("skill: %q starts with a dash, which git reads as an option", address)
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("skill: %q is not an address git can be given: %w", address, err)
	}

	switch parsed.Scheme {
	case "https", "ssh", "git":
		return nil
	case "http":
		return fmt.Errorf(
			"skill: %q is not encrypted; what comes back becomes instructions this "+
				"agent reads, and anybody between here and there could write them",
			address)
	case "file", "":
		return fmt.Errorf(
			"skill: %q is a path on this machine; a source has to be somewhere the "+
				"install can be repeated from, or there is no point recording it",
			address)
	default:
		return fmt.Errorf("skill: %q uses %q, which is not a way to reach a repository",
			address, parsed.Scheme)
	}
}
