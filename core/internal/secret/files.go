package secret

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

const (
	// MaxValueBytes is the most a credential may be. The longest anything here
	// is given is a few hundred bytes; this leaves room for whatever a provider
	// invents next and still stops a file being passed off as one.
	MaxValueBytes = 8 << 10

	// MaxFileBytes is the most a file may hold and still be taken for a
	// credential file, which is what lets a call that replaces or deletes it be
	// sure it is not replacing or deleting something else. Twice the largest
	// value, for a file somebody saved with room to spare.
	MaxFileBytes = 16 << 10
)

var (
	// ErrInvalidValue is a value that cannot be a credential. The reason it is
	// wrapped with says what is wrong with it and never repeats any of it.
	ErrInvalidValue = errors.New("not a credential")

	// ErrNotCredentialFile is something at the file's name that is not a
	// credential file: a link, a directory, a file too large or of the wrong
	// shape. Nothing here replaces, removes or changes the mode of it.
	ErrNotCredentialFile = errors.New("exists and is not a credential file")

	// ErrNoFile is a call that needs the file, made when there is none.
	ErrNoFile = errors.New("there is no such file")

	// ErrChanged is a save that expected the file as it was and found it
	// modified, or gone.
	ErrChanged = errors.New("the file was modified after it was last read")
)

// Directory is the deployment directory credential files are written in.
//
// Opened once, with its links followed, so that every name is joined onto the
// directory itself and not onto a path that can lead somewhere else after the
// check. Callers hold one lock around anything that changes a file: nothing
// here is safe against two writers at once.
type Directory struct {
	resolved string
}

// OpenDirectory resolves the deployment directory.
func OpenDirectory(root string) (Directory, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Directory{}, fmt.Errorf("secret: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Directory{}, fmt.Errorf("secret: %w", err)
	}
	if !info.IsDir() {
		return Directory{}, fmt.Errorf("secret: %s is not a directory", resolved)
	}
	return Directory{resolved: resolved}, nil
}

// path is where a name lives, and refuses a name that is not a file directly in
// the directory. A second line behind the rule for what a name may be, which
// is the caller's: whatever that allows, this is what keeps a write inside.
func (d Directory) path(name string) (string, error) {
	path := filepath.Join(d.resolved, name)
	if filepath.Dir(path) != d.resolved || filepath.Base(path) != name {
		return "", errors.New("secret: the name is not a file directly inside the deployment directory")
	}
	return path, nil
}

// CheckValue is the credential a value stands for: the value with the
// whitespace around it taken off, or why it cannot be one.
//
// None of the reasons quote the value or say how long it is.
func CheckValue(value string) (string, error) {
	trimmed := strings.TrimSpace(value)

	switch {
	case trimmed == "":
		return "", fmt.Errorf("%w: it is empty", ErrInvalidValue)
	case len(trimmed) > MaxValueBytes:
		return "", fmt.Errorf("%w: it is longer than %d bytes", ErrInvalidValue, MaxValueBytes)
	case !utf8.ValidString(trimmed):
		return "", fmt.Errorf("%w: it is not UTF-8 text", ErrInvalidValue)
	case strings.ContainsRune(trimmed, 0):
		return "", fmt.Errorf("%w: it contains a NUL", ErrInvalidValue)
	case strings.ContainsAny(trimmed, "\r\n"):
		return "", fmt.Errorf("%w: it is more than one line", ErrInvalidValue)
	}
	return trimmed, nil
}

// credentialShaped is whether content is what this package would have written,
// give or take the line ending an editor adds.
func credentialShaped(content []byte) bool {
	if len(content) > MaxFileBytes || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return false
	}
	return !bytes.ContainsAny(bytes.TrimRight(content, "\r\n"), "\r\n")
}

// leaf looks at what is at path without following it, and says it is nothing,
// or a credential file, or neither.
//
// The mode is not part of the test. A file anybody can read is exactly the one
// a person is here to put right by saving it again, and refusing it would send
// them to a shell.
//
// shortcut: the name can be swapped for a link between this look and the call
// that follows, by somebody who can already write in the deployment directory,
// which is the operator's own account. Replacing and removing act on the name,
// so they are not led astray; only the content read here and a chmod can follow
// the link. Upgrade to opening with O_NOFOLLOW if that account ever stops being
// the only one trusted there.
func leaf(path string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil, ErrNotCredentialFile
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	if !credentialShaped(content) {
		return nil, ErrNotCredentialFile
	}
	return info, nil
}

// Write makes value the content of the credential file called name.
//
// Written beside the file and moved over it, with the owner-only mode on the
// new file before the first byte goes into it, so that at no moment is there a
// file anybody else could read, and a failure leaves the old file as it was and
// no temporary file behind. No backup is made: a copy of a secret is a second
// secret to protect.
//
// A non-zero expect is the modification time the caller last saw. Compared as a
// time and never as content, so nothing here has to hash or compare what is in
// the file.
func (d Directory) Write(name, value string, expect time.Time) error {
	return d.write(name, value, func(current fs.FileInfo) bool {
		return expect.IsZero() || (current != nil && current.ModTime().Equal(expect))
	})
}

// WriteNew is Write for a caller that was told there was no file: it saves only
// if there still is none. A file that has appeared since is somebody's, and is
// left as it is.
func (d Directory) WriteNew(name, value string) error {
	return d.write(name, value, func(current fs.FileInfo) bool { return current == nil })
}

// write saves value if unchanged accepts what is at the name now (nil for
// nothing).
func (d Directory) write(name, value string, unchanged func(fs.FileInfo) bool) error {
	value, err := CheckValue(value)
	if err != nil {
		return err
	}
	path, err := d.path(name)
	if err != nil {
		return err
	}

	current, err := leaf(path)
	if err != nil {
		return err
	}
	if !unchanged(current) {
		return ErrChanged
	}

	return d.replace(path, value)
}

// replace is the write itself, for a value and a path that have been checked.
func (d Directory) replace(path, value string) error {
	temporary, err := os.CreateTemp(d.resolved, ".credential-*.tmp")
	if err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	moved := false
	defer func() {
		if !moved {
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()

	// Before any byte. CreateTemp already makes it owner-only on Unix, and does
	// not on Windows; this is what makes it so on both.
	if err := fsperm.Restrict(temporary.Name()); err != nil {
		return err
	}
	if _, err := temporary.WriteString(value); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	moved = true

	return d.ownerOnly(path, true)
}

// Remove deletes the credential file called name, and says whether there was
// one to delete.
func (d Directory) Remove(name string) (bool, error) {
	path, err := d.path(name)
	if err != nil {
		return false, err
	}

	current, err := leaf(path)
	if err != nil || current == nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("secret: %w", err)
	}
	return true, nil
}

// Restrict makes the credential file called name readable by its owner alone,
// and says whether that changed anything.
func (d Directory) Restrict(name string) (bool, error) {
	path, err := d.path(name)
	if err != nil {
		return false, err
	}

	current, err := leaf(path)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, ErrNoFile
	}

	already, _, _ := fsperm.EnsureOwnerOnly(path)
	if err := fsperm.Restrict(path); err != nil {
		return false, err
	}
	if err := d.ownerOnly(path, false); err != nil {
		return false, err
	}
	return !already, nil
}

// ownerOnly asserts that a file nobody else can read is what is there.
//
// A file system that cannot keep a file private would otherwise be told so
// after the fact by the next start refusing the key. When the file was just
// written it is taken away again, so that a failed call does not leave a
// credential anybody can read.
func (d Directory) ownerOnly(path string, remove bool) error {
	ok, detail, err := fsperm.EnsureOwnerOnly(path)
	if err == nil && ok {
		return nil
	}
	if remove {
		_ = os.Remove(path)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("secret: the file %s after it was restricted, so this file system cannot keep it private", detail)
}
