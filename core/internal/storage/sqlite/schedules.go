package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const scheduleColumns = `id, revision, expression, zone, prompt, session_id,
	created_by, deliver, missed_policy, paused, created_at`

func (s *Store) CreateSchedule(ctx context.Context, schedule domain.Schedule) error {
	createdBy, err := storage.EncodeOrigin(schedule.CreatedBy)
	if err != nil {
		return err
	}
	deliver, err := storage.EncodeDeliveryTargets(schedule.Deliver)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO schedules (`+scheduleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(schedule.ID), schedule.Revision, schedule.Expression, schedule.Zone,
		schedule.Prompt, string(schedule.SessionID),
		string(createdBy), string(deliver),
		string(schedule.Missed), schedule.Paused, schedule.CreatedAt.UnixNano(),
	)
	if isUniqueViolation(err) {
		return storage.ErrDuplicateSchedule
	}
	if err != nil {
		return fmt.Errorf("sqlite: create schedule: %w", err)
	}
	return nil
}

// UpdateSchedule replaces what a schedule does, and counts the change.
//
// The revision goes up because it is part of a firing's identity. Without
// that, editing "every hour" to "every day" would leave the hourly firings
// already resolved looking like they belonged to the new instruction — or
// worse, unresolved.
func (s *Store) UpdateSchedule(ctx context.Context, schedule domain.Schedule) error {
	deliver, err := storage.EncodeDeliveryTargets(schedule.Deliver)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET revision = revision + 1, expression = ?, zone = ?,
			prompt = ?, deliver = ?, missed_policy = ?, paused = ? WHERE id = ?`,
		schedule.Expression, schedule.Zone, schedule.Prompt,
		string(deliver), string(schedule.Missed), schedule.Paused, string(schedule.ID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: update schedule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return storage.ErrScheduleNotFound
	}
	return nil
}

// SetSchedulePaused stops or resumes one without counting as a change.
//
// Separate from UpdateSchedule on purpose: pausing does not alter what the
// schedule means, so it must not bump the revision and orphan the firings
// already resolved under it.
func (s *Store) SetSchedulePaused(ctx context.Context, id domain.ScheduleID, paused bool) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET paused = ? WHERE id = ?`, paused, string(id))
	if err != nil {
		return fmt.Errorf("sqlite: pause schedule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return storage.ErrScheduleNotFound
	}
	return nil
}

func (s *Store) Schedule(ctx context.Context, id domain.ScheduleID) (domain.Schedule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, string(id))

	schedule, err := scanSchedule(row)
	if err != nil {
		return domain.Schedule{}, wrapNotFound(err, storage.ErrScheduleNotFound)
	}
	return schedule, nil
}

// ListSchedules is every schedule, paused ones included.
//
// Paused ones included because the commonest reason to list is to find the
// one to turn back on, and a listing that hid them would make that
// impossible from the command that shows them.
func (s *Store) ListSchedules(ctx context.Context) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list schedules: %w", err)
	}
	defer rows.Close()

	var schedules []domain.Schedule
	for rows.Next() {
		schedule, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list schedules: %w", err)
	}
	return schedules, nil
}

func (s *Store) DeleteSchedule(ctx context.Context, id domain.ScheduleID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("sqlite: delete schedule: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return storage.ErrScheduleNotFound
	}
	return nil
}

// ResolveFiring records that an occasion has been accounted for.
//
// The insert is what makes reconciling idempotent, so a second attempt at the
// same occasion is not an error to report but the answer to the question
// being asked: somebody already did this. A daemon that restarts twice in a
// minute must not produce two runs for one three o'clock.
func (s *Store) ResolveFiring(ctx context.Context, firing domain.Firing) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schedule_firings
			(schedule_id, revision, due_at, observed_at, missed, run_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(firing.ScheduleID), firing.Revision,
		firing.For.UnixNano(), firing.Observed.UnixNano(),
		firing.Missed, string(firing.RunID),
	)
	if isUniqueViolation(err) {
		return storage.ErrFiringAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("sqlite: resolve firing: %w", err)
	}
	return nil
}

// RecordFiringRun links an occasion to the run it became.
func (s *Store) RecordFiringRun(ctx context.Context, firing domain.Firing) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE schedule_firings SET run_id = ?
		 WHERE schedule_id = ? AND revision = ? AND due_at = ?`,
		string(firing.RunID), string(firing.ScheduleID), firing.Revision,
		firing.For.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: record firing run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return storage.ErrFiringNotResolved
	}
	return nil
}

// LastFiring is when this schedule was last accounted for, at this revision.
//
// At this revision: a schedule that changed is a different instruction, and
// asking what the old one last did would let an edit skip the first firing of
// the new one.
func (s *Store) LastFiring(
	ctx context.Context, id domain.ScheduleID, revision int,
) (time.Time, error) {
	var due sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(due_at) FROM schedule_firings WHERE schedule_id = ? AND revision = ?`,
		string(id), revision).Scan(&due)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: last firing: %w", err)
	}
	if !due.Valid {
		return time.Time{}, nil
	}
	return timeFromNanos(due.Int64), nil
}

func scanSchedule(row scanner) (domain.Schedule, error) {
	var (
		schedule  domain.Schedule
		createdBy string
		deliver   string
		created   int64
	)

	err := row.Scan(
		&schedule.ID, &schedule.Revision, &schedule.Expression, &schedule.Zone,
		&schedule.Prompt, &schedule.SessionID,
		&createdBy, &deliver, &schedule.Missed, &schedule.Paused, &created,
	)
	if err != nil {
		return domain.Schedule{}, err
	}

	if schedule.CreatedBy, err = storage.DecodeOrigin([]byte(createdBy)); err != nil {
		return domain.Schedule{}, err
	}
	if schedule.Deliver, err = storage.DecodeDeliveryTargets([]byte(deliver)); err != nil {
		return domain.Schedule{}, err
	}
	schedule.CreatedAt = timeFromNanos(created)

	return schedule, nil
}
