// Package domain holds the core types of the agent runtime.
//
// Nothing here may import generated protobuf code. The runtime must never
// learn the wire format: swapping Connect for another transport, or adding a
// second one, has to be possible without touching this package.
package domain

import "time"

type (
	SessionID string
	RunID     string
	EventID   string
	MessageID string
)

// Seq orders events within a single session. It is strictly increasing with
// no gaps, which is what lets a disconnected client ask for everything after
// the last sequence it managed to apply.
type Seq uint64

type RunStatus string

const (
	RunQueued  RunStatus = "queued"
	RunRunning RunStatus = "running"

	// Reserved for M1, when a permission decision or a structured answer from
	// a human can park a run mid-flight.
	RunAwaitingApproval RunStatus = "awaiting_approval"
	RunAwaitingInput    RunStatus = "awaiting_input"

	RunCancelling RunStatus = "cancelling"
	RunCompleted  RunStatus = "completed"
	RunCancelled  RunStatus = "cancelled"
	RunFailed     RunStatus = "failed"
)

// IsTerminal reports whether a run has reached a state it cannot leave.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunCompleted, RunCancelled, RunFailed:
		return true
	default:
		return false
	}
}

// TrustLevel records where a piece of content came from. Content never
// escalates its own level: a document that says "the user approved this"
// remains untrusted, because trust is a property of provenance, not of text.
type TrustLevel string

const (
	TrustUntrusted TrustLevel = "untrusted"
	TrustUser      TrustLevel = "user"
	TrustWorkspace TrustLevel = "workspace"
	TrustSystem    TrustLevel = "system"
)

type RunOriginKind string

const (
	OriginLocalClient RunOriginKind = "local_client"

	// Reserved for M1b. No gateway plane exists yet, but Run carries an origin
	// from the start so that adding one is not a schema migration.
	OriginGateway RunOriginKind = "gateway"
)

// ExternalPrincipal is an identity owned by an external platform. Authorization
// reads PrincipalID only; DisplayName is presentation and can be changed by its
// owner at will.
type ExternalPrincipal struct {
	Platform    string
	AccountID   string
	TenantID    string
	PrincipalID string
	DisplayName string
}

type RunOrigin struct {
	Kind      RunOriginKind
	ClientID  string
	Principal *ExternalPrincipal
}

// DeliveryTarget says where a run's output should go. It belongs to the Run
// rather than the Session so that taking over a gateway-started session from a
// GUI does not echo the operator's own notes back to the origin channel.
type DeliveryTarget struct {
	Kind string
	Ref  string
}

const DeliveryLocalClient = "local_client"

type Session struct {
	ID        SessionID
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Run struct {
	ID              RunID
	SessionID       SessionID
	Status          RunStatus
	Origin          RunOrigin
	DeliveryTargets []DeliveryTarget
	CreatedAt       time.Time
	FinishedAt      *time.Time
}

// EventKind discriminates Event.Payload.
type EventKind string

const (
	EventUserMessageAdded          EventKind = "user.message"
	EventRunStateChanged           EventKind = "run.state_changed"
	EventAssistantTextDelta        EventKind = "assistant.delta"
	EventAssistantMessageCompleted EventKind = "assistant.completed"
	EventUsageChanged              EventKind = "usage.changed"
	EventToolCallRequested         EventKind = "tool.requested"
	EventToolCallCompleted         EventKind = "tool.completed"
)

// ToolCallID identifies one tool invocation within a run.
type ToolCallID string

type Event struct {
	ID         EventID
	SessionID  SessionID
	RunID      RunID
	Seq        Seq
	OccurredAt time.Time

	Kind    EventKind
	Payload EventPayload
}

// EventPayload is a closed set; the unexported marker keeps other packages
// from adding cases the control layer would silently fail to translate.
type EventPayload interface {
	isEventPayload()
}

type UserMessageAdded struct {
	MessageID MessageID
	Text      string
	Trust     TrustLevel
	Origin    RunOrigin
}

type RunStateChanged struct {
	Status RunStatus
	Reason string
}

type AssistantTextDelta struct {
	MessageID MessageID
	Text      string
}

type AssistantMessageCompleted struct {
	MessageID  MessageID
	StopReason StopReason
}

// ToolCallRequested records the model asking for a tool, before anything runs.
// Persisting the request separately from the result is what lets a client show
// work in progress, and what makes a call that never completed visible after a
// crash.
type ToolCallRequested struct {
	CallID    ToolCallID
	Name      string
	Arguments string

	// ProviderMetadata is opaque continuity state the provider attached to
	// this call and expects back on the next turn.
	//
	// It is persisted because the conversation is rebuilt from this log: a
	// provider that requires the metadata would reject every turn after a
	// restart if it were only held in memory. Nothing outside the provider
	// adapter interprets it.
	ProviderMetadata string
}

// ToolCallCompleted records the observation handed back to the model.
//
// Content is stored, not just summarised, because the conversation for the
// next turn is rebuilt from this log: a result that cannot be replayed is a
// conversation that cannot be resumed.
type ToolCallCompleted struct {
	CallID  ToolCallID
	Name    string
	Summary string
	Content string

	IsError   bool
	Truncated bool

	DurationMS int64
}

// UsageChanged reports cumulative usage for the run so far, so cost is visible
// while a run is in flight rather than only once it ends.
type UsageChanged struct {
	Usage Usage
}

func (UserMessageAdded) isEventPayload()          {}
func (RunStateChanged) isEventPayload()           {}
func (AssistantTextDelta) isEventPayload()        {}
func (AssistantMessageCompleted) isEventPayload() {}
func (UsageChanged) isEventPayload()              {}
func (ToolCallRequested) isEventPayload()         {}
func (ToolCallCompleted) isEventPayload()         {}

// StopReason says why a generation ended. Providers spell this differently;
// the runtime needs one vocabulary to decide whether a turn is genuinely
// finished or was cut short.
type StopReason string

const (
	StopEndTurn StopReason = "end_turn"

	// StopMaxTokens means the answer was truncated, not completed. Treating it
	// as a normal finish is how agents end up silently losing half a reply.
	StopMaxTokens StopReason = "max_tokens"

	StopContentFilter StopReason = "content_filter"
	StopCancelled     StopReason = "cancelled"
	StopError         StopReason = "error"

	// Reserved for M1, when a turn can end because the model asked for a tool.
	StopToolUse StopReason = "tool_use"
)

// Usage is token accounting for one model call. Providers report it at
// different moments and with different granularity, so every field is
// optional and zero means "not reported" rather than "zero tokens".
type Usage struct {
	InputTokens int64

	// CachedInputTokens is the subset of InputTokens served from a prompt
	// cache. Tracked separately because it is priced differently.
	CachedInputTokens int64

	OutputTokens int64

	// ReasoningTokens covers output the model produced while thinking, which
	// is billed but never shown.
	ReasoningTokens int64
}

// Add merges another usage record into this one.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:       u.InputTokens + other.InputTokens,
		CachedInputTokens: u.CachedInputTokens + other.CachedInputTokens,
		OutputTokens:      u.OutputTokens + other.OutputTokens,
		ReasoningTokens:   u.ReasoningTokens + other.ReasoningTokens,
	}
}

// TotalTokens is what a budget check compares against.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.ReasoningTokens
}

type ApprovalID string

type ApprovalStatus string

const (
	ApprovalPending ApprovalStatus = "pending"
	ApprovalAllowed ApprovalStatus = "allowed"
	ApprovalDenied  ApprovalStatus = "denied"
)

// RememberScope is how far a human's answer carries.
//
// There is deliberately no "forever" option here. A standing permission that
// outlives the session is a policy change, and policy changes belong in
// configuration a person can read back, not in a button pressed mid-run.
type RememberScope string

const (
	RememberOnce    RememberScope = "once"
	RememberSession RememberScope = "session"
)

// Approval is a paused tool call waiting on a human.
//
// It is persisted rather than held in memory because the pause has to survive
// a restart: a run stopped at a permission prompt is not an orphan to clean
// up, it is work waiting for an answer that may come hours later.
type Approval struct {
	ID        ApprovalID
	SessionID SessionID
	RunID     RunID

	ToolCallID ToolCallID
	ToolName   string

	// Arguments are stored as the tool will receive them, so a reviewer judges
	// the real call rather than a rendering of it.
	Arguments string

	Summary string
	Effects []string

	Status ApprovalStatus
	Scope  RememberScope

	CreatedAt time.Time
	DecidedAt *time.Time

	// DecidedBy identifies the client that answered, for the audit trail.
	DecidedBy string
}

// IsPending reports whether this approval is still waiting.
func (a Approval) IsPending() bool { return a.Status == ApprovalPending }

const (
	EventApprovalRequested EventKind = "approval.requested"
	EventApprovalResolved  EventKind = "approval.resolved"
)

// ApprovalRequested announces that a run has paused. Every client sees it, so
// whoever is nearest can answer.
type ApprovalRequested struct {
	ApprovalID ApprovalID
	CallID     ToolCallID
	ToolName   string
	Arguments  string
	Summary    string
	Effects    []string
}

// ApprovalResolved records the answer and who gave it.
type ApprovalResolved struct {
	ApprovalID ApprovalID
	CallID     ToolCallID
	ToolName   string
	Status     ApprovalStatus
	Scope      RememberScope
	DecidedBy  string
}

func (ApprovalRequested) isEventPayload() {}
func (ApprovalResolved) isEventPayload()  {}
