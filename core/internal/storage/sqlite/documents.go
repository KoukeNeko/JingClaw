package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// The two column lists are in the same order and shape, so one scan reads a
// document and a version of one.
const (
	documentColumns = `name, text, hash, version, source, updated_at`
	versionColumns  = `name, text, hash, version, source, saved_at`
)

func (s *Store) Document(ctx context.Context, name string) (storage.Document, error) {
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	doc, err := scanDocument(s.db.QueryRowContext(ctx,
		`SELECT `+documentColumns+` FROM documents WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Document{Name: name}, nil
	}
	if err != nil {
		return storage.Document{}, fmt.Errorf("sqlite: read document: %w", err)
	}
	return doc, nil
}

// putDocument is the whole compare-and-set in one statement.
//
// The guard reads the hash as it is now, an absent row counting as "", and
// compares it to what the edit started from. When it holds, the insert either
// creates the row or finds it and bumps its version; when it does not, nothing
// is inserted and RETURNING yields no row. A statement that writes takes
// SQLite's write lock before it reads anything, so nobody can change the row
// between the check and the write — not another goroutine, and not another
// process. Reading first and updating after would leave that gap.
//
// The guard is in the SELECT rather than in a WHERE on the update half
// because an absent row has no update half: with expect "" the insert has to
// go through, and with anything else it has to be refused.
const putDocument = `
INSERT INTO documents (name, text, hash, version, source, updated_at)
SELECT ?, ?, ?, 1, ?, ?
WHERE COALESCE((SELECT hash FROM documents WHERE name = ?), '') = ?
ON CONFLICT (name) DO UPDATE SET
	text = excluded.text, hash = excluded.hash, version = documents.version + 1,
	source = excluded.source, updated_at = excluded.updated_at
RETURNING version`

func (s *Store) PutDocument(
	ctx context.Context, name, text, source, expect string, at time.Time,
) (storage.Document, error) {
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	doc := storage.Document{
		Name: name, Text: text, Hash: storage.DocumentHash(text),
		Source: source, UpdatedAt: timeFromNanos(at.UnixNano()),
	}

	// The write is the first statement of the transaction, so it takes the
	// write lock itself and has no earlier read whose snapshot another
	// process could have made stale. The history row goes in with it: a
	// document whose newest version is not in its history would not be
	// restorable to the text it has.
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, putDocument,
			name, text, doc.Hash, source, doc.UpdatedAt.UnixNano(), name, expect,
		).Scan(&doc.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrDocumentChanged
		}
		if err != nil {
			return fmt.Errorf("sqlite: write document: %w", err)
		}
		return keepVersion(ctx, tx, doc)
	})
	if err != nil {
		return storage.Document{}, err
	}
	return doc, nil
}

// keepVersion adds a version to the history and discards what is past
// DocumentVersionsKept, in the caller's transaction.
//
// What survives is chosen by version rather than by arithmetic on the new
// one, so a gap in the numbers cannot make it keep more or fewer than asked.
func keepVersion(ctx context.Context, tx *sql.Tx, doc storage.Document) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO document_versions (`+versionColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		doc.Name, doc.Text, doc.Hash, doc.Version, doc.Source, doc.UpdatedAt.UnixNano(),
	); err != nil {
		return fmt.Errorf("sqlite: keep document version: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM document_versions WHERE name = ? AND version NOT IN (
			SELECT version FROM document_versions WHERE name = ?
			ORDER BY version DESC LIMIT ?)`,
		doc.Name, doc.Name, storage.DocumentVersionsKept,
	); err != nil {
		return fmt.Errorf("sqlite: prune document versions: %w", err)
	}
	return nil
}

func (s *Store) DocumentVersions(ctx context.Context, name string) ([]storage.Document, error) {
	if err := storage.CheckDocumentName(name); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+versionColumns+` FROM document_versions WHERE name = ? ORDER BY version DESC`,
		name)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list document versions: %w", err)
	}
	defer rows.Close()

	var versions []storage.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list document versions: %w", err)
		}
		versions = append(versions, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list document versions: %w", err)
	}
	return versions, nil
}

func (s *Store) DocumentVersion(
	ctx context.Context, name string, version int64,
) (storage.Document, error) {
	if err := storage.CheckDocumentName(name); err != nil {
		return storage.Document{}, err
	}

	doc, err := scanDocument(s.db.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM document_versions WHERE name = ? AND version = ?`,
		name, version))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.Document{}, storage.ErrDocumentVersionNotFound
		}
		return storage.Document{}, fmt.Errorf("sqlite: read document version: %w", err)
	}
	return doc, nil
}

// SeedDocuments creates the documents that are missing, all in one
// transaction.
//
// ON CONFLICT DO NOTHING is what makes it never overwrite, and the rows
// affected say which seeds actually took. A failure anywhere rolls back
// every one of them.
func (s *Store) SeedDocuments(
	ctx context.Context, seeds []storage.DocumentSeed, at time.Time,
) ([]string, error) {
	var written []string

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		written = nil
		for _, seed := range seeds {
			if err := storage.CheckDocumentName(seed.Name); err != nil {
				return err
			}

			doc := storage.Document{
				Name: seed.Name, Text: seed.Text, Hash: storage.DocumentHash(seed.Text),
				Source: seed.Source, Version: 1, UpdatedAt: timeFromNanos(at.UnixNano()),
			}
			result, err := tx.ExecContext(ctx,
				`INSERT INTO documents (`+documentColumns+`) VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT (name) DO NOTHING`,
				doc.Name, doc.Text, doc.Hash, doc.Version, doc.Source, doc.UpdatedAt.UnixNano(),
			)
			if err != nil {
				return fmt.Errorf("sqlite: seed document: %w", err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("sqlite: seed document: %w", err)
			}
			if affected == 0 {
				continue
			}

			if err := keepVersion(ctx, tx, doc); err != nil {
				return err
			}
			written = append(written, doc.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Seeding is followed by something that cannot be undone: the file a
	// document came from is set aside. Under WAL with synchronous NORMAL a
	// commit is in the log and not yet on disk, and a power cut after the
	// rename and before the log is synced would lose the document and keep the
	// rename. Checkpointing writes the log into the database file and syncs
	// both. Saves from the dashboard are not paired with anything, and are as
	// durable as every other write here.
	if len(written) > 0 {
		if err := s.checkpoint(ctx); err != nil {
			return nil, err
		}
	}
	return written, nil
}

// checkpoint writes the log into the database file and syncs both.
func (s *Store) checkpoint(ctx context.Context) error {
	var busy, logged, moved int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logged, &moved); err != nil {
		return fmt.Errorf("sqlite: sync the log: %w", err)
	}
	if busy != 0 {
		return errors.New("sqlite: sync the log: the database is busy")
	}
	return nil
}

func scanDocument(row scanner) (storage.Document, error) {
	var (
		doc     storage.Document
		updated int64
	)
	if err := row.Scan(&doc.Name, &doc.Text, &doc.Hash, &doc.Version, &doc.Source, &updated); err != nil {
		return storage.Document{}, err
	}
	doc.UpdatedAt = timeFromNanos(updated)
	return doc, nil
}
