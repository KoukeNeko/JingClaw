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
	"slices"

	"github.com/KoukeNeko/JingClaw/core/internal/home"
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

	// Files are fallback paths checked in order, each expected to contain only
	// the credential. First readable file wins.
	Files []string
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

	for _, path := range opts.Files {
		if path == "" {
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Value{}, fmt.Errorf("secret: stat %s: %w", path, err)
		}

		// A credential readable by other local accounts is a credential to
		// treat as compromised, so this refuses rather than quietly using it.
		// Refusing beats skipping: silently falling through to the next
		// candidate would hide the exposure.
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return Value{}, fmt.Errorf(
				"secret: %s is mode %#o; it must not be readable by group or others (chmod 600)",
				path, mode)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return Value{}, fmt.Errorf("secret: read %s: %w", path, err)
		}
		if strings.TrimSpace(string(raw)) == "" {
			continue
		}

		return New(string(raw)), nil
	}

	return Value{}, nil
}

// DefaultFiles returns the conventional locations for a named credential, in
// search order.
//
// The platform-native config directory comes first. On macOS that is
// ~/Library/Application Support, but developer tools there commonly follow the
// XDG convention instead, and a headless macOS build box is usually set up
// that way, so ~/.config is also accepted. On Linux os.UserConfigDir already
// resolves to ~/.config and the two collapse into one entry.
func DefaultFiles(name string) ([]string, error) {
	var files []string

	// A .JingClaw directory is looked in first when there is one, so a
	// credential lives beside the deployment that uses it rather than in a
	// place shared with every other.
	if dir, found := home.FromWorkingDirectory(); found {
		files = append(files, dir.SecretFile(name))
	}

	base, err := os.UserConfigDir()
	if err != nil {
		if len(files) > 0 {
			return files, nil
		}
		return nil, fmt.Errorf("secret: locate config dir: %w", err)
	}
	files = append(files, filepath.Join(base, appDir, name))

	// Developer tools on macOS commonly follow the XDG convention even though
	// os.UserConfigDir does not, so a file there is honoured too.
	if userHome, err := os.UserHomeDir(); err == nil {
		xdg := filepath.Join(userHome, ".config", appDir, name)
		if !slices.Contains(files, xdg) {
			files = append(files, xdg)
		}
	}

	return files, nil
}

const appDir = "JingClaw"

// Find is the whole of "where does this credential come from".
//
// The environment first, then the files DefaultFiles names — and a clear
// refusal naming both when there is nothing, because "no token" with no
// indication of where one should go is a message somebody has to read the
// source to act on.
//
// Here rather than in each command that needs one. There are three now, and
// three copies of this would eventually disagree about which places are
// looked in.
func Find(envVars []string, fileName string) (Value, error) {
	files, err := DefaultFiles(fileName)
	if err != nil {
		return Value{}, err
	}

	found, err := Load(LoadOptions{EnvVars: envVars, Files: files})
	if err != nil {
		return Value{}, err
	}
	if !found.IsSet() {
		return Value{}, fmt.Errorf(
			"no credential: set %s, or write it with mode 600 to one of: %v",
			strings.Join(envVars, " or "), files)
	}

	return found, nil
}
