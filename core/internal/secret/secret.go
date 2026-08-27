// Package secret loads credentials without letting them escape.
//
// Values loaded here must never be logged, echoed in an error, written to the
// event log, or included in a discovery file. A credential that reaches a log
// is a credential that has to be rotated.
package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Value wraps a credential so that printing it by accident is hard. Its String
// and Format methods deliberately hide the contents; call Reveal at the exact
// point the value is handed to the client that needs it.
type Value struct {
	inner string
}

func New(raw string) Value { return Value{inner: strings.TrimSpace(raw)} }

func (v Value) Reveal() string { return v.inner }

func (v Value) IsSet() bool { return v.inner != "" }

// String is what fmt prints, including inside %v on a surrounding struct.
func (v Value) String() string {
	if v.inner == "" {
		return "<unset>"
	}
	return "<redacted>"
}

// Format covers %s, %q, %v and friends so no verb leaks the value.
func (v Value) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(v.String()))
}

// MarshalJSON keeps the value out of anything serialized, such as a config
// dump or a diagnostic bundle.
func (v Value) MarshalJSON() ([]byte, error) {
	return []byte(`"<redacted>"`), nil
}

// LoadOptions describes where to look for a credential.
type LoadOptions struct {
	// EnvVars are checked in order. First non-empty wins.
	EnvVars []string

	// File is a fallback path, expected to contain only the credential.
	File string
}

// Load reads a credential from the environment or a file.
//
// The environment is checked first so a shell session or a launchd plist can
// override the on-disk value without editing it. A missing credential is not
// an error here: the caller decides whether the feature it unlocks is
// required.
func Load(opts LoadOptions) (Value, error) {
	for _, name := range opts.EnvVars {
		if raw := os.Getenv(name); strings.TrimSpace(raw) != "" {
			return New(raw), nil
		}
	}

	if opts.File == "" {
		return Value{}, nil
	}

	info, err := os.Stat(opts.File)
	if err != nil {
		if os.IsNotExist(err) {
			return Value{}, nil
		}
		return Value{}, fmt.Errorf("secret: stat %s: %w", opts.File, err)
	}

	// A credential readable by other local accounts is a credential to treat
	// as compromised, so this refuses rather than quietly using it.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return Value{}, fmt.Errorf(
			"secret: %s is mode %#o; it must not be readable by group or others (chmod 600)",
			opts.File, mode)
	}

	raw, err := os.ReadFile(opts.File)
	if err != nil {
		return Value{}, fmt.Errorf("secret: read %s: %w", opts.File, err)
	}

	return New(string(raw)), nil
}

// DefaultFile returns the conventional path for a named credential inside the
// JingClaw config directory.
func DefaultFile(name string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("secret: locate config dir: %w", err)
	}
	return filepath.Join(base, "JingClaw", name), nil
}
