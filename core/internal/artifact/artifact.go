// Package artifact stores output too large to put in front of a model.
//
// A build log that fails at line 40,000 is the most useful thing in the
// session and the least printable. Truncating it to fit the context window
// throws away the part somebody will want; keeping it whole ends the session.
// So the model gets a bounded excerpt and an identifier, and the bytes stay on
// disk where a tool call or a client can go back for them.
//
// Content is addressed by its digest, which means storing the same thing twice
// costs nothing. Running a failing test suite four times is a normal afternoon,
// and four identical 400 KB logs should not be four copies.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// algorithm is part of the identifier rather than assumed, so that
	// replacing it later does not mean guessing what an old identifier meant.
	algorithm = "sha256"

	// DefaultMaxBytes bounds one artifact. Something larger is a file that
	// should be on disk in the workspace, not output that was captured.
	DefaultMaxBytes = 64 << 20

	dirPerm  = 0o700
	filePerm = 0o600
)

// ErrTooLarge is returned when content exceeds the configured bound.
var ErrTooLarge = errors.New("artifact: content is larger than the limit")

// ErrNotFound is returned for an identifier the store does not hold.
var ErrNotFound = errors.New("artifact: no such artifact")

// Ref identifies stored content and says how much of it there is.
type Ref struct {
	// ID is "sha256-<hex>". It is the content's own digest, so two tools
	// producing identical output produce the same identifier.
	ID string

	Size int64

	// MediaType is what the content is, for a client deciding how to show it.
	// It is not sniffed: the tool that produced the bytes knows, and guessing
	// from the first few bytes is how a diff becomes an octet-stream.
	MediaType string
}

// Store keeps artifacts under a directory.
type Store struct {
	root     string
	maxBytes int64
}

// Open prepares the store, creating its directories.
func Open(root string, maxBytes int64) (*Store, error) {
	if root == "" {
		return nil, errors.New("artifact: no directory given")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	for _, dir := range []string{root, filepath.Join(root, "incoming")} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("artifact: create %s: %w", dir, err)
		}
	}

	return &Store{root: root, maxBytes: maxBytes}, nil
}

// Put stores everything the reader produces and returns its identifier.
//
// The content is written to a temporary file while being hashed, then renamed
// into place. The rename is atomic, so a crash halfway through leaves a stray
// temporary file rather than an artifact whose identifier promises content it
// does not have.
func (s *Store) Put(ctx context.Context, content io.Reader, mediaType string) (Ref, error) {
	incoming, err := os.CreateTemp(filepath.Join(s.root, "incoming"), "put-*")
	if err != nil {
		return Ref{}, fmt.Errorf("artifact: create a temporary file: %w", err)
	}
	temporary := incoming.Name()

	// Removed on every path that does not rename it away. A store that
	// accumulates half-written files is one that fills a disk quietly.
	defer func() {
		_ = incoming.Close()
		_ = os.Remove(temporary)
	}()

	if err := os.Chmod(temporary, filePerm); err != nil {
		return Ref{}, fmt.Errorf("artifact: set permissions: %w", err)
	}

	digest := sha256.New()
	// One extra byte past the limit is enough to tell "at the limit" from
	// "over it" without reading the rest of something enormous.
	written, err := io.Copy(io.MultiWriter(incoming, digest),
		io.LimitReader(readerWithContext{ctx: ctx, inner: content}, s.maxBytes+1))
	if err != nil {
		return Ref{}, fmt.Errorf("artifact: read the content: %w", err)
	}
	if written > s.maxBytes {
		return Ref{}, fmt.Errorf("%w of %d bytes", ErrTooLarge, s.maxBytes)
	}

	if err := incoming.Sync(); err != nil {
		return Ref{}, fmt.Errorf("artifact: flush: %w", err)
	}
	if err := incoming.Close(); err != nil {
		return Ref{}, fmt.Errorf("artifact: close: %w", err)
	}

	id := algorithm + "-" + hex.EncodeToString(digest.Sum(nil))

	path, err := s.pathFor(id)
	if err != nil {
		return Ref{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return Ref{}, fmt.Errorf("artifact: create the directory: %w", err)
	}

	// Already there means somebody stored these exact bytes before, and the
	// bytes are the identity, so there is nothing to do but keep the original.
	if _, err := os.Stat(path); err == nil {
		return Ref{ID: id, Size: written, MediaType: mediaType}, nil
	}

	if err := os.Rename(temporary, path); err != nil {
		return Ref{}, fmt.Errorf("artifact: store: %w", err)
	}

	return Ref{ID: id, Size: written, MediaType: mediaType}, nil
}

// PutBytes is Put for content already in memory.
func (s *Store) PutBytes(ctx context.Context, content []byte, mediaType string) (Ref, error) {
	return s.Put(ctx, strings.NewReader(string(content)), mediaType)
}

// Stat reports what the store holds under an identifier.
func (s *Store) Stat(id string) (Ref, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return Ref{}, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Ref{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Ref{}, fmt.Errorf("artifact: stat %s: %w", id, err)
	}

	return Ref{ID: id, Size: info.Size()}, nil
}

// Reader opens an artifact for reading.
func (s *Store) Reader(id string) (io.ReadSeekCloser, error) {
	path, err := s.pathFor(id)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: open %s: %w", id, err)
	}

	return file, nil
}

// ReadRange returns a window of an artifact, and the total size.
//
// The size comes back with every read so that a caller paging through does not
// have to hold a separate belief about how much there is; that belief is how
// paging loops end one window early or one too late.
func (s *Store) ReadRange(id string, offset, limit int64) ([]byte, int64, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("artifact: offset %d is before the beginning", offset)
	}
	if limit <= 0 {
		return nil, 0, fmt.Errorf("artifact: a limit of %d asks for nothing", limit)
	}

	reader, err := s.Reader(id)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = reader.Close() }()

	total, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, fmt.Errorf("artifact: measure %s: %w", id, err)
	}
	if offset >= total {
		return nil, total, nil
	}
	if _, err := reader.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("artifact: seek in %s: %w", id, err)
	}

	window := make([]byte, min(limit, total-offset))
	if _, err := io.ReadFull(reader, window); err != nil {
		return nil, 0, fmt.Errorf("artifact: read %s: %w", id, err)
	}

	return window, total, nil
}

// pathFor maps an identifier to a file, and refuses anything that is not one.
//
// The identifier reaches this function from a model, so it is not trusted to
// be a digest. Validating the shape is what keeps "../../../etc/passwd" from
// being a path this store will happily open.
func (s *Store) pathFor(id string) (string, error) {
	name, ok := strings.CutPrefix(id, algorithm+"-")
	if !ok {
		return "", fmt.Errorf("artifact: %q is not an artifact identifier", id)
	}

	// Exactly a sha256 digest in lowercase hex, and nothing else. Anything
	// that decodes cannot contain a separator or a dot.
	if len(name) != sha256.Size*2 {
		return "", fmt.Errorf("artifact: %q is not an artifact identifier", id)
	}
	if _, err := hex.DecodeString(name); err != nil {
		return "", fmt.Errorf("artifact: %q is not an artifact identifier", id)
	}

	// Two levels of fan-out. A single directory with a hundred thousand files
	// in it is slow to list on every filesystem people actually use.
	return filepath.Join(s.root, algorithm, name[:2], name), nil
}

// readerWithContext makes a long copy abandonable.
//
// Capturing four hundred megabytes from a command that will not stop should
// end when the run does, not when the disk fills.
type readerWithContext struct {
	ctx   context.Context
	inner io.Reader
}

func (r readerWithContext) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.inner.Read(p)
}
