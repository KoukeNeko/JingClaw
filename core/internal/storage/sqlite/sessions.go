package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

func (s *Store) CreateSession(ctx context.Context, session domain.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		string(session.ID), session.Title, session.Model,
		session.CreatedAt.UnixNano(), session.UpdatedAt.UnixNano(),
	)
	if isUniqueViolation(err) {
		return storage.ErrDuplicateSession
	}
	if err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	return nil
}

func (s *Store) Session(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	var (
		session          domain.Session
		created, updated int64
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, model, created_at, updated_at FROM sessions WHERE id = ?`,
		string(id),
	).Scan(&session.ID, &session.Title, &session.Model, &created, &updated)
	if err != nil {
		return domain.Session{}, wrapNotFound(err, storage.ErrSessionNotFound)
	}

	session.CreatedAt = timeFromNanos(created)
	session.UpdatedAt = timeFromNanos(updated)
	return session, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]domain.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, model, created_at, updated_at FROM sessions ORDER BY created_at DESC, id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSessions(rows)
}

func scanSessions(rows *sql.Rows) ([]domain.Session, error) {
	var sessions []domain.Session

	for rows.Next() {
		var (
			session          domain.Session
			created, updated int64
		)
		if err := rows.Scan(&session.ID, &session.Title, &session.Model, &created, &updated); err != nil {
			return nil, fmt.Errorf("sqlite: scan session: %w", err)
		}

		session.CreatedAt = timeFromNanos(created)
		session.UpdatedAt = timeFromNanos(updated)
		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate sessions: %w", err)
	}
	return sessions, nil
}

// SetSessionModel changes which model answers in a session.
//
// Recorded on the session rather than applied to the run in flight: a run
// already generating has a request open with a model in it, and changing that
// underneath would produce a turn attributed to a model that did not write it.
// The next run picks it up.
func (s *Store) SetSessionModel(ctx context.Context, id domain.SessionID, model string, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET model = ?, updated_at = ? WHERE id = ?`,
		model, at.UnixNano(), string(id))
	if err != nil {
		return fmt.Errorf("sqlite: set session model: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: set session model: %w", err)
	}
	if affected == 0 {
		return storage.ErrSessionNotFound
	}
	return nil
}
