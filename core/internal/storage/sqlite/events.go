package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// Append allocates the next sequence number and inserts the event in one
// transaction.
//
// Splitting those two steps is the classic way to corrupt an event log: a
// number handed out before its row is visible lets a subscriber read past it
// and never come back. BEGIN IMMEDIATE takes the write lock before the MAX
// query, so two concurrent appends serialize instead of racing to the same
// sequence.
func (s *Store) Append(ctx context.Context, event domain.Event) (domain.Seq, error) {
	payload, err := storage.EncodePayload(event.Payload)
	if err != nil {
		return 0, err
	}

	var seq domain.Seq
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sessions WHERE id = ?`, string(event.SessionID),
		).Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: check session: %w", err)
		}
		if exists == 0 {
			return storage.ErrSessionNotFound
		}

		var next int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = ?`, string(event.SessionID),
		).Scan(&next); err != nil {
			return fmt.Errorf("sqlite: allocate seq: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (session_id, seq, id, run_id, occurred_at, kind, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(event.SessionID), next, string(event.ID), string(event.RunID),
			event.OccurredAt.UnixNano(), string(event.Kind), string(payload),
		); err != nil {
			return fmt.Errorf("sqlite: insert event: %w", err)
		}

		seq = domain.Seq(next)
		return nil
	})
	if err != nil {
		return 0, err
	}

	return seq, nil
}

func (s *Store) ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error) {
	query := `SELECT session_id, seq, id, run_id, occurred_at, kind, payload
	          FROM events WHERE session_id = ? AND seq > ? ORDER BY seq`
	args := []any{string(id), int64(after)}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []domain.Event
	for rows.Next() {
		var (
			event    domain.Event
			seq      int64
			occurred int64
			payload  string
		)
		if err := rows.Scan(&event.SessionID, &seq, &event.ID, &event.RunID, &occurred, &event.Kind, &payload); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}

		decoded, err := storage.DecodePayload(event.Kind, []byte(payload))
		if err != nil {
			return nil, err
		}

		event.Seq = domain.Seq(seq)
		event.OccurredAt = timeFromNanos(occurred)
		event.Payload = decoded
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate events: %w", err)
	}

	// An empty result for an unknown session is indistinguishable from an
	// empty session, so callers that care are told explicitly.
	if len(events) == 0 {
		if _, err := s.Session(ctx, id); err != nil {
			return nil, err
		}
	}

	return events, nil
}

func (s *Store) Head(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	if _, err := s.Session(ctx, id); err != nil {
		return 0, err
	}

	var head int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?`, string(id),
	).Scan(&head); err != nil {
		return 0, fmt.Errorf("sqlite: head: %w", err)
	}

	return domain.Seq(head), nil
}

// Oldest is the earliest event still kept for a session.
func (s *Store) Oldest(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM events WHERE session_id = ?`, string(id)).Scan(&oldest)
	if err != nil {
		return 0, fmt.Errorf("sqlite: oldest event: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	return domain.Seq(oldest.Int64), nil
}

// PruneEvents discards everything at or below through.
//
// The caller decides what is safe to discard; this only does it. The rule that
// matters — never past the last compaction — belongs with the runtime, which
// is what knows how a conversation is rebuilt.
func (s *Store) PruneEvents(
	ctx context.Context, id domain.SessionID, through domain.Seq,
) (int64, error) {
	if through <= 0 {
		return 0, nil
	}

	result, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE session_id = ? AND seq <= ?`, string(id), int64(through))
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune events: %w", err)
	}

	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune events: %w", err)
	}
	return removed, nil
}
