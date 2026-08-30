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

		// The position in the whole log, taken under the same lock as the one
		// above. Two numbers from two transactions could interleave, and a
		// console reading them in order would then see an event it had
		// already passed.
		var global int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(global_seq), 0) + 1 FROM events`,
		).Scan(&global); err != nil {
			return fmt.Errorf("sqlite: allocate global seq: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (session_id, seq, global_seq, id, run_id, occurred_at, kind, payload)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			string(event.SessionID), next, global, string(event.ID), string(event.RunID),
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
	query := `SELECT session_id, seq, COALESCE(global_seq, 0), id, run_id, occurred_at, kind, payload
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

	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
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

	var removed int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The highest position about to go, read before the delete because
		// afterwards there is nothing left to ask. Raising the watermark in
		// the same transaction is what keeps it from ever being lower than
		// what is actually gone: a client resuming into the gap would then be
		// told nothing had happened.
		var highest int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(global_seq), 0) FROM events WHERE session_id = ? AND seq <= ?`,
			string(id), int64(through),
		).Scan(&highest); err != nil {
			return fmt.Errorf("sqlite: read what is about to be pruned: %w", err)
		}

		result, err := tx.ExecContext(ctx,
			`DELETE FROM events WHERE session_id = ? AND seq <= ?`, string(id), int64(through))
		if err != nil {
			return fmt.Errorf("sqlite: prune events: %w", err)
		}
		if removed, err = result.RowsAffected(); err != nil {
			return fmt.Errorf("sqlite: prune events: %w", err)
		}

		// Only ever upwards. Sessions are pruned one at a time and in no
		// particular order, so a later prune can be of older events, and
		// lowering the mark would claim back events that are still gone.
		if _, err := tx.ExecContext(ctx,
			`UPDATE log_watermark SET pruned_through = MAX(pruned_through, ?) WHERE id = 1`,
			highest,
		); err != nil {
			return fmt.Errorf("sqlite: raise the log watermark: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// ListAllAfter reads the whole log from a position in it.
//
// The counterpart of ListAfter for something watching every session at once.
// Ordered by the position it is asked for rather than by time: the two agree
// almost always, and where they disagree the append order is the one this
// program actually knows.
func (s *Store) ListAllAfter(ctx context.Context, after domain.Seq, limit int) ([]domain.Event, error) {
	query := `SELECT session_id, seq, COALESCE(global_seq, 0), id, run_id, occurred_at, kind, payload
	          FROM events WHERE global_seq > ? ORDER BY global_seq`
	args := []any{int64(after)}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list the log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanEvents(rows)
}

// LogHead is the position of the last event appended.
func (s *Store) LogHead(ctx context.Context) (domain.Seq, error) {
	var head int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(global_seq), 0) FROM events`,
	).Scan(&head); err != nil {
		return 0, fmt.Errorf("sqlite: read the log head: %w", err)
	}
	return domain.Seq(head), nil
}

// LogPrunedThrough is how far the log has been discarded.
//
// A client resuming from at or below this has missed events that are gone,
// which is a different answer from "nothing has happened since" and has to be
// told apart from it.
func (s *Store) LogPrunedThrough(ctx context.Context) (domain.Seq, error) {
	var through int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT pruned_through FROM log_watermark WHERE id = 1`,
	).Scan(&through); err != nil {
		return 0, fmt.Errorf("sqlite: read the log watermark: %w", err)
	}
	return domain.Seq(through), nil
}

// scanEvents reads a result set of events, whichever order it was asked for.
//
// The column list is the same for both, and one of two copies drifting from
// the other is a class of bug where the log reads back subtly wrong.
func scanEvents(rows *sql.Rows) ([]domain.Event, error) {
	var events []domain.Event
	for rows.Next() {
		var (
			event    domain.Event
			seq      int64
			global   int64
			occurred int64
			payload  string
		)
		if err := rows.Scan(&event.SessionID, &seq, &global, &event.ID, &event.RunID,
			&occurred, &event.Kind, &payload); err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}

		decoded, err := storage.DecodePayload(event.Kind, []byte(payload))
		if err != nil {
			return nil, err
		}

		event.Seq = domain.Seq(seq)
		event.GlobalSeq = domain.Seq(global)
		event.OccurredAt = timeFromNanos(occurred)
		event.Payload = decoded
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate events: %w", err)
	}
	return events, nil
}
