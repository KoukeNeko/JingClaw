// Package storage defines the persistence contracts for the agent runtime.
//
// It is split from internal/event on purpose: the event hub is a transient
// notification mechanism scoped to live subscribers, while these interfaces
// describe durable state that must survive a restart. Conflating them made
// sense while everything lived in memory and stops making sense the moment a
// database is involved.
package storage

import (
	"context"
	"errors"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

var (
	ErrSessionNotFound = errors.New("storage: session not found")
	ErrRunNotFound     = errors.New("storage: run not found")

	// ErrDuplicateSession and ErrDuplicateRun surface a primary key conflict,
	// which in practice means an ID generator collided or a request was
	// replayed.
	ErrDuplicateSession = errors.New("storage: session already exists")
	ErrDuplicateRun     = errors.New("storage: run already exists")
)

type SessionStore interface {
	CreateSession(ctx context.Context, session domain.Session) error
	Session(ctx context.Context, id domain.SessionID) (domain.Session, error)
	ListSessions(ctx context.Context) ([]domain.Session, error)
}

type RunStore interface {
	CreateRun(ctx context.Context, run domain.Run) error

	// UpdateRun persists status and finish time. Runs are otherwise immutable.
	UpdateRun(ctx context.Context, run domain.Run) error

	Run(ctx context.Context, id domain.RunID) (domain.Run, error)
	ListRuns(ctx context.Context, session domain.SessionID) ([]domain.Run, error)

	// UnfinishedRuns returns runs left in a non-terminal state. After a crash
	// these are orphans: nothing is driving them any more, so startup has to
	// resolve them rather than leave clients watching a run that will never
	// end.
	UnfinishedRuns(ctx context.Context) ([]domain.Run, error)
}

type EventStore interface {
	// Append assigns the next sequence number for the session and returns it.
	// Allocation and insertion must be atomic: a number handed out before the
	// row is readable would let a subscriber skip past an event forever.
	Append(ctx context.Context, event domain.Event) (domain.Seq, error)

	// ListAfter returns events with Seq > after, oldest first. A limit of zero
	// means no limit.
	ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error)

	// Head is the highest sequence stored for the session, or zero if it has
	// no events yet.
	Head(ctx context.Context, id domain.SessionID) (domain.Seq, error)
}

// Store is the full persistence surface the runtime depends on.
type Store interface {
	SessionStore
	RunStore
	EventStore
}
