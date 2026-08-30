package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const runColumns = `id, session_id, status, origin, delivery_targets, created_at, finished_at,
	kind, parent_run_id`

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	origin, err := storage.EncodeOrigin(run.Origin)
	if err != nil {
		return err
	}
	targets, err := storage.EncodeDeliveryTargets(run.DeliveryTargets)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (`+runColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(run.ID), string(run.SessionID), string(run.Status),
		string(origin), string(targets),
		run.CreatedAt.UnixNano(), nullableTime(run.FinishedAt),
		string(run.Kind), string(run.ParentRunID),
	)
	if isUniqueViolation(err) {
		return storage.ErrDuplicateRun
	}
	if err != nil {
		return fmt.Errorf("sqlite: create run: %w", err)
	}
	return nil
}

// UpdateRun persists the mutable part of a run. Identity, session, origin and
// delivery targets are fixed at creation, so they are deliberately not
// updatable here.
func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
		string(run.Status), nullableTime(run.FinishedAt), string(run.ID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: update run: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update run: %w", err)
	}
	if affected == 0 {
		return storage.ErrRunNotFound
	}
	return nil
}

func (s *Store) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, string(id))

	run, err := scanRun(row)
	if err != nil {
		return domain.Run{}, wrapNotFound(err, storage.ErrRunNotFound)
	}
	return run, nil
}

func (s *Store) ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE session_id = ? ORDER BY created_at, id`,
		string(session),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanRuns(rows)
}

// UnfinishedRuns finds runs that were still live when the process last
// stopped. Nothing is driving them any more, so the caller must resolve them;
// leaving them alone would strand every client on a run that never ends.
func (s *Store) UnfinishedRuns(ctx context.Context) ([]domain.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs
		 WHERE status NOT IN ('completed', 'cancelled', 'failed')
		 ORDER BY created_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list unfinished runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanRuns(rows)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(row scanner) (domain.Run, error) {
	var (
		run        domain.Run
		originRaw  string
		targetsRaw string
		created    int64
		finished   sql.NullInt64
	)

	if err := row.Scan(&run.ID, &run.SessionID, &run.Status, &originRaw, &targetsRaw,
		&created, &finished, &run.Kind, &run.ParentRunID); err != nil {
		return domain.Run{}, err
	}

	origin, err := storage.DecodeOrigin([]byte(originRaw))
	if err != nil {
		return domain.Run{}, err
	}
	targets, err := storage.DecodeDeliveryTargets([]byte(targetsRaw))
	if err != nil {
		return domain.Run{}, err
	}

	run.Origin = origin
	run.DeliveryTargets = targets
	run.CreatedAt = timeFromNanos(created)
	if finished.Valid {
		at := timeFromNanos(finished.Int64)
		run.FinishedAt = &at
	}

	return run, nil
}

func scanRuns(rows *sql.Rows) ([]domain.Run, error) {
	var runs []domain.Run

	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan run: %w", err)
		}
		runs = append(runs, run)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate runs: %w", err)
	}
	return runs, nil
}
