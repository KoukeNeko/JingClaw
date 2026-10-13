// Package sqlite implements storage.Store on SQLite.
//
// The driver is modernc.org/sqlite, a pure-Go translation, so the daemon
// builds with CGO_ENABLED=0 on every target. That matters for a tool meant to
// be dropped onto a Linux box as a single binary.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/fsperm"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is a SQLite-backed storage.Store.
type Store struct {
	db *sql.DB
}

var _ storage.Store = (*Store)(nil)

// Open opens (or creates) the database at path and applies migrations.
// Pass ":memory:" for an ephemeral database in tests.
//
// A database on disk is readable by its owner alone, new or not: it holds the
// conversations and memories, and the daemon's settings, which can carry
// credentials such as the headers sent to a tool server.
func Open(ctx context.Context, path string) (*Store, error) {
	if !isMemory(path) {
		if err := restrictDatabase(path); err != nil {
			return nil, err
		}
	}

	dsn := path + pragmaSuffix(path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	// A single writer avoids SQLITE_BUSY entirely: the driver serializes
	// writes instead of having callers retry a lock they were never going to
	// win. Reads still run concurrently thanks to WAL.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.indexMemories(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

// restrictDatabase makes the database file, and the write-ahead log and index
// beside it, owner-only.
//
// The file is created before SQLite opens it because SQLite makes the -wal and
// -shm files with the mode of the database, so creating it owner-only is what
// makes them owner-only. Where they already exist — left by a daemon that was
// killed rather than stopped — they keep the mode they had, and the daemon is
// about to append to them, so they are tightened here too.
func restrictDatabase(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sqlite: create %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("sqlite: create %s: %w", path, err)
	}

	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := fsperm.Restrict(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("sqlite: %w", err)
		}
	}
	return nil
}

// isMemory is a database that is not a file, which has nothing to protect and
// nothing to create.
func isMemory(path string) bool {
	return strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory")
}

func pragmaSuffix(path string) string {
	pragmas := []string{
		// Readers never block the writer, which is what lets an event stream
		// keep serving while a run appends.
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(ON)",
		"_pragma=busy_timeout(5000)",
		// Durable enough under WAL: a crash can lose the last transaction but
		// cannot corrupt the database.
		"_pragma=synchronous(NORMAL)",
	}

	// WAL needs a real file; an in-memory database rejects it.
	if isMemory(path) {
		pragmas = pragmas[1:]
	}

	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return separator + strings.Join(pragmas, "&")
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`,
	); err != nil {
		return fmt.Errorf("sqlite: create migrations table: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("sqlite: list migrations: %w", err)
	}
	// Filenames are zero-padded, so lexical order is application order.
	sort.Strings(entries)

	for _, entry := range entries {
		var applied int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, entry,
		).Scan(&applied); err != nil {
			return fmt.Errorf("sqlite: check migration %s: %w", entry, err)
		}
		if applied > 0 {
			continue
		}

		script, err := migrationFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", entry, err)
		}

		// Each migration and its bookkeeping row commit together, so a crash
		// mid-migration cannot leave the schema half-applied yet recorded.
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return fmt.Errorf("sqlite: apply migration %s: %w", entry, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
				entry, time.Now().UnixNano(),
			); err != nil {
				return fmt.Errorf("sqlite: record migration %s: %w", entry, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

// inTx runs fn inside one transaction.
//
// The transaction is deferred, which is the driver's default and what this
// DSN leaves it at: the write lock is taken by the first statement that
// writes, not at BEGIN. Within this process that is enough for a read followed
// by a write, such as sequence allocation, because the pool has one
// connection and a transaction holds it. It does not exclude another process
// on the same file: a transaction that reads before it writes can fail with
// SQLITE_BUSY if that process committed in between. Code that has to hold
// across processes starts its transaction with the write, as PutDocument does.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func timeFromNanos(nanos int64) time.Time {
	return time.Unix(0, nanos).UTC()
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports constraint failures in the message rather
	// than through a typed error, so matching on it is the available option.
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

func wrapNotFound(err error, notFound error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return notFound
	}
	return err
}

var _ = domain.SessionID("")
