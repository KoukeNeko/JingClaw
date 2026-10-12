package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
)

// ErrChangedUnderneath is a write refused because the file is not the one the
// edit started from.
//
// Somebody else edited it — in an editor, or from another browser tab — after
// it was read. Writing over that would discard their change without either of
// them having seen the other's.
var ErrChangedUnderneath = errors.New("config: the file changed after it was read")

// backupsKept is how many earlier versions stay beside the file.
const backupsKept = 3

// Hash names the bytes of a file, so whoever edits it can say which version
// they started from.
func Hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Preview applies edits to the text of the file and says what the result
// would do: what changes, and what, if anything, is wrong with it.
//
// Nothing is written. The problems are the ones the daemon would report on
// starting with this file, because they come from the same check.
func Preview(src []byte, edits []Edit) (Patched, []Problem, error) {
	patched, err := Patch(src, edits)
	if err != nil {
		return Patched{}, nil, err
	}

	candidate, err := Candidate(patched.Text)
	if err != nil {
		return Patched{}, nil, err
	}

	var invalid *InvalidError
	if err := candidate.Validate(); errors.As(err, &invalid) {
		return patched, invalid.Problems, nil
	}
	return patched, nil, nil
}

// Replace writes content over the file, whole or not at all.
//
// A file half written when the machine stops is a daemon that will not start,
// and the setting that did it is in a file nobody can read. So the new content
// is written beside it and moved into place, and the version it replaces is
// kept: a few earlier ones, numbered, newest first.
//
// expect is the Hash of the file the edit started from. A file that is not
// that any more is refused, and a missing file is only acceptable when nothing
// was expected.
func Replace(path string, content []byte, expect string) error {
	// A file that is a link is replaced where it points, not by a regular
	// file standing where the link was.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}

	current, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if expect != "" {
			return ErrChangedUnderneath
		}
	case err != nil:
		return fmt.Errorf("config: read %s: %w", path, err)
	case expect != "" && Hash(current) != expect:
		return ErrChangedUnderneath
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}

	staged, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: stage the new file: %w", err)
	}
	defer func() { _ = os.Remove(staged.Name()) }()

	if _, err := staged.Write(content); err != nil {
		_ = staged.Close()
		return fmt.Errorf("config: write %s: %w", staged.Name(), err)
	}
	// On disk before the name moves, or a crash leaves the old name pointing
	// at nothing.
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return fmt.Errorf("config: write %s: %w", staged.Name(), err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("config: write %s: %w", staged.Name(), err)
	}
	if err := fsperm.Restrict(staged.Name()); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	if current != nil {
		if err := keepBackup(path, current); err != nil {
			return err
		}
	}

	if err := os.Rename(staged.Name(), path); err != nil {
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
}

// keepBackup shifts the numbered backups along and writes the newest.
func keepBackup(path string, previous []byte) error {
	name := func(n int) string { return path + ".bak." + strconv.Itoa(n) }

	for n := backupsKept; n > 1; n-- {
		if err := os.Rename(name(n-1), name(n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config: rotate backups: %w", err)
		}
	}

	if err := os.WriteFile(name(1), previous, 0o600); err != nil {
		return fmt.Errorf("config: keep a backup: %w", err)
	}
	if err := fsperm.Restrict(name(1)); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
