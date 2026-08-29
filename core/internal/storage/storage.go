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
	"time"

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

	ErrApprovalNotFound = errors.New("storage: approval not found")
	ErrMemoryNotFound   = errors.New("storage: memory not found")

	// ErrApprovalDecided guards the race where two clients answer the same
	// prompt: the second answer must not run the tool again.
	ErrApprovalDecided = errors.New("storage: approval has already been decided")
)

type SessionStore interface {
	CreateSession(ctx context.Context, session domain.Session) error
	Session(ctx context.Context, id domain.SessionID) (domain.Session, error)
	ListSessions(ctx context.Context) ([]domain.Session, error)

	// SetSessionModel changes which model answers here. The next run picks it
	// up; one already generating has a request open with a model in it, and
	// changing that underneath would attribute a turn to a model that did not
	// write it.
	SetSessionModel(ctx context.Context, id domain.SessionID, model string, at time.Time) error
}

// PlanStore holds what the agent said it was going to do.
//
// Read back rather than kept in memory: a plan that did not survive a restart
// would be one the agent forgot every time the daemon was updated, which is
// exactly when somebody is most likely to be watching.
type PlanStore interface {
	Plan(ctx context.Context, session domain.SessionID) ([]domain.PlanItem, error)
	SetPlan(ctx context.Context, session domain.SessionID, items []domain.PlanItem, at time.Time) error
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

type ApprovalStore interface {
	CreateApproval(ctx context.Context, approval domain.Approval) error

	Approval(ctx context.Context, id domain.ApprovalID) (domain.Approval, error)

	// DecideApproval settles a pending approval. It must fail rather than
	// overwrite when the approval is already decided, so a duplicate answer
	// cannot cause a second execution.
	DecideApproval(ctx context.Context, id domain.ApprovalID, status domain.ApprovalStatus,
		scope domain.RememberScope, decidedBy string, at time.Time) (domain.Approval, error)

	// PendingApprovals lists what is waiting, so a client that connects late
	// can still answer.
	PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error)

	// ApprovalForCall finds the decision made about a specific tool call,
	// which is how a resumed run learns what it was told.
	ApprovalForCall(ctx context.Context, run domain.RunID, call domain.ToolCallID) (domain.Approval, error)
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

	// Oldest is the earliest event still kept for a session.
	//
	// Zero when nothing has been pruned. A client resuming from below this
	// has to start again: what it asked for is gone, and handing it whatever
	// survived would draw a conversation missing its middle.
	Oldest(ctx context.Context, id domain.SessionID) (domain.Seq, error)

	// PruneEvents discards events at or below through, for one session.
	//
	// Never called with a sequence past the last compaction: the conversation
	// sent to the model is rebuilt from this log, and discarding an event the
	// rebuild still needs makes the session unusable rather than smaller.
	PruneEvents(ctx context.Context, id domain.SessionID, through domain.Seq) (int64, error)
}

// Store is the full persistence surface the runtime depends on.
// MemoryStore keeps what the agent has been told to remember across sessions.
//
// Writes are append-only and corrections invalidate rather than overwrite, so
// the store can always answer both "what is believed now" and "what was
// believed then". Forget is the exception, and exists because a person asking
// the agent to forget something has to be answered by it actually being gone.
type MemoryStore interface {
	// Remember stores a memory. When supersedes names an existing one, both
	// happen together: a correction that half applied would leave the agent
	// believing two contradictory things.
	Remember(ctx context.Context, memory domain.Memory, supersedes domain.MemoryID) error

	// Memories returns what is believed, newest first.
	Memories(ctx context.Context, query MemoryQuery) ([]domain.Memory, error)

	// SearchMemories is Memories with a full-text filter.
	SearchMemories(ctx context.Context, text string, query MemoryQuery) ([]domain.Memory, error)

	// Memory returns one by id.
	Memory(ctx context.Context, id domain.MemoryID) (domain.Memory, error)

	// Forget removes a memory for good, index and all.
	//
	// It removes what the agent believes, not every trace of the fact. The
	// conversation the memory came from is still in the event log, and so is
	// the tool call that wrote it — an append-only log cannot forget, which is
	// the price of it being able to say what happened.
	//
	// That is why provenance is on every memory: forgetting tells you where
	// else to look. Saying "deleted" without saying which of the two was meant
	// is the part that would be dishonest.
	Forget(ctx context.Context, id domain.MemoryID) error
}

// MemoryQuery narrows what comes back.
//
// Scopes is a list because one caller legitimately wants several: a local run
// reads what the project knows and what its operator said. It is never
// implicit, though — a query that forgot to say whose memories it wanted would
// otherwise return everybody's.
type MemoryQuery struct {
	Scopes []MemoryScopeRef

	// Activation empty means either.
	Activation domain.MemoryActivation

	// IncludeInvalidated returns superseded memories too, which is for showing
	// a person what changed rather than for telling a model anything.
	IncludeInvalidated bool

	Limit int
}

// MemoryScopeRef is one scope and the thing it belongs to.
type MemoryScopeRef struct {
	Scope domain.MemoryScope
	Ref   string
}

type Store interface {
	PlanStore
	SessionStore
	RunStore
	ApprovalStore
	EventStore
	MemoryStore
}
