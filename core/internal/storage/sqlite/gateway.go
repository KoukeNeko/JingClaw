package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
)

var _ gateway.Store = (*Store)(nil)

const bindingColumns = `id, platform, account_id, tenant_id, channel_id,
	workspace_id, permission_profile, allowed_principals, allowed_claims, created_at`

func (s *Store) UpsertBinding(ctx context.Context, binding gateway.Binding) error {
	principals, err := json.Marshal(binding.AllowedPrincipals)
	if err != nil {
		return fmt.Errorf("sqlite: encode principals: %w", err)
	}
	claims, err := json.Marshal(binding.AllowedClaims)
	if err != nil {
		return fmt.Errorf("sqlite: encode claims: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO gateway_bindings (`+bindingColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (platform, account_id, tenant_id, channel_id) DO UPDATE SET
		   workspace_id = excluded.workspace_id,
		   permission_profile = excluded.permission_profile,
		   allowed_principals = excluded.allowed_principals,
		   allowed_claims = excluded.allowed_claims`,
		binding.ID, string(binding.Platform), binding.AccountID, binding.TenantID, binding.ChannelID,
		binding.WorkspaceID, binding.PermissionProfile, string(principals), string(claims),
		binding.CreatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: upsert binding: %w", err)
	}
	return nil
}

func (s *Store) Binding(
	ctx context.Context,
	platform gateway.Platform,
	accountID, tenantID, channelID string,
) (gateway.Binding, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM gateway_bindings
		 WHERE platform = ? AND account_id = ? AND tenant_id = ? AND channel_id = ?`,
		string(platform), accountID, tenantID, channelID,
	)

	binding, err := scanBinding(row)
	if err != nil {
		return gateway.Binding{}, wrapNotFound(err, gateway.ErrBindingNotFound)
	}
	return binding, nil
}

func (s *Store) ListBindings(ctx context.Context) ([]gateway.Binding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+bindingColumns+` FROM gateway_bindings ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var bindings []gateway.Binding
	for rows.Next() {
		binding, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan binding: %w", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate bindings: %w", err)
	}
	return bindings, nil
}

func (s *Store) DeleteBinding(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_bindings WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete binding: %w", err)
	}
	return nil
}

func scanBinding(row scanner) (gateway.Binding, error) {
	var (
		binding       gateway.Binding
		platform      string
		principalsRaw string
		claimsRaw     string
		created       int64
	)

	if err := row.Scan(&binding.ID, &platform, &binding.AccountID, &binding.TenantID,
		&binding.ChannelID, &binding.WorkspaceID, &binding.PermissionProfile,
		&principalsRaw, &claimsRaw, &created); err != nil {
		return gateway.Binding{}, err
	}

	binding.Platform = gateway.Platform(platform)
	binding.CreatedAt = timeFromNanos(created)

	if err := json.Unmarshal([]byte(principalsRaw), &binding.AllowedPrincipals); err != nil {
		return gateway.Binding{}, fmt.Errorf("sqlite: decode principals: %w", err)
	}
	if err := json.Unmarshal([]byte(claimsRaw), &binding.AllowedClaims); err != nil {
		return gateway.Binding{}, fmt.Errorf("sqlite: decode claims: %w", err)
	}

	return binding, nil
}

// LinkConversation fails rather than overwrites, so two messages arriving at
// once cannot each create a session for the same thread.
func (s *Store) LinkConversation(
	ctx context.Context,
	key string,
	session domain.SessionID,
	bindingID string,
	at time.Time,
) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_conversations (conversation_key, session_id, binding_id, created_at)
		 VALUES (?, ?, ?, ?)`,
		key, string(session), bindingID, at.UnixNano(),
	)
	if isUniqueViolation(err) {
		return gateway.ErrAlreadyProcessed
	}
	if err != nil {
		return fmt.Errorf("sqlite: link conversation: %w", err)
	}
	return nil
}

func (s *Store) SessionForConversation(ctx context.Context, key string) (domain.SessionID, bool, error) {
	var session string

	err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM gateway_conversations WHERE conversation_key = ?`, key,
	).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: session for conversation: %w", err)
	}

	return domain.SessionID(session), true, nil
}

// ClaimInbound records a message as handled, reporting whether this was the
// first time. A platform redelivering after a reconnect must not produce a
// second run doing the same work.
func (s *Store) ClaimInbound(
	ctx context.Context,
	key, accountID string,
	session domain.SessionID,
	run domain.RunID,
	at time.Time,
) (bool, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_inbound (idempotency_key, account_id, session_id, run_id, received_at)
		 VALUES (?, ?, ?, ?, ?)`,
		key, accountID, string(session), string(run), at.UnixNano(),
	)
	if isUniqueViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: claim inbound: %w", err)
	}
	return true, nil
}

const dispatchColumns = `id, seq, account_id, session_id, run_id, target,
	kind, payload, created_at, delivered_at, platform_message_ids`

// EnqueueDispatch allocates the next sequence for the account and inserts, in
// one transaction. Splitting the two would let concurrent enqueues claim the
// same number, and a gateway resuming from a cursor would then skip one.
func (s *Store) EnqueueDispatch(ctx context.Context, dispatch gateway.Dispatch) (gateway.DispatchSeq, error) {
	target, err := json.Marshal(dispatch.Target)
	if err != nil {
		return 0, fmt.Errorf("sqlite: encode target: %w", err)
	}

	var seq int64
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM gateway_dispatches WHERE account_id = ?`,
			dispatch.AccountID,
		).Scan(&seq); err != nil {
			return fmt.Errorf("sqlite: allocate dispatch seq: %w", err)
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO gateway_dispatches (`+dispatchColumns+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '[]')`,
			dispatch.ID, seq, dispatch.AccountID, string(dispatch.SessionID), string(dispatch.RunID),
			string(target), string(dispatch.Kind), dispatch.Payload, dispatch.CreatedAt.UnixNano(),
		)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: enqueue dispatch: %w", err)
	}

	return gateway.DispatchSeq(seq), nil
}

func (s *Store) DispatchesAfter(
	ctx context.Context,
	accountID string,
	after gateway.DispatchSeq,
	limit int,
) ([]gateway.Dispatch, error) {
	query := `SELECT ` + dispatchColumns + ` FROM gateway_dispatches
	          WHERE account_id = ? AND seq > ? AND delivered_at IS NULL ORDER BY seq`
	args := []any{accountID, int64(after)}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list dispatches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var dispatches []gateway.Dispatch
	for rows.Next() {
		dispatch, err := scanDispatch(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan dispatch: %w", err)
		}
		dispatches = append(dispatches, dispatch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate dispatches: %w", err)
	}
	return dispatches, nil
}

// MarkDelivered settles a dispatch, refusing a second attempt so a duplicate
// acknowledgement cannot make a reply appear twice.
func (s *Store) MarkDelivered(ctx context.Context, id string, platformMessageIDs []string, at time.Time) error {
	ids, err := json.Marshal(platformMessageIDs)
	if err != nil {
		return fmt.Errorf("sqlite: encode message ids: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE gateway_dispatches SET delivered_at = ?, platform_message_ids = ?
		 WHERE id = ? AND delivered_at IS NULL`,
		at.UnixNano(), string(ids), id,
	)
	if err != nil {
		return fmt.Errorf("sqlite: mark delivered: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: mark delivered: %w", err)
	}
	if affected == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM gateway_dispatches WHERE id = ?`, id,
		).Scan(&exists); err != nil {
			return fmt.Errorf("sqlite: check dispatch: %w", err)
		}
		if exists == 0 {
			return gateway.ErrDispatchNotFound
		}
		return gateway.ErrAlreadyDelivered
	}

	return nil
}

func scanDispatch(row scanner) (gateway.Dispatch, error) {
	var (
		dispatch  gateway.Dispatch
		seq       int64
		targetRaw string
		kind      string
		created   int64
		delivered sql.NullInt64
		idsRaw    string
	)

	if err := row.Scan(&dispatch.ID, &seq, &dispatch.AccountID, &dispatch.SessionID,
		&dispatch.RunID, &targetRaw, &kind, &dispatch.Payload, &created, &delivered, &idsRaw); err != nil {
		return gateway.Dispatch{}, err
	}

	dispatch.Seq = gateway.DispatchSeq(seq)
	dispatch.Kind = gateway.DispatchKind(kind)
	dispatch.CreatedAt = timeFromNanos(created)

	if err := json.Unmarshal([]byte(targetRaw), &dispatch.Target); err != nil {
		return gateway.Dispatch{}, fmt.Errorf("sqlite: decode target: %w", err)
	}
	if err := json.Unmarshal([]byte(idsRaw), &dispatch.PlatformMessageIDs); err != nil {
		return gateway.Dispatch{}, fmt.Errorf("sqlite: decode message ids: %w", err)
	}

	if delivered.Valid {
		at := timeFromNanos(delivered.Int64)
		dispatch.DeliveredAt = &at
	}

	return dispatch, nil
}
