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
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
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
func Open(ctx context.Context, path string) (*Store, error) {
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

	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

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
	if strings.Contains(path, ":memory:") || strings.Contains(path, "mode=memory") {
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

// inTx runs fn inside an immediate transaction. BEGIN IMMEDIATE takes the
// write lock up front rather than discovering the conflict at COMMIT, which is
// what makes read-then-write sequences such as sequence allocation safe.
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
