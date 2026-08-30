package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const approvalColumns = `id, session_id, run_id, tool_call_id, tool_name, arguments,
	summary, effects, preview, status, scope, created_at, decided_at, decided_by,
	read_foreign`

func (s *Store) CreateApproval(ctx context.Context, approval domain.Approval) error {
	effects, err := json.Marshal(approval.Effects)
	if err != nil {
		return fmt.Errorf("sqlite: encode effects: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO approvals (`+approvalColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(approval.ID), string(approval.SessionID), string(approval.RunID),
		string(approval.ToolCallID), approval.ToolName, approval.Arguments,
		approval.Summary, string(effects), approval.Preview,
		string(approval.Status), string(approval.Scope),
		approval.CreatedAt.UnixNano(), nullableTime(approval.DecidedAt), storedOrigin{approval.DecidedBy},
		approval.ReadForeign,
	)
	if isUniqueViolation(err) {
		return storage.ErrApprovalDecided
	}
	if err != nil {
		return fmt.Errorf("sqlite: create approval: %w", err)
	}
	return nil
}

func (s *Store) Approval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, string(id))

	approval, err := scanApproval(row)
	if err != nil {
		return domain.Approval{}, wrapNotFound(err, storage.ErrApprovalNotFound)
	}
	return approval, nil
}

// DecideApproval settles a pending approval.
//
// The status check is in the UPDATE rather than in a preceding SELECT: two
// clients answering the same prompt at the same moment would both pass a
// read-then-write check, and the tool would run twice.
func (s *Store) DecideApproval(
	ctx context.Context,
	id domain.ApprovalID,
	status domain.ApprovalStatus,
	scope domain.RememberScope,
	decidedBy domain.RunOrigin,
	at time.Time,
) (domain.Approval, error) {
	var approval domain.Approval

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE approvals SET status = ?, scope = ?, decided_by = ?, decided_at = ?
			 WHERE id = ? AND status = ?`,
			string(status), string(scope), storedOrigin{decidedBy}, at.UnixNano(),
			string(id), string(domain.ApprovalPending),
		)
		if err != nil {
			return fmt.Errorf("sqlite: decide approval: %w", err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: decide approval: %w", err)
		}
		if affected == 0 {
			// Either it does not exist or someone already answered. The two
			// are distinguished so a client can tell a typo from a race.
			var exists int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM approvals WHERE id = ?`, string(id),
			).Scan(&exists); err != nil {
				return fmt.Errorf("sqlite: check approval: %w", err)
			}
			if exists == 0 {
				return storage.ErrApprovalNotFound
			}
			return storage.ErrApprovalDecided
		}

		row := tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, string(id))
		approval, err = scanApproval(row)
		return err
	})
	if err != nil {
		return domain.Approval{}, err
	}

	return approval, nil
}

func (s *Store) PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalColumns+` FROM approvals
		 WHERE session_id = ? AND status = ? ORDER BY created_at, id`,
		string(session), string(domain.ApprovalPending),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var approvals []domain.Approval
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan approval: %w", err)
		}
		approvals = append(approvals, approval)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate approvals: %w", err)
	}

	return approvals, nil
}

func (s *Store) ApprovalForCall(ctx context.Context, run domain.RunID, call domain.ToolCallID) (domain.Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE run_id = ? AND tool_call_id = ?`,
		string(run), string(call),
	)

	approval, err := scanApproval(row)
	if err != nil {
		return domain.Approval{}, wrapNotFound(err, storage.ErrApprovalNotFound)
	}
	return approval, nil
}

func scanApproval(row scanner) (domain.Approval, error) {
	var (
		approval   domain.Approval
		effectsRaw string
		created    int64
		decided    sql.NullInt64
		decidedBy  storedOrigin
	)

	if err := row.Scan(
		&approval.ID, &approval.SessionID, &approval.RunID,
		&approval.ToolCallID, &approval.ToolName, &approval.Arguments,
		&approval.Summary, &effectsRaw, &approval.Preview, &approval.Status, &approval.Scope,
		&created, &decided, &decidedBy, &approval.ReadForeign,
	); err != nil {
		return domain.Approval{}, err
	}

	approval.DecidedBy = decidedBy.RunOrigin

	if effectsRaw != "" {
		if err := json.Unmarshal([]byte(effectsRaw), &approval.Effects); err != nil {
			return domain.Approval{}, fmt.Errorf("sqlite: decode effects: %w", err)
		}
	}

	approval.CreatedAt = timeFromNanos(created)
	if decided.Valid {
		at := timeFromNanos(decided.Int64)
		approval.DecidedAt = &at
	}

	return approval, nil
}
