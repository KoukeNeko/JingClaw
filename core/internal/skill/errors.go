package skill

import "errors"

// Why something was refused, for a caller that has to tell one reason from
// another without reading the words: a service answers a digest that no longer
// matches differently from a repository that could not be reached.
//
// None of these is ever what a person is shown. The words are the cause's own,
// and the sentinel only rides along with them (see because).
var (
	// ErrBusy is a second Stage while one is still fetching. Refused rather
	// than queued, because a fetch can take minutes and a request that waits
	// that long is indistinguishable from one that hung.
	ErrBusy = errors.New("skill: another skill is being fetched")

	// ErrSource is a source that did not yield a skill: the repository could
	// not be reached or does not have the commit, the path is not in it, or
	// what is there is not a skill that can be staged.
	ErrSource = errors.New("skill: the source did not yield a skill")

	// ErrNotStaged is a name nothing is staged under.
	ErrNotStaged = errors.New("skill: nothing is staged under that name")

	// ErrStale is an activation whose digest is not the staged skill's, or a
	// staged skill that is no longer the one that was staged. Either way, what
	// would be installed is not what was looked at.
	ErrStale = errors.New("skill: the staged skill is not the one that was reviewed")

	// ErrCollision is an activation that would replace a skill already in the
	// skills directory, installed or put there by hand, without being told it
	// may.
	ErrCollision = errors.New("skill: a skill of that name is already there")

	// ErrNotInstalled is a name no installed skill has.
	ErrNotInstalled = errors.New("skill: no such skill is installed")

	// ErrBadName is a name that could not be a skill's directory.
	ErrBadName = errors.New("skill: not a name a skill can have")
)

// tagged is an error that reads as its cause and is also one of the above, so
// that the message stays the one worth reading and errors.Is still answers.
type tagged struct {
	kind, cause error
}

// because tags a cause with the sentinel that says what kind of refusal it is.
func because(kind, cause error) error { return &tagged{kind: kind, cause: cause} }

func (t *tagged) Error() string   { return t.cause.Error() }
func (t *tagged) Unwrap() []error { return []error{t.kind, t.cause} }
