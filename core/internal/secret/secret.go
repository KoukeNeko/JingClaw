// Package secret loads credentials without letting them escape.
//
// Values loaded here must never be logged, echoed in an error, written to the
// event log, or included in a discovery file. A credential that reaches a log
// is a credential that has to be rotated.
package secret

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
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
	found, raw, err := resolve(opts)
	if err != nil {
		return Value{}, err
	}
	if found.Source == SourceNone {
		return Value{}, nil
	}
	return New(raw), nil
}

// resolve is the whole of Load's decision, and the one place it is made.
//
// Inspect asks the same question and keeps only the verdict. Two copies of the
// order the environment and the files are looked in, and of what counts as a
// credential, would eventually disagree, and a status that says a key will be
// used when the daemon refuses it is worse than none.
func resolve(opts LoadOptions) (found Verdict, raw string, err error) {
	for _, name := range opts.EnvVars {
		if given := os.Getenv(name); strings.TrimSpace(given) != "" {
			return Verdict{Source: SourceEnvironment, EnvVar: name}, given, nil
		}
	}

	for _, path := range opts.Files {
		if path == "" {
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				found.File = FileAbsent
				continue
			}
			return blocked(found, FileUnreadable, why(err)), "", fmt.Errorf("secret: stat %s: %w", path, err)
		}
		found.ModifiedAt = info.ModTime()

		// Before anything is read: opening a pipe for reading waits for a writer,
		// and a directory is not a credential however its mode reads.
		if !info.Mode().IsRegular() {
			return blocked(found, FileNotRegular, ""), "", fmt.Errorf("secret: %s is not a regular file", path)
		}

		// A credential readable by other local accounts is a credential to
		// treat as compromised, so this refuses rather than quietly using it.
		// Refusing beats skipping: silently falling through to the next
		// candidate would hide the exposure.
		ownerOnly, exposure, err := fsperm.EnsureOwnerOnly(path)
		if err != nil {
			return blocked(found, FileUnreadable, why(err)), "", fmt.Errorf("secret: check %s: %w", path, err)
		}
		if !ownerOnly {
			return blocked(found, FileWrongMode, exposure), "", fmt.Errorf(
				"secret: %s %s; it must be readable only by its owner (chmod 600)",
				path, exposure)
		}

		// Never more than the largest file a credential can be kept in, which is
		// also what the calls that replace or delete one accept. Without a bound a
		// file of any size at a credential's name was read whole, by the daemon at
		// start and by every status request after it.
		content, err := readBounded(path)
		if errors.Is(err, errTooLarge) {
			return blocked(found, FileUnreadable, fmt.Sprintf("is larger than %d bytes, so it is not a credential file", MaxFileBytes)),
				"", fmt.Errorf("secret: %s is larger than %d bytes, so it is not a credential file", path, MaxFileBytes)
		}
		if err != nil {
			return blocked(found, FileUnreadable, why(err)), "", fmt.Errorf("secret: read %s: %w", path, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			found.File = FileEmpty
			continue
		}

		found.Source, found.File = SourceFile, FilePresent
		return found, string(content), nil
	}

	return found, "", nil
}

var errTooLarge = errors.New("larger than a credential file")

// readBounded is a file's content, or errTooLarge when it holds more than
// MaxFileBytes. At most one byte more than that is ever read.
func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > MaxFileBytes {
		return nil, errTooLarge
	}
	return content, nil
}

// blocked is the verdict for a file Load refuses.
func blocked(found Verdict, state FileState, detail string) Verdict {
	found.Source, found.File, found.Detail = SourceBlocked, state, detail
	return found
}

// why is what the system said, without the path it said it about.
func why(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return "could not be read"
}

// DefaultFiles returns where a named credential file is looked for, in search
// order: the deployment's own directory, and nowhere else. It is empty for no
// name, or when there is no deployment directory.
func DefaultFiles(name string) ([]string, error) {
	// No name is no file, not the directory the files live in. A backend that
	// needs no credential leaves this empty, and joining "" onto each location
	// would hand back the directories themselves — which read as a file only
	// far enough to fail with "is a directory".
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}

	var files []string

	// Beside the deployment that uses it, and nowhere else. A credential found
	// in a second location belongs to a second deployment, and reading it here
	// is how one agent ends up authenticating as another.
	dir, found := home.Resolve()
	if !found {
		return nil, nil
	}
	files = append(files, dir.SecretFile(name))

	return files, nil
}

// Absent is the refusal for a credential that is nowhere: the variables to
// set, and the setting that names the file to put it in.
//
// The setting is named and not the file. The name of a credential file is
// something typed, the commonest mistake with it is to paste the key there, and
// this message goes into a log and onto the page.
func Absent(what string, envVars []string, setting, fileName string) error {
	var ways []string
	if len(envVars) > 0 {
		ways = append(ways, "set "+strings.Join(envVars, " or "))
	}
	if fileName == "" {
		ways = append(ways, "name a file in "+setting+" and write it there with mode 600")
	} else {
		ways = append(ways, "write it with mode 600 to the file that "+setting+" names")
	}
	return fmt.Errorf("no %s: %s", what, strings.Join(ways, ", or "))
}

// Find is the whole of "where does this credential come from".
//
// The environment first, then the files DefaultFiles names — and a clear
// refusal saying where one can go when there is nothing, because "no token"
// with no indication of where one should go is a message somebody has to read
// the source to act on. setting is the one that names the file.
//
// Here rather than in each command that needs one. There are three now, and
// three copies of this would eventually disagree about which places are
// looked in.
func Find(envVars []string, fileName, setting string) (Value, error) {
	files, err := DefaultFiles(fileName)
	if err != nil {
		return Value{}, err
	}

	found, err := Load(LoadOptions{EnvVars: envVars, Files: files})
	if err != nil {
		return Value{}, err
	}
	if !found.IsSet() {
		return Value{}, Absent("credential", envVars, setting, fileName)
	}

	return found, nil
}
