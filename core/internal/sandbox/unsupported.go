//go:build !darwin && !linux

package sandbox

// This file is what every platform without a backend gets: an honest no.
//
// Not a stub that pretends. The whole feature rests on one rule — a sandbox
// that runs the command anyway when it cannot confine it is worse than none,
// because the operator believes there is one — and the way to keep that rule
// on a platform nobody has implemented is to say so at the two places anybody
// asks.

// Available reports whether commands can be confined here. They cannot.
func Available() bool { return false }

// Wrap refuses, because there is nothing here to confine with.
func Wrap(Policy, string, []string) (string, []string, func(), error) {
	return "", nil, nil, ErrUnavailable
}

// LooksConfined reports whether a program refused because of the sandbox.
//
// Nothing is confined here, so nothing refused for that reason. Answering
// yes to any output would attribute an ordinary failure to a sandbox that
// does not exist.
func LooksConfined(string) bool { return false }

// describeBackend is never reached: Available is false here, and Describe
// asks it first.
func describeBackend() string { return "not available here" }
