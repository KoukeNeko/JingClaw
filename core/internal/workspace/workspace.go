// Package workspace confines file access to a directory tree.
//
// Every path a tool receives comes from a language model, which means it comes
// from text that may have been shaped by a web page, a file, or anything else
// the model read. Paths are therefore treated as untrusted input: resolved,
// canonicalised, and checked against the root before any I/O happens.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrOutsideWorkspace is returned for any path that resolves outside the
	// root, whether by traversal, by absolute path, or through a symlink.
	ErrOutsideWorkspace = errors.New("workspace: path is outside the workspace")

	ErrNotFound = errors.New("workspace: no such file or directory")
)

// Workspace is a resolved directory tree that tools may read.
type Workspace struct {
	// root is absolute and fully symlink-resolved, so comparisons against it
	// are meaningful.
	root string
}

// Open resolves a workspace root. The directory must already exist: creating
// it implicitly would let a typo silently produce an empty workspace.
func Open(path string) (*Workspace, error) {
	if path == "" {
		return nil, errors.New("workspace: no root given")
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve %s: %w", path, err)
	}

	// Resolving the root once means a symlinked root (/tmp on macOS is one)
	// does not make every path inside it look like an escape.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("workspace: %s does not exist", absolute)
		}
		return nil, fmt.Errorf("workspace: resolve %s: %w", absolute, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("workspace: stat %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace: %s is not a directory", resolved)
	}

	return &Workspace{root: resolved}, nil
}

// Root is the absolute, resolved workspace directory.
func (w *Workspace) Root() string { return w.root }

// Resolve turns a workspace-relative path into an absolute one, refusing
// anything that escapes.
//
// The check happens after symlink resolution, because a symlink pointing at
// /etc/passwd is a lexically innocent path. Where the final element does not
// exist yet, its parent is resolved instead, so a write target can be
// validated before it is created.
func (w *Workspace) Resolve(relative string) (string, error) {
	if filepath.IsAbs(relative) {
		// An absolute path is not necessarily hostile, but accepting one would
		// mean the workspace root no longer describes what is reachable.
		return "", fmt.Errorf("%w: %s is absolute; use a path relative to the workspace root",
			ErrOutsideWorkspace, relative)
	}
	if relative == "" {
		relative = "."
	}

	// Reject traversal rather than clamping it. Joining a cleaned "/../x" onto
	// the root would silently turn a request for something outside into a
	// request for something inside, and a tool that quietly answers a
	// different question than the one asked is worse than one that refuses.
	cleaned := filepath.Clean(relative)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, relative)
	}

	// A component may reach outside without any traversal: on Windows a
	// reserved device name opens the device from any directory, and a colon
	// names a drive-relative path or an alternate data stream. Lexical
	// containment does not catch these, so they are refused before the join.
	if escapesByName(cleaned) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, relative)
	}

	candidate := filepath.Join(w.root, cleaned)

	resolved, err := resolveExisting(candidate)
	if err != nil {
		return "", err
	}
	if !w.contains(resolved) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, relative)
	}

	return candidate, nil
}

// RelativeTo turns an absolute path back into a workspace-relative one, for
// showing a model or a user a path that means something.
func (w *Workspace) RelativeTo(absolute string) (string, error) {
	// The root is symlink-resolved, so the input has to be too before the two
	// can be compared. On macOS /var is a link to /private/var, which is
	// enough on its own to make an unresolved path look like an escape.
	resolved, err := resolveExisting(absolute)
	if err != nil {
		return "", err
	}

	relative, err := filepath.Rel(w.root, resolved)
	if err != nil {
		return "", fmt.Errorf("workspace: relativise %s: %w", absolute, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideWorkspace, absolute)
	}
	return filepath.ToSlash(relative), nil
}

// contains reports whether resolved sits at or under the root.
//
// The separator suffix matters: without it, "/work" would appear to contain
// "/workspace-elsewhere".
func (w *Workspace) contains(resolved string) bool {
	if resolved == w.root {
		return true
	}
	return strings.HasPrefix(resolved, w.root+string(filepath.Separator))
}

// resolveExisting evaluates symlinks on the longest existing prefix of path.
//
// EvalSymlinks fails outright on a path that does not exist, but a caller may
// legitimately be asking about a file it is about to create. Walking up to the
// nearest existing ancestor lets that case be validated without weakening the
// check for paths that do exist.
func resolveExisting(path string) (string, error) {
	remainder := ""
	current := path

	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("workspace: resolve %s: %w", path, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Walked past the filesystem root without finding anything.
			return "", fmt.Errorf("%w: %s", ErrNotFound, path)
		}

		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}
