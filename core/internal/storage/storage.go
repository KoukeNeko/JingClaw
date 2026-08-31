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

	ErrQuestionNotFound = errors.New("storage: question not found")

	// ErrQuestionAnswered guards the same race approvals guard: two clients
	// answering the same prompt must not resume the run twice.
	ErrQuestionAnswered = errors.New("storage: question has already been answered")

	ErrScheduleNotFound  = errors.New("storage: schedule not found")
	ErrDuplicateSchedule = errors.New("storage: schedule already exists")

	// ErrFiringAlreadyResolved is not a failure. It is the answer to the
	// question a reconcile is asking: somebody already accounted for this
	// occasion. Two daemons starting a second apart, or one restarting twice,
	// must produce one run for one three o'clock.
	ErrFiringAlreadyResolved = errors.New("storage: that firing is already resolved")

	// ErrFiringNotResolved means a link was written for an occasion nobody
	// had claimed, which is an ordering mistake rather than a missing row.
	ErrFiringNotResolved = errors.New("storage: that firing was never resolved")
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

// QuestionStore holds what the agent asked and what came back.
//
// Separate from approvals because the two are different questions: an
// approval asks whether something may happen and is answered yes or no, and
// this asks what a person wants and is answered with their words.
type QuestionStore interface {
	CreateQuestion(ctx context.Context, question domain.Question) error
	Question(ctx context.Context, id domain.QuestionID) (domain.Question, error)

	// QuestionForCall finds the question a tool call asked, so a resumed run
	// settles the call it is looking at rather than some other question.
	QuestionForCall(ctx context.Context, run domain.RunID, call domain.ToolCallID) (domain.Question, error)

	PendingQuestions(ctx context.Context, session domain.SessionID) ([]domain.Question, error)

	AnswerQuestion(
		ctx context.Context,
		id domain.QuestionID,
		status domain.QuestionStatus,
		answer string,
		answeredBy domain.RunOrigin,
		at time.Time,
	) (domain.Question, error)
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
		scope domain.RememberScope, decidedBy domain.RunOrigin, at time.Time) (domain.Approval, error)

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

	// ListAllAfter returns events from the whole log with GlobalSeq > after,
	// in the order they were appended. A limit of zero means all of them.
	//
	// For anything watching every session at once, which cannot resume from a
	// per-session sequence: two sessions both at 50 make "I have read up to
	// 50" mean nothing.
	ListAllAfter(ctx context.Context, after domain.Seq, limit int) ([]domain.Event, error)

	// LogHead is the position of the last event appended, across every
	// session. Zero when nothing has been.
	LogHead(ctx context.Context) (domain.Seq, error)

	// LogPrunedThrough is the highest position that has been discarded.
	//
	// A client resuming from at or below it has missed events that are gone.
	// "Nothing has happened since" and "what happened is no longer here" are
	// opposite answers, and a stream that cannot tell them apart is one that
	// loses history silently.
	LogPrunedThrough(ctx context.Context) (domain.Seq, error)
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

	// At is the moment to answer for. Zero means now.
	//
	// It exists so that "what did it believe when that run happened" is a
	// question with an answer. A store that only knows about now cannot
	// explain a run from last month, and explaining old runs is most of what
	// a log is for.
	At time.Time

	Limit int
}

// MemoryScopeRef is one scope and the thing it belongs to.
type MemoryScopeRef struct {
	Scope domain.MemoryScope
	Ref   string
}

type Store interface {
	PlanStore
	QuestionStore
	SessionStore
	RunStore
	ApprovalStore
	EventStore
	MemoryStore
	ScheduleStore
}

// ScheduleStore keeps standing instructions and the occasions already
// accounted for.
//
// The two together, because the second is what makes the first safe to act
// on. A schedule alone says when something is due; only the firings say
// whether this particular three o'clock has already been answered, and
// without that a daemon that restarts answers it again.
type ScheduleStore interface {
	CreateSchedule(ctx context.Context, schedule domain.Schedule) error

	// UpdateSchedule counts the change: the revision goes up, because a
	// schedule that changed is a different instruction and the firings
	// resolved under the old one belong to the old one.
	UpdateSchedule(ctx context.Context, schedule domain.Schedule) error

	// SetSchedulePaused does not count as a change. Pausing does not alter
	// what the schedule means, and bumping the revision would orphan every
	// firing already resolved under it.
	SetSchedulePaused(ctx context.Context, id domain.ScheduleID, paused bool) error

	Schedule(ctx context.Context, id domain.ScheduleID) (domain.Schedule, error)

	// ListSchedules includes paused ones. The commonest reason to look is to
	// find the one to turn back on.
	ListSchedules(ctx context.Context) ([]domain.Schedule, error)

	DeleteSchedule(ctx context.Context, id domain.ScheduleID) error

	// ResolveFiring records that an occasion has been accounted for, and
	// refuses a second attempt at the same one with ErrFiringAlreadyResolved.
	// That refusal is how reconciling stays idempotent.
	ResolveFiring(ctx context.Context, firing domain.Firing) error

	// RecordFiringRun links an occasion to the run it became.
	//
	// Separate from resolving it, and after it, because the two answer
	// different questions and only one of them has to happen first. The
	// claim has to be made before anything starts, or two daemons both start
	// a run for one three o'clock; the run's identity is not known until
	// after. A link that fails to be written costs a nicety in a listing.
	RecordFiringRun(ctx context.Context, firing domain.Firing) error

	// LastFiring is the most recent occasion accounted for at this revision,
	// or the zero time. It is the left-hand end of "what is owed".
	LastFiring(ctx context.Context, id domain.ScheduleID, revision int) (time.Time, error)
}
