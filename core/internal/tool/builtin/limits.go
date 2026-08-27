package builtin

import "time"

// Limits bound what the built-in tools will read, search and run.
//
// They are settings rather than constants because the right ceiling depends on
// the model's context window and on what the workspace contains: a repository
// of generated SDKs and one of hand-written Go want different answers.
//
// A zero field takes the default, so a caller can set only what it cares about
// and a new limit does not silently become zero for every existing caller.
type Limits struct {
	ReadLimit         int64
	MaxReadableFile   int64
	MaxOverwriteBytes int64
	MaxSearchableFile int64

	GlobResults int
	GrepResults int

	CommandTimeout    time.Duration
	MaxCommandTimeout time.Duration
	MaxCommandOutput  int
}

// DefaultLimits are what runs when nothing says otherwise.
func DefaultLimits() Limits {
	return Limits{
		ReadLimit:         64 * 1024,
		MaxReadableFile:   8 * 1024 * 1024,
		MaxOverwriteBytes: 128 * 1024,
		MaxSearchableFile: 2 * 1024 * 1024,
		GlobResults:       200,
		GrepResults:       100,
		CommandTimeout:    2 * time.Minute,
		MaxCommandTimeout: 10 * time.Minute,
		MaxCommandOutput:  32 * 1024,
	}
}

// withDefaults fills anything left unset.
func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()

	if l.ReadLimit <= 0 {
		l.ReadLimit = defaults.ReadLimit
	}
	if l.MaxReadableFile <= 0 {
		l.MaxReadableFile = defaults.MaxReadableFile
	}
	if l.MaxOverwriteBytes <= 0 {
		l.MaxOverwriteBytes = defaults.MaxOverwriteBytes
	}
	if l.MaxSearchableFile <= 0 {
		l.MaxSearchableFile = defaults.MaxSearchableFile
	}
	if l.GlobResults <= 0 {
		l.GlobResults = defaults.GlobResults
	}
	if l.GrepResults <= 0 {
		l.GrepResults = defaults.GrepResults
	}
	if l.CommandTimeout <= 0 {
		l.CommandTimeout = defaults.CommandTimeout
	}
	if l.MaxCommandTimeout <= 0 {
		l.MaxCommandTimeout = defaults.MaxCommandTimeout
	}
	if l.MaxCommandOutput <= 0 {
		l.MaxCommandOutput = defaults.MaxCommandOutput
	}

	return l
}
