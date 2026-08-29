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
	ID    SessionID
	Title string

	// Model overrides which model answers here. Empty uses the configured
	// one, which is what almost every session does.
	//
	// Per session rather than per run: a conversation whose model changed
	// halfway through is one whose earlier turns were written by a different
	// writer, and the answer to "why did it get worse" would be invisible.
	// Per session, the choice is a property of the conversation and shows up
	// in every run summary it produces.
	//
	// The provider is deliberately not overridable. A conversation carries
	// blocks only its own provider can read back — a thought signature, an
	// opaque tool-call payload — and moving it to another one silently
	// discards them. Choosing a model within the configured provider is what
	// a local deployment actually needs: the small one that fits in memory,
	// or the large one that does not.
	Model string

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

// Turn is one thing somebody said, and where the answer is owed.
//
// A struct rather than five parameters because the list was already at the
// length where a caller has to count commas, and attachments would have made
// it six.
type Turn struct {
	Text   string
	Origin RunOrigin

	// Targets are where the reply goes. A turn with none is answered to
	// nobody, which is a bug rather than a feature.
	Targets []DeliveryTarget

	// Attachments are files that arrived with it, already in the artifact
	// store. The bytes do not travel through here.
	Attachments []Attachment
}

// Attachment is a file that arrived with a message.
//
// The bytes are in the artifact store and this is the reference to them. They
// are deliberately not in the event: an image is large, the log is replayed on
// every turn, and a conversation carrying copies of everything ever sent to it
// would stop working long before the context window did.
type Attachment struct {
	ArtifactID string
	Name       string
	MediaType  string
	Size       int64
}

// IsImage reports whether this is something a model can look at.
//
// Decided by the declared media type, not by sniffing the bytes: what a
// provider will accept is a question about the type it is told, and guessing
// differently from the sender is how a request gets rejected for a reason
// nobody can see.
func (a Attachment) IsImage() bool {
	switch a.MediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// MemoryID identifies one thing the agent has been told to remember.
type MemoryID string

// MemoryScope says who a memory belongs to.
//
// These are the two boundaries that turned out to be real. A channel is not
// one — that is a session. A machine is not one — that is the daemon.
type MemoryScope string

const (
	// ScopeWorkspace is a fact about the project: its conventions, how it is
	// built, what its scripts need.
	ScopeWorkspace MemoryScope = "workspace"

	// ScopePrincipal is what one person said. A Discord account and the
	// operator of this machine are different people, and what one of them said
	// is not recalled for the other.
	ScopePrincipal MemoryScope = "principal"
)

// MemoryActivation decides how a memory reaches the model.
//
// It names the mechanism rather than pretending to be an ontology. "Instruction
// or fact" was the earlier shape and it decided nothing: "the user is called
// 江委員" is a fact that belongs in every prompt, and "prefer Helm for
// Kubernetes questions" is an instruction with no business being there while
// somebody writes Python. What matters is whether it is carried or looked up.
type MemoryActivation string

const (
	// MemoryStanding is put in front of the model on every turn. It is bounded
	// and it is the privileged kind: a memory here shapes every future run
	// without anybody asking for it, which is why writing one stops for a
	// person and writing the other does not.
	MemoryStanding MemoryActivation = "standing"

	// MemoryRetrieval is looked up when it is wanted. Most memories are these:
	// something worth having is not the same as something worth carrying.
	MemoryRetrieval MemoryActivation = "retrieval"
)

// Memory is one thing the agent has been told to remember across sessions.
//
// Provenance is not optional. A memory whose origin has been lost is a claim
// nobody can check, and the way untrusted text becomes a trusted fact is by
// passing through a summary that dropped where it came from.
type Memory struct {
	ID         MemoryID
	Scope      MemoryScope
	ScopeRef   string
	Activation MemoryActivation
	Text       string

	// Trust is the least trusted thing that contributed to this memory, not
	// the trust of whoever approved it. It only ever travels downwards.
	Trust TrustLevel

	Origin        RunOrigin
	SourceSession SessionID
	SourceSeq     Seq

	// ApprovedBy is who let this in. Every memory has one: nothing is written
	// without a person, which is the whole design.
	ApprovedBy string

	CreatedAt time.Time

	// InvalidatedAt and SupersededBy record a fact that stopped being true.
	// It is not deleted: "what is true now" and "what was true then" are
	// different questions and both have answers.
	InvalidatedAt *time.Time
	SupersededBy  MemoryID
}

// IsCurrent reports whether a memory is still believed.
func (m Memory) IsCurrent() bool { return m.InvalidatedAt == nil }

// EventKind discriminates Event.Payload.
type EventKind string

const (
	EventUserMessageAdded          EventKind = "user.message"
	EventRunStateChanged           EventKind = "run.state_changed"
	EventAssistantTextDelta        EventKind = "assistant.delta"
	EventAssistantReasoningDelta   EventKind = "assistant.reasoning"
	EventAssistantMessageCompleted EventKind = "assistant.completed"
	EventUsageChanged              EventKind = "usage.changed"
	EventToolCallRequested         EventKind = "tool.requested"
	EventToolCallCompleted         EventKind = "tool.completed"
	EventConversationCompacted     EventKind = "conversation.compacted"
	EventRunDirections             EventKind = "run.directions"
	EventPlanChanged               EventKind = "plan.changed"
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

	// Attachments are what arrived with it, by reference. The bytes are in the
	// artifact store; the model is shown the images among them when a request
	// is assembled.
	Attachments []Attachment
}

type RunStateChanged struct {
	Status RunStatus

	// Reason is written for whoever is debugging, and may carry a provider's
	// own wording verbatim. That makes it right for a log and for a
	// control-plane client, and wrong for a chat channel other people read.
	Reason string

	// FailureKind names what went wrong in a form a client can branch on
	// without reading Reason. Empty when there is nothing to classify.
	//
	// It exists because the alternative is a client matching on English prose
	// written by somebody else's API, which changes without notice and turns
	// a presentation decision into a guess.
	FailureKind string
}

type AssistantTextDelta struct {
	MessageID MessageID
	Text      string
}

// AssistantReasoningDelta is a chunk of the model's working-out.
//
// A separate kind from the answer, and the separation is the point: this is
// not what the model is telling anybody. It never leaves the machine — the
// projector refuses it, so no chat platform can carry it, console channel
// included. A platform account is somebody's account, and the working-out
// quotes whatever the run has been reading.
//
// Not folded back into the conversation on the next turn either. A provider
// that needs its own reasoning returned carries it as an opaque block on the
// message it belongs to; this is for reading.
type AssistantReasoningDelta struct {
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

	// Artifact names the whole of what Content is an excerpt of, when the tool
	// stored one. A truncated result with nothing here is output that is gone.
	Artifact *Artifact

	DurationMS int64
}

// Artifact refers to content in the artifact store.
//
// It lives in the event rather than in a table of its own because the event
// already records which session, which run and which tool produced it. A
// second place holding the same fact is a second place for it to be wrong.
type Artifact struct {
	ID        string
	Size      int64
	MediaType string
}

// ConversationCompacted replaces the history up to ThroughSeq with a summary.
//
// It is an event rather than in-memory state because the conversation is
// rebuilt from this log. Holding the summary anywhere else would mean a
// restarted process replaying the history the summary was meant to replace,
// which is exactly the situation compaction exists to avoid.
//
// Nothing is deleted. The events before ThroughSeq stay in the log and every
// client can still read them; they are simply no longer sent to the model.
type ConversationCompacted struct {
	Summary string

	// ThroughSeq is the last event folded into the summary. Everything after
	// it is replayed as it was.
	ThroughSeq Seq

	// MessagesFolded and the estimates are for display: an operator seeing a
	// reply lose its memory deserves to know it happened and how much went.
	MessagesFolded int
	TokensBefore   int64
	TokensAfter    int64
}

// RunDirections records the part of the system prompt that was assembled for
// this run rather than fixed at startup.
//
// It is an event because it has to be, not because it is interesting. Standing
// directions come from memory, and memory changes; recomputing them when a run
// resumes after an approval — possibly in a different process, hours later —
// would mean the same run was given two different prompts and neither the log
// nor anybody reading it could say which.
//
// Everything else the model sees is already derivable from this log. This was
// the one thing that was not.
type RunDirections struct {
	Text string
}

// MessageRole says who said something, for a client drawing a conversation.
//
// Separate from the provider package's own role, which describes what a model
// is sent. This one describes what a person sees, and the two diverge: a tool
// result is a role to a provider and part of an assistant turn to a reader.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
)

// UsageChanged reports cumulative usage for the run so far, so cost is visible
// while a run is in flight rather than only once it ends.
type UsageChanged struct {
	Usage Usage
}

// AllEventKinds is every kind the log can hold.
//
// A kind has to be understood in more than one place — persisted by storage,
// translated by the wire format, rendered by a client — and the failure when
// one of those is missed is not a compile error. It is a run that works in
// memory and drops the event on a real database. This list is what the tests
// in those packages measure their coverage against.
func AllEventKinds() []EventKind {
	return []EventKind{
		EventUserMessageAdded,
		EventRunStateChanged,
		EventAssistantTextDelta,
		EventAssistantReasoningDelta,
		EventAssistantMessageCompleted,
		EventUsageChanged,
		EventToolCallRequested,
		EventToolCallCompleted,
		EventConversationCompacted,
		EventRunDirections,
		EventPlanChanged,
		EventApprovalRequested,
		EventApprovalResolved,
	}
}

func (UserMessageAdded) isEventPayload()          {}
func (RunStateChanged) isEventPayload()           {}
func (AssistantTextDelta) isEventPayload()        {}
func (AssistantReasoningDelta) isEventPayload()   {}
func (AssistantMessageCompleted) isEventPayload() {}
func (UsageChanged) isEventPayload()              {}
func (ToolCallRequested) isEventPayload()         {}
func (ToolCallCompleted) isEventPayload()         {}
func (ConversationCompacted) isEventPayload()     {}
func (RunDirections) isEventPayload()             {}
func (PlanChanged) isEventPayload()               {}

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

	// StopUnknown is a reason the provider gave that this build does not
	// recognise. It is its own value so that an unfamiliar one is not quietly
	// recorded as a normal ending, which would make a truncated answer
	// indistinguishable from a complete one.
	StopUnknown StopReason = "unknown"

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

	// Preview is the call rendered for somebody deciding — a diff for an
	// edit, the command line for an execution. Empty when the arguments are
	// already the clearest thing to show.
	//
	// Alongside Arguments rather than replacing them: the arguments are what
	// will actually run, and a decision made against a rendering that
	// disagreed with them would be a decision about something else.
	Preview string

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

	// Preview is the call rendered for review, where the tool could render
	// one. See Approval.Preview.
	Preview string
}

// PlanItem is one step of what the agent says it is going to do.
//
// The plan is agent state rather than something written into an answer. A
// model that describes its plan in prose has to find it again in its own
// output next turn, and a person watching has to read a paragraph to learn
// what is left — so it is neither reliable for the model nor legible for
// anybody else.
type PlanItem struct {
	ID     string
	Title  string
	Status PlanStatus

	// Note is why a step was abandoned, or anything the model wanted recorded
	// against it. Optional, and usually empty.
	Note string
}

// PlanStatus is where one step has got to.
type PlanStatus string

const (
	PlanPending    PlanStatus = "pending"
	PlanInProgress PlanStatus = "in_progress"
	PlanCompleted  PlanStatus = "completed"

	// PlanAbandoned is a step that will not be done, said rather than
	// deleted. A step that disappears reads as one that was finished.
	PlanAbandoned PlanStatus = "abandoned"
)

// IsTerminal reports whether a step is done with, either way.
func (s PlanStatus) IsTerminal() bool {
	return s == PlanCompleted || s == PlanAbandoned
}

// IsValid reports whether this is a state a step can actually be in.
func (s PlanStatus) IsValid() bool {
	switch s {
	case PlanPending, PlanInProgress, PlanCompleted, PlanAbandoned:
		return true
	default:
		return false
	}
}

// Mark is the short word shown against a step.
//
// On the status rather than in each place that draws one: the plan is
// rendered for the model, for a chat channel and for three clients, and four
// copies of the same switch is four places for "dropped" to quietly become
// "done".
func (s PlanStatus) Mark() string {
	switch s {
	case PlanCompleted:
		return "done"
	case PlanInProgress:
		return "doing"
	case PlanAbandoned:
		return "dropped"
	default:
		return "todo"
	}
}

// PlanOpRequest is one change to the plan, as asked for.
//
// Operations rather than the whole list, which is the difference between a
// plan that survives being edited and one that does not. Asked to rewrite the
// list, a model drops ids it does not think matter, revives steps it already
// finished, and reorders ones that were waiting on each other — and none of
// that is visible as an error, because a list is a valid list whatever is in
// it.
type PlanOpRequest struct {
	// Op is add, set_status or set_title.
	Op string

	// ID names an existing step, for everything but add.
	ID string

	// Title is the step's text, for add and set_title.
	Title string

	// Status is where it has got to, for set_status.
	Status PlanStatus

	// Note is why, usually only when abandoning something.
	Note string
}

// Plan operation names, as the tool's schema spells them.
const (
	PlanOpAdd       = "add"
	PlanOpSetStatus = "set_status"
	PlanOpSetTitle  = "set_title"
)

// PlanChanged carries the whole plan after a change, not the change itself.
//
// Whole on purpose. A client that joined late, or one resuming after events
// were pruned, reads one event and knows the plan; replaying a sequence of
// operations to arrive at it would make the plan the one thing in this log
// that cannot be understood from a single entry.
type PlanChanged struct {
	Items []PlanItem
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
