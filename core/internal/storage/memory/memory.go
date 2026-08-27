// Package memory implements storage.Store in memory.
//
// It exists so unit tests do not pay for a database, and so the SQLite
// implementation has something to be checked against: internal/storage runs
// the same conformance suite over both, which is how the two are kept
// behaviourally identical.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

type Store struct {
	mu        sync.RWMutex
	sessions  map[domain.SessionID]domain.Session
	runs      map[domain.RunID]domain.Run
	events    map[domain.SessionID][]domain.Event
	approvals map[domain.ApprovalID]domain.Approval
}

var _ storage.Store = (*Store)(nil)

func New() *Store {
	return &Store{
		sessions:  make(map[domain.SessionID]domain.Session),
		runs:      make(map[domain.RunID]domain.Run),
		events:    make(map[domain.SessionID][]domain.Event),
		approvals: make(map[domain.ApprovalID]domain.Approval),
	}
}

func (s *Store) CreateSession(ctx context.Context, session domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sessions[session.ID]; exists {
		return storage.ErrDuplicateSession
	}
	s.sessions[session.ID] = session
	return nil
}

func (s *Store) Session(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return domain.Session{}, storage.ErrSessionNotFound
	}
	return session, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]domain.Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}

	// Newest first, matching the SQLite ordering.
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
		}
		return sessions[i].ID > sessions[j].ID
	})
	return sessions, nil
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.runs[run.ID]; exists {
		return storage.ErrDuplicateRun
	}
	s.runs[run.ID] = run
	return nil
}

func (s *Store) UpdateRun(ctx context.Context, run domain.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.runs[run.ID]
	if !ok {
		return storage.ErrRunNotFound
	}

	// Mirror the SQLite statement, which only writes these two columns.
	existing.Status = run.Status
	existing.FinishedAt = run.FinishedAt
	s.runs[run.ID] = existing
	return nil
}

func (s *Store) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	if err := ctx.Err(); err != nil {
		return domain.Run{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	run, ok := s.runs[id]
	if !ok {
		return domain.Run{}, storage.ErrRunNotFound
	}
	return run, nil
}

func (s *Store) ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error) {
	return s.filterRuns(ctx, func(run domain.Run) bool { return run.SessionID == session })
}

func (s *Store) UnfinishedRuns(ctx context.Context) ([]domain.Run, error) {
	return s.filterRuns(ctx, func(run domain.Run) bool { return !run.Status.IsTerminal() })
}

func (s *Store) filterRuns(ctx context.Context, keep func(domain.Run) bool) ([]domain.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var runs []domain.Run
	for _, run := range s.runs {
		if keep(run) {
			runs = append(runs, run)
		}
	}

	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
	return runs, nil
}

func (s *Store) Append(ctx context.Context, event domain.Event) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[event.SessionID]; !ok {
		return 0, storage.ErrSessionNotFound
	}

	events := s.events[event.SessionID]
	event.Seq = domain.Seq(len(events) + 1)
	s.events[event.SessionID] = append(events, event)

	return event.Seq, nil
}

func (s *Store) ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.sessions[id]; !ok {
		return nil, storage.ErrSessionNotFound
	}

	events := s.events[id]
	if int(after) >= len(events) {
		return nil, nil
	}

	// Sequences are dense and 1-based, so seq N is at index N-1.
	tail := events[after:]
	if limit > 0 && len(tail) > limit {
		tail = tail[:limit]
	}

	out := make([]domain.Event, len(tail))
	copy(out, tail)
	return out, nil
}

func (s *Store) Head(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.sessions[id]; !ok {
		return 0, storage.ErrSessionNotFound
	}
	return domain.Seq(len(s.events[id])), nil
}

func (s *Store) CreateApproval(ctx context.Context, approval domain.Approval) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.approvals == nil {
		s.approvals = make(map[domain.ApprovalID]domain.Approval)
	}
	for _, existing := range s.approvals {
		// Mirrors the unique index: one approval per tool call, so the same
		// prompt cannot be raised or answered twice.
		if existing.RunID == approval.RunID && existing.ToolCallID == approval.ToolCallID {
			return storage.ErrApprovalDecided
		}
	}

	s.approvals[approval.ID] = approval
	return nil
}

func (s *Store) Approval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	approval, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, storage.ErrApprovalNotFound
	}
	return approval, nil
}

func (s *Store) DecideApproval(
	ctx context.Context,
	id domain.ApprovalID,
	status domain.ApprovalStatus,
	scope domain.RememberScope,
	decidedBy string,
	at time.Time,
) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	approval, ok := s.approvals[id]
	if !ok {
		return domain.Approval{}, storage.ErrApprovalNotFound
	}
	if !approval.IsPending() {
		return domain.Approval{}, storage.ErrApprovalDecided
	}

	approval.Status = status
	approval.Scope = scope
	approval.DecidedBy = decidedBy
	approval.DecidedAt = &at
	s.approvals[id] = approval

	return approval, nil
}

func (s *Store) PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var pending []domain.Approval
	for _, approval := range s.approvals {
		if approval.SessionID == session && approval.IsPending() {
			pending = append(pending, approval)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		if !pending[i].CreatedAt.Equal(pending[j].CreatedAt) {
			return pending[i].CreatedAt.Before(pending[j].CreatedAt)
		}
		return pending[i].ID < pending[j].ID
	})
	return pending, nil
}

func (s *Store) ApprovalForCall(ctx context.Context, run domain.RunID, call domain.ToolCallID) (domain.Approval, error) {
	if err := ctx.Err(); err != nil {
		return domain.Approval{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, approval := range s.approvals {
		if approval.RunID == run && approval.ToolCallID == call {
			return approval, nil
		}
	}
	return domain.Approval{}, storage.ErrApprovalNotFound
}
