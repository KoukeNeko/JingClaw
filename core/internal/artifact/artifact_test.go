package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/artifact"
)

func newStore(t *testing.T, maxBytes int64) (*artifact.Store, string) {
	t.Helper()

	root := t.TempDir()
	store, err := artifact.Open(root, maxBytes)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store, root
}

func put(t *testing.T, store *artifact.Store, content string) artifact.Ref {
	t.Helper()

	ref, err := store.PutBytes(context.Background(), []byte(content), "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return ref
}

func TestWhatGoesInComesOut(t *testing.T) {
	store, _ := newStore(t, 0)

	const content = "FAIL TestCountVowels\n江委員\n"
	ref := put(t, store, content)

	if ref.Size != int64(len(content)) {
		t.Errorf("size is %d, want %d", ref.Size, len(content))
	}

	reader, err := store.Reader(ref.ID)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var read bytes.Buffer
	if _, err := read.ReadFrom(reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.String() != content {
		t.Errorf("read back %q, want %q", read.String(), content)
	}
}

// Running a failing test suite four times in an afternoon is ordinary, and
// four identical logs should not be four copies.
func TestTheSameContentIsStoredOnce(t *testing.T) {
	store, root := newStore(t, 0)

	const log = "=== RUN TestThing\n--- FAIL\n"
	first := put(t, store, log)
	second := put(t, store, log)

	if first.ID != second.ID {
		t.Errorf("the same bytes produced two identifiers: %s and %s", first.ID, second.ID)
	}

	stored := 0
	if err := filepath.WalkDir(filepath.Join(root, "sha256"),
		func(_ string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				stored++
			}
			return nil
		}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	if stored != 1 {
		t.Errorf("%d files on disk for one piece of content", stored)
	}
}

func TestDifferentContentIsKeptApart(t *testing.T) {
	store, _ := newStore(t, 0)

	if put(t, store, "one").ID == put(t, store, "two").ID {
		t.Error("two different things got the same identifier")
	}
}

// The identifier reaches the store from a model, which makes it input rather
// than a fact. A store that opens whatever it is handed is a way out of the
// directory it was given.
func TestOnlyRealIdentifiersAreAccepted(t *testing.T) {
	store, _ := newStore(t, 0)

	for _, id := range []string{
		"../../../etc/passwd",
		"sha256-../../../etc/passwd",
		"sha256-" + strings.Repeat("a", 63), // one short
		"sha256-" + strings.Repeat("a", 65), // one long
		"sha256-" + strings.Repeat("z", 64), // not hex
		"md5-" + strings.Repeat("a", 32),    // another algorithm
		"sha256-",
		"",
	} {
		if _, err := store.Reader(id); err == nil {
			t.Errorf("%q was accepted as an identifier", id)
		}
		if _, err := store.Stat(id); err == nil {
			t.Errorf("%q was accepted by Stat", id)
		}
	}
}

func TestAMissingArtifactSaysSo(t *testing.T) {
	store, _ := newStore(t, 0)

	absent := "sha256-" + strings.Repeat("0", 64)

	_, err := store.Reader(absent)
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("error is %v, want one that reports it is not found", err)
	}
}

// Paging is what makes a large artifact useful rather than merely stored, and
// a caller paging through must not have to hold its own belief about how much
// there is.
func TestReadRangeWindowsTheContentAndSaysHowMuchThereIs(t *testing.T) {
	store, _ := newStore(t, 0)

	content := strings.Repeat("0123456789", 100)
	ref := put(t, store, content)

	window, total, err := store.ReadRange(ref.ID, 10, 5)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if string(window) != "01234" {
		t.Errorf("window is %q, want %q", window, "01234")
	}
	if total != int64(len(content)) {
		t.Errorf("total is %d, want %d", total, len(content))
	}

	// The last window is short rather than an error: a reader that has to
	// guess the final offset exactly will get it wrong.
	tail, _, err := store.ReadRange(ref.ID, int64(len(content))-3, 100)
	if err != nil {
		t.Fatalf("read the tail: %v", err)
	}
	if string(tail) != "789" {
		t.Errorf("the tail is %q, want %q", tail, "789")
	}

	// Reading past the end is empty, not an error, so a paging loop ends
	// cleanly instead of on a failure it has to interpret.
	past, total, err := store.ReadRange(ref.ID, int64(len(content))+50, 10)
	if err != nil {
		t.Fatalf("read past the end: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("reading past the end returned %d bytes", len(past))
	}
	if total != int64(len(content)) {
		t.Errorf("total is %d after reading past the end", total)
	}
}

func TestNonsensicalRangesAreRefused(t *testing.T) {
	store, _ := newStore(t, 0)
	ref := put(t, store, "something")

	if _, _, err := store.ReadRange(ref.ID, -1, 10); err == nil {
		t.Error("a negative offset was accepted")
	}
	if _, _, err := store.ReadRange(ref.ID, 0, 0); err == nil {
		t.Error("a limit of zero was accepted")
	}
}

// Something past the bound is a file that belongs in the workspace, not output
// that was captured, and refusing is better than filling a disk.
func TestContentPastTheLimitIsRefused(t *testing.T) {
	store, root := newStore(t, 100)

	_, err := store.PutBytes(context.Background(), bytes.Repeat([]byte("x"), 101), "text/plain")
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("error is %v, want one that reports the content is too large", err)
	}

	// And nothing is left behind. A store that accumulates half-written files
	// fills a disk quietly.
	leftovers, err := os.ReadDir(filepath.Join(root, "incoming"))
	if err != nil {
		t.Fatalf("read the incoming directory: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("%d temporary files were left behind", len(leftovers))
	}

	// Exactly at the limit is fine; the bound is a limit, not a margin.
	if _, err := store.PutBytes(context.Background(), bytes.Repeat([]byte("x"), 100), "text/plain"); err != nil {
		t.Errorf("content exactly at the limit was refused: %v", err)
	}
}

// Capturing the output of a command that will not stop should end when the run
// does, not when the disk fills.
func TestAnAbandonedPutStopsAndLeavesNothing(t *testing.T) {
	store, root := newStore(t, 1<<30)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.Put(ctx, neverEnding{}, "text/plain")
	if err == nil {
		t.Fatal("a cancelled put succeeded")
	}

	leftovers, err := os.ReadDir(filepath.Join(root, "incoming"))
	if err != nil {
		t.Fatalf("read the incoming directory: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("%d temporary files were left behind", len(leftovers))
	}
}

type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestStatReportsWhatIsThere(t *testing.T) {
	store, _ := newStore(t, 0)
	ref := put(t, store, "twelve bytes")

	found, err := store.Stat(ref.ID)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if found.Size != ref.Size {
		t.Errorf("stat says %d bytes, put said %d", found.Size, ref.Size)
	}
	if found.ID != ref.ID {
		t.Errorf("stat says %s, put said %s", found.ID, ref.ID)
	}
}
