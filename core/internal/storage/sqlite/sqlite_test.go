package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/storagetest"
)

// Tests run against a real file rather than :memory: so that WAL, the busy
// timeout and transaction behaviour are the ones the daemon actually uses.
func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
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
