package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

const memoryColumns = `id, scope, scope_ref, activation, text, trust,
	origin_kind, origin_client, origin_platform, origin_principal,
	source_session, source_seq, approved_by,
	created_at, invalidated_at, superseded_by,
	valid_from, valid_until, expires_at, last_used_at`

// The search joins the table to its index, where "text" names a column in
// both, so the same list has to be qualified there.
const memoryColumnsQualified = `memories.id, memories.scope, memories.scope_ref,
	memories.activation, memories.text, memories.trust,
	memories.origin_kind, memories.origin_client,
	memories.origin_platform, memories.origin_principal,
	memories.source_session, memories.source_seq, memories.approved_by,
	memories.created_at, memories.invalidated_at, memories.superseded_by,
	memories.valid_from, memories.valid_until,
	memories.expires_at, memories.last_used_at`

// Remember stores a memory, and invalidates the one it corrects.
//
// Both in one transaction. A correction that half applied would leave the
// agent believing two contradictory things and no way to tell which it meant.
func (s *Store) Remember(
	ctx context.Context,
	memory domain.Memory,
	supersedes domain.MemoryID,
) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memories (`+memoryColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(memory.ID),
			string(memory.Scope),
			memory.ScopeRef,
			string(memory.Activation),
			memory.Text,
			string(memory.Trust),
			string(memory.Origin.Kind),
			memory.Origin.ClientID,
			principalPlatform(memory.Origin),
			principalID(memory.Origin),
			string(memory.SourceSession),
			int64(memory.SourceSeq),
			memory.ApprovedBy,
			memory.CreatedAt.UnixNano(),
			nullableTime(memory.InvalidatedAt),
			nullableID(memory.SupersededBy),
			memory.ValidFrom.UnixNano(),
			nullableTime(memory.ValidUntil),
			nullableTime(memory.ExpiresAt),
			nullableTime(memory.LastUsedAt),
		); err != nil {
			return fmt.Errorf("sqlite: remember: %w", err)
		}

		if supersedes == "" {
			return nil
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE memories
			   SET invalidated_at = ?, superseded_by = ?
			 WHERE id = ? AND invalidated_at IS NULL`,
			memory.CreatedAt.UnixNano(),
			string(memory.ID),
			string(supersedes),
		)
		if err != nil {
			return fmt.Errorf("sqlite: supersede %s: %w", supersedes, err)
		}

		// Nothing updated means it was already superseded or never existed,
		// and a correction to something that is not there is a mistake worth
		// reporting rather than a write that quietly means less than it said.
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return fmt.Errorf("sqlite: %s is not a memory that can be superseded", supersedes)
		}
		return nil
	})
}

func (s *Store) Memories(ctx context.Context, query storage.MemoryQuery) ([]domain.Memory, error) {
	where, args := memoryFilter(query)
	return s.queryMemories(ctx, `SELECT `+memoryColumns+` FROM memories`+where+
		` ORDER BY created_at DESC, id DESC`+memoryLimit(query), args...)
}

// SearchMemories is Memories with a full-text filter.
//
// The query is passed to FTS5 as a phrase rather than as an expression: text
// that reaches here came from a model, and letting it write MATCH syntax turns
// a search into a way to reach rows the scope filter was meant to exclude.
func (s *Store) SearchMemories(
	ctx context.Context,
	text string,
	query storage.MemoryQuery,
) ([]domain.Memory, error) {
	phrase := ftsPhrase(text)
	if phrase == "" {
		return nil, nil
	}

	where, args := memoryFilter(query)
	if where == "" {
		where = " WHERE"
	} else {
		where += " AND"
	}

	return s.queryMemories(ctx, `
		SELECT `+memoryColumnsQualified+`
		  FROM memories
		  JOIN memories_fts ON memories_fts.rowid = memories.rowid`+
		where+` memories_fts MATCH ?
		 ORDER BY bm25(memories_fts)`+memoryLimit(query),
		append(args, phrase)...)
}

func (s *Store) Memory(ctx context.Context, id domain.MemoryID) (domain.Memory, error) {
	found, err := s.queryMemories(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = ?`, string(id))
	if err != nil {
		return domain.Memory{}, err
	}
	if len(found) == 0 {
		return domain.Memory{}, fmt.Errorf("sqlite: no memory %s: %w", id, storage.ErrMemoryNotFound)
	}
	return found[0], nil
}

// Forget removes a memory for good.
//
// Separate from invalidation on purpose. Invalidation answers "that stopped
// being true"; this answers "forget you were ever told", and a person who asks
// for the second and gets the first has not been answered.
func (s *Store) Forget(ctx context.Context, id domain.MemoryID) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		// Anything that pointed at it would otherwise become a reference to
		// something that is gone.
		if _, err := tx.ExecContext(ctx, `
			UPDATE memories SET superseded_by = NULL WHERE superseded_by = ?`,
			string(id)); err != nil {
			return fmt.Errorf("sqlite: unlink %s: %w", id, err)
		}

		result, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, string(id))
		if err != nil {
			return fmt.Errorf("sqlite: forget %s: %w", id, err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return fmt.Errorf("sqlite: no memory %s: %w", id, storage.ErrMemoryNotFound)
		}
		return nil
	})
}

func memoryFilter(query storage.MemoryQuery) (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if len(query.Scopes) > 0 {
		var scopes []string
		for _, scope := range query.Scopes {
			scopes = append(scopes, "(memories.scope = ? AND memories.scope_ref = ?)")
			args = append(args, string(scope.Scope), scope.Ref)
		}
		clauses = append(clauses, "("+strings.Join(scopes, " OR ")+")")
	}

	if query.Activation != "" {
		clauses = append(clauses, "memories.activation = ?")
		args = append(args, string(query.Activation))
	}
	if !query.IncludeInvalidated {
		at := query.At
		if at.IsZero() {
			at = time.Now()
		}
		moment := at.UnixNano()

		// Believed at that moment, on both timelines. Record time says this
		// agent had not retracted it; valid time says the thing was true.
		//
		// Expiry is folded in here rather than swept separately, so that a
		// memory past its expiry stops being offered the moment it is, not
		// whenever a sweep next runs.
		clauses = append(clauses,
			"(memories.invalidated_at IS NULL OR memories.invalidated_at > ?)",
			"(memories.expires_at IS NULL OR memories.expires_at > ?)",
			"(memories.valid_from = 0 OR memories.valid_from <= ?)",
			"(memories.valid_until IS NULL OR memories.valid_until > ?)")
		args = append(args, moment, moment, moment, moment)
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func memoryLimit(query storage.MemoryQuery) string {
	if query.Limit <= 0 {
		return ""
	}
	return fmt.Sprintf(" LIMIT %d", query.Limit)
}

// ftsPhrase makes model-written text safe to hand to FTS5.
//
// Every word becomes a quoted phrase, so nothing in it is read as MATCH
// syntax. Without this, a search is a way to write query operators, and the
// scope filter stops being the only thing deciding which rows come back.
func ftsPhrase(text string) string {
	var terms []string

	for _, word := range strings.Fields(text) {
		cleaned := strings.Map(func(r rune) rune {
			if r == '"' {
				return -1
			}
			return r
		}, word)

		if cleaned != "" {
			terms = append(terms, `"`+cleaned+`"`)
		}
	}

	return strings.Join(terms, " OR ")
}

func (s *Store) queryMemories(ctx context.Context, query string, args ...any) ([]domain.Memory, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query memories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var memories []domain.Memory
	for rows.Next() {
		var (
			memory       domain.Memory
			scope        string
			activation   string
			trust        string
			originKind   string
			platform     string
			principal    string
			createdAt    int64
			invalidated  sql.NullInt64
			supersededBy sql.NullString
			sourceSeq    int64
			validFrom    int64
			validUntil   sql.NullInt64
			expiresAt    sql.NullInt64
			lastUsedAt   sql.NullInt64
		)

		if err := rows.Scan(
			&memory.ID, &scope, &memory.ScopeRef, &activation, &memory.Text, &trust,
			&originKind, &memory.Origin.ClientID, &platform, &principal,
			&memory.SourceSession, &sourceSeq, &memory.ApprovedBy,
			&createdAt, &invalidated, &supersededBy,
			&validFrom, &validUntil, &expiresAt, &lastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("sqlite: scan memory: %w", err)
		}

		memory.Scope = domain.MemoryScope(scope)
		memory.Activation = domain.MemoryActivation(activation)
		memory.Trust = domain.TrustLevel(trust)
		memory.Origin.Kind = domain.RunOriginKind(originKind)
		memory.SourceSeq = domain.Seq(sourceSeq)

		if principal != "" {
			memory.Origin.Principal = &domain.ExternalPrincipal{
				Platform:    platform,
				PrincipalID: principal,
			}
		}

		memory.CreatedAt = timeFromNanos(createdAt)
		if invalidated.Valid {
			at := timeFromNanos(invalidated.Int64)
			memory.InvalidatedAt = &at
		}
		if supersededBy.Valid {
			memory.SupersededBy = domain.MemoryID(supersededBy.String)
		}

		// Zero means "since it was learned", which is the honest default for
		// every memory written before there was anywhere to say otherwise.
		memory.ValidFrom = memory.CreatedAt
		if validFrom > 0 {
			memory.ValidFrom = timeFromNanos(validFrom)
		}
		if validUntil.Valid {
			at := timeFromNanos(validUntil.Int64)
			memory.ValidUntil = &at
		}
		if expiresAt.Valid {
			at := timeFromNanos(expiresAt.Int64)
			memory.ExpiresAt = &at
		}
		if lastUsedAt.Valid {
			at := timeFromNanos(lastUsedAt.Int64)
			memory.LastUsedAt = &at
		}

		memories = append(memories, memory)
	}

	return memories, rows.Err()
}

func principalPlatform(origin domain.RunOrigin) string {
	if origin.Principal == nil {
		return ""
	}
	return origin.Principal.Platform
}

func principalID(origin domain.RunOrigin) string {
	if origin.Principal == nil {
		return ""
	}
	return origin.Principal.PrincipalID
}

func nullableID(id domain.MemoryID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// ExpireMemories stops believing what has gone unused past its expiry.
//
// Invalidated rather than deleted, and with no superseding memory: reaching an
// expiry is not evidence anything was wrong, and the record of having once
// believed it is the thing that makes "why did it do that in June" answerable.
//
// A sweep as well as the filter in every query, because the two answer
// different needs: the filter is what stops an expired memory being used, and
// this is what makes "agent memory list" tell the truth without every reader
// re-deriving it.
func (s *Store) ExpireMemories(ctx context.Context, at time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE memories
		   SET invalidated_at = ?
		 WHERE invalidated_at IS NULL
		   AND expires_at IS NOT NULL
		   AND expires_at <= ?`,
		at.UnixNano(), at.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("sqlite: expire memories: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: expire memories: %w", err)
	}
	return int(affected), nil
}

// TouchMemories records that these were used, and pushes their expiry out.
//
// Both together: recording the use without moving the expiry would make the
// timestamp decoration, and moving the expiry without recording the use would
// leave nothing to explain why it moved.
//
// Only memories that already have an expiry are moved. One written without a
// lifetime is not given one by being read.
func (s *Store) TouchMemories(ctx context.Context, ids []domain.MemoryID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := make([]string, 0, len(ids))
	args := []any{at.UnixNano(), at.UnixNano()}
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, string(id))
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE memories
		   SET last_used_at = ?,
		       expires_at = CASE
		           WHEN expires_at IS NULL THEN NULL
		           ELSE ? + (expires_at - COALESCE(last_used_at, created_at))
		       END
		 WHERE id IN (`+strings.Join(placeholders, ", ")+`)
		   AND invalidated_at IS NULL`, args...)
	if err != nil {
		return fmt.Errorf("sqlite: touch memories: %w", err)
	}
	return nil
}
