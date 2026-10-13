package secret

import "time"

// Source is where Load takes a credential from.
type Source int

const (
	// SourceNone is nothing set and no file with anything in it.
	SourceNone Source = iota

	// SourceEnvironment is one of the variables, named in Verdict.EnvVar.
	SourceEnvironment

	// SourceFile is the file.
	SourceFile

	// SourceBlocked is a file Load refuses: it returns an error, and a process
	// that asked for the credential does not start.
	SourceBlocked
)

// FileState is what Load found at a credential's file.
type FileState int

const (
	// FileNone is that no file was looked at, because none was listed or the
	// environment decided first.
	FileNone FileState = iota
	FileAbsent

	// FileEmpty has nothing but whitespace in it, which Load reads as not being
	// there.
	FileEmpty
	FilePresent
	FileWrongMode
	FileNotRegular
	FileUnreadable
)

// Verdict is what Load would do, and none of what it would load.
//
// There is no field for the credential, its size, any part of it or anything
// computed from it, and there must not be: this is what a status page is made
// of. The one thing taken from a file is when it was last modified.
type Verdict struct {
	Source Source

	// EnvVar is the variable in use, for SourceEnvironment.
	EnvVar string

	File FileState

	// Detail is why, for a file Load refuses: the mode for FileWrongMode, and
	// what the system said for FileUnreadable. Nothing from inside the file.
	Detail string

	// ModifiedAt is when the file was last modified, zero when it was not found.
	ModifiedAt time.Time

	// RestartNeeded is that the file the credential is read from was modified
	// after started, so that a process which began then holds what it said
	// before. False when the environment decides, because the file is then not
	// what is read.
	RestartNeeded bool
}

// Inspect says what Load would do with these options, without handing anyone
// the credential.
//
// It makes the decision Load makes, in the order Load makes it, because it is
// Load's own walk and not a second one. It does read the file, since whether a
// file with only whitespace in it counts as a credential cannot be told
// otherwise, and drops what it read.
func Inspect(opts LoadOptions, started time.Time) Verdict {
	// The error is the same fact as the verdict's Source, with the path in it,
	// and the path is nothing this is for.
	found, _, _ := resolve(opts)

	found.RestartNeeded = found.Source != SourceEnvironment &&
		!found.ModifiedAt.IsZero() && found.ModifiedAt.After(started)
	return found
}
