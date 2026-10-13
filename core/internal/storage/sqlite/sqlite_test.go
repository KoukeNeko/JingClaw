package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm/permtest"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/storagetest"
)

// Tests run against a real file rather than :memory: so that WAL, the busy
// timeout and transaction behaviour are the ones the daemon actually uses.
func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()

	return openStoreAt(t, filepath.Join(t.TempDir(), "test.db"))
}

func openStoreAt(t *testing.T, path string) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		return openTestStore(t)
	})
}

// Two stores on one file are what two daemons are to the database: separate
// connections that share nothing but the file, so the only thing that can keep
// them from both winning is the database itself. The conformance suite runs
// through a single store, whose one connection would serialize them anyway.
func TestTwoStoresOnOneFileLetOneEditorWinPerHash(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "shared.db")
	stores := []*sqlite.Store{openStoreAt(t, path), openStoreAt(t, path)}

	var current storage.Document
	for round := range 5 {
		start, err := stores[0].PutDocument(ctx, "config", fmt.Sprintf("round %d", round), "web", current.Hash, at)
		if err != nil {
			t.Fatalf("round %d: start: %v", round, err)
		}

		const editors = 8
		results := make(chan error, editors)
		var wg sync.WaitGroup
		for i := range editors {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := stores[i%2].PutDocument(ctx, "config", fmt.Sprintf("editor %d", i), "web", start.Hash, at)
				results <- err
			}()
		}
		wg.Wait()
		close(results)

		var won int
		for err := range results {
			switch {
			case err == nil:
				won++
			case errors.Is(err, storage.ErrDocumentChanged):
			default:
				// A locked database here is the failure this test exists for:
				// a write that was not a single statement.
				t.Errorf("round %d: an editor failed for another reason: %v", round, err)
			}
		}
		if won != 1 {
			t.Errorf("round %d: %d editors won, want 1", round, won)
		}

		// Read through the other store, which did not make the first write.
		if current, err = stores[1].Document(ctx, "config"); err != nil {
			t.Fatalf("round %d: read back: %v", round, err)
		}
	}

	if current.Version != 10 {
		t.Errorf("version %d after five rounds of a start and a winner, want 10", current.Version)
	}
}

// What a start does next to a seed cannot be undone: the file it came from is
// set aside. So the seed is on disk, in the database file itself and not only in
// the log beside it, before SeedDocuments returns. A copy of the file alone is
// what a power cut leaves, and it has to hold the documents.
func TestSeededDocumentsAreInTheDatabaseFileWhenSeedingReturns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "live.db")
	store := openStoreAt(t, path)

	if _, err := store.SeedDocuments(ctx, []storage.DocumentSeed{
		{Name: "config", Text: "imported\n", Source: "file"},
	}, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The file alone, not the -wal beside it, taken while the store is still open.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	alone := filepath.Join(t.TempDir(), "alone.db")
	if err := os.WriteFile(alone, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := openStoreAt(t, alone).Document(ctx, "config")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != "imported\n" || doc.Version != 1 {
		t.Errorf("the database file alone holds %q at version %d, want what was seeded", doc.Text, doc.Version)
	}
}

// requireOwnerOnly fails unless the file exists and is closed to everyone but
// its owner. A file that is not there fails too: these checks would otherwise
// pass for a store that never made the log they are meant to cover.
func requireOwnerOnly(t *testing.T, path string) {
	t.Helper()

	ownerOnly, detail, err := fsperm.EnsureOwnerOnly(path)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !ownerOnly {
		t.Errorf("%s is readable by others: %s", filepath.Base(path), detail)
	}
}

// File modes are what this is about, and Windows has none. The same guarantee
// there is an access control list that SQLite's log files do not inherit from
// the database, so what these tests check does not carry over.
func skipWithoutFileModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("owner-only files are an access control list on Windows, not a mode")
	}
}

// The database holds the daemon's settings, credentials for tool servers
// included, so a new one is closed to other accounts from the first byte —
// and so are the log and index SQLite makes beside it, which carry the same
// data until they are merged back.
func TestANewDatabaseAndItsLogAreReadableByTheOwnerOnly(t *testing.T) {
	skipWithoutFileModes(t)

	path := filepath.Join(t.TempDir(), "new.db")
	openStoreAt(t, path)

	// Opening applied the migrations, so the store is open with a log behind it.
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		requireOwnerOnly(t, file)
	}
}

// A database made before this one was closed is tightened when it is opened,
// log and index included: the daemon appends to the log it finds, so a log
// left readable by a daemon that was killed would take the new data too.
func TestOpeningTightensADatabaseAndItsLogThatWereLeftReadable(t *testing.T) {
	skipWithoutFileModes(t)

	path := filepath.Join(t.TempDir(), "old.db")
	// Kept open so that its log and index are there to be left readable, as a
	// daemon that was killed leaves them.
	openStoreAt(t, path)

	files := []string{path, path + "-wal", path + "-shm"}
	for _, file := range files {
		if err := permtest.Expose(file); err != nil {
			t.Fatalf("expose: %v", err)
		}
		if ownerOnly, _, err := fsperm.EnsureOwnerOnly(file); err != nil || ownerOnly {
			t.Fatalf("%s is still closed after exposing it (err %v); the test would prove nothing", filepath.Base(file), err)
		}
	}

	openStoreAt(t, path)

	for _, file := range files {
		requireOwnerOnly(t, file)
	}
}
