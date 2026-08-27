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
)

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
