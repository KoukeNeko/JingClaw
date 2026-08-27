// Package event owns the append-only event log and the notification hub that
// wakes subscribers when it grows.
package event

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

var ErrSessionNotFound = errors.New("event: session not found")

// Store is the append-only log. M0 keeps it in memory; M1 swaps in SQLite
// without changing this interface.
type Store interface {
	// Append assigns the next sequence number for the session and returns it.
	Append(ctx context.Context, ev domain.Event) (domain.Seq, error)

	// ListAfter returns events with Seq > after, oldest first, capped at limit.
	ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error)

	// Head is the highest sequence currently stored for the session.
	Head(ctx context.Context, id domain.SessionID) (domain.Seq, error)
}

// IDGenerator produces identifiers. Injected so tests can be deterministic.
type IDGenerator func() string

// MemoryStore is an in-memory Store for M0.
type MemoryStore struct {
	newID IDGenerator
	now   func() time.Time

	mu       sync.RWMutex
	sessions map[domain.SessionID][]domain.Event
}

func NewMemoryStore(newID IDGenerator, now func() time.Time) *MemoryStore {
	return &MemoryStore{
		newID:    newID,
		now:      now,
		sessions: make(map[domain.SessionID][]domain.Event),
	}
}

func (s *MemoryStore) Append(ctx context.Context, ev domain.Event) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Sequence assignment and insertion happen under one lock so the log can
	// never hand out a number that is not yet readable. In M1 this becomes a
	// single SQLite transaction for the same reason.
	events := s.sessions[ev.SessionID]
	ev.Seq = domain.Seq(len(events) + 1)

	if ev.ID == "" {
		ev.ID = domain.EventID(s.newID())
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = s.now()
	}

	s.sessions[ev.SessionID] = append(events, ev)
	return ev.Seq, nil
}

func (s *MemoryStore) ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	events, ok := s.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	if int(after) >= len(events) {
		return nil, nil
	}

	// Seq is 1-based and events are dense, so seq N sits at index N-1.
	tail := events[after:]
	if limit > 0 && len(tail) > limit {
		tail = tail[:limit]
	}

	out := make([]domain.Event, len(tail))
	copy(out, tail)
	return out, nil
}

func (s *MemoryStore) Head(ctx context.Context, id domain.SessionID) (domain.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	events, ok := s.sessions[id]
	if !ok {
		return 0, ErrSessionNotFound
	}
	return domain.Seq(len(events)), nil
}

// EnsureSession registers a session so that subscribing to it before any event
// exists does not look like a missing session.
func (s *MemoryStore) EnsureSession(id domain.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[id]; !ok {
		s.sessions[id] = nil
	}
}
