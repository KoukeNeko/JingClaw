package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

type planItemRow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// Plan is what the agent said it was going to do in this session.
//
// A session with no plan is not an error: most sessions never make one, and
// an empty list is the honest answer.
func (s *Store) Plan(ctx context.Context, session domain.SessionID) ([]domain.PlanItem, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT items FROM session_plans WHERE session_id = ?`, string(session)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read plan: %w", err)
	}

	var rows []planItemRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("sqlite: decode plan: %w", err)
	}

	items := make([]domain.PlanItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.PlanItem{
			ID: row.ID, Title: row.Title,
			Status: domain.PlanStatus(row.Status), Note: row.Note,
		})
	}
	return items, nil
}

// SetPlan replaces the plan for a session.
func (s *Store) SetPlan(
	ctx context.Context,
	session domain.SessionID,
	items []domain.PlanItem,
	at time.Time,
) error {
	rows := make([]planItemRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, planItemRow{
			ID: item.ID, Title: item.Title,
			Status: string(item.Status), Note: item.Note,
		})
	}

	encoded, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("sqlite: encode plan: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO session_plans (session_id, items, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET items = excluded.items, updated_at = excluded.updated_at`,
		string(session), string(encoded), at.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlite: write plan: %w", err)
	}
	return nil
}
