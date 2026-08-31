// Package domain holds the core types of the agent runtime.
//
// Nothing here may import generated protobuf code. The runtime must never
// learn the wire format: swapping Connect for another transport, or adding a
// second one, has to be possible without touching this package.
package domain

import (
	"errors"
	"time"
)

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

	// OriginSchedule is a run nobody asked for just now: a schedule came due.
	//
	// Its own kind rather than the creator's, and that is the whole design.
	// Creating a schedule is delegation; running one is not impersonation. A
	// run that carried the authority of whoever set it up would be that
	// person still acting at three in the morning, and every later question
	// about what may be done unattended would have to be argued against an
	// architecture that had already answered it.
	OriginSchedule RunOriginKind = "schedule"
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

// RunOrigin says where something came from and, when that is knowable, who
// was behind it.
//
// Two things rather than one, because they are two different facts and only
// one of them is a claim about a person. A platform identifies whoever pressed
// a button; a loopback credential identifies the machine and says nothing
// about who is at it. Written as one string, the second case has nowhere
// honest to go, and what ends up in the field meaning "who" is the name of a
// program.
//
// Principal is nil exactly when nobody is known. That is a fact worth
// recording rather than a gap to fill in.
type RunOrigin struct {
	Kind      RunOriginKind
	ClientID  string
	Principal *ExternalPrincipal
}

// FromTheMachine is a decision made by somebody with local access.
//
// No principal. The loopback credential authenticates the machine, and every
// process running as its owner holds it, so which person typed the command is
// not something this program knows.
func FromTheMachine(client string) RunOrigin {
	return RunOrigin{Kind: OriginLocalClient, ClientID: client}
}

// FromASchedule is a run that came due.
//
// Who set it up is on the schedule, not here. This says who is acting, and
// the answer is the schedule itself.
func FromASchedule(id ScheduleID) RunOrigin {
	return RunOrigin{Kind: OriginSchedule, ClientID: string(id)}
}

// FromAPlatformAccount is a decision made by somebody the platform named.
func FromAPlatformAccount(platform, principalID, displayName string) RunOrigin {
	return RunOrigin{
		Kind: OriginGateway,
		Principal: &ExternalPrincipal{
			Platform:    platform,
			PrincipalID: principalID,
			DisplayName: displayName,
		},
	}
}

// FromAChannel is a decision made in a room that is itself the credential.
//
// For a console binding, where a typed command is enough and the platform is
// not asked who typed it. The room is what was authorised, so the room is what
// is recorded — not the platform's name, which would say only that somebody
// somewhere on Discord decided this.
func FromAChannel(platform, channelID string) RunOrigin {
	return RunOrigin{
		Kind:     OriginGateway,
		ClientID: platform + ":" + channelID,
	}
}

// Describe is the one-line form, for a person reading a log or a listing.
//
// Never empty: an origin that says nothing would read as a decision nobody
// made.
func (o RunOrigin) Describe() string {
	if o.Principal != nil {
		if o.Principal.DisplayName != "" {
			return o.Principal.DisplayName
		}
		if o.Principal.PrincipalID != "" {
			return o.Principal.Platform + ":" + o.Principal.PrincipalID
		}
	}
	if o.ClientID != "" {
		return o.ClientID
	}
	if o.Kind != "" {
		return string(o.Kind)
	}
	return "unrecorded"
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

	// Kind says whether this run is the conversation or work done inside it.
	Kind RunKind

	// ParentRunID is the run that asked for this one, for a worker.
	//
	// Recorded rather than inferred, because after a crash the question is
	// which run was waiting on which — and the alternative is guessing it
	// from a parent's unfinished tool call.
	ParentRunID RunID
}

// RunKind separates a turn of the conversation from work done inside one.
type RunKind string

const (
	// RunConversation is a turn somebody asked for. Empty means this, so
	// every run written before the distinction existed is one.
	RunConversation RunKind = ""

	// RunWorker is a bounded piece of work a run delegated.
	//
	// Its own run rather than more events in the parent's, because a run is
	// one agent trajectory and two of them in one would make resuming, usage
	// and status all branch-aware. And its events are deliberately not part
	// of the conversation: keeping a search's hundred tool results out of
	// what the model has to read again is the whole reason to delegate it.
	RunWorker RunKind = "worker"
)

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

	// From is whose words this came out of.
	//
	// The question Trust cannot answer. A local turn that ran a command is
	// trusted — the operator asked for it — and what the command printed is
	// still not the operator speaking. Only this can tell a standing
	// instruction the operator dictated from one the model read somewhere and
	// wrote down.
	From Provenance

	Origin        RunOrigin
	SourceSession SessionID
	SourceSeq     Seq

	// ApprovedBy is who let this in. Every memory has one: nothing is written
	// without a person, which is the whole design.
	ApprovedBy string

	// Two timelines, and conflating them is the mistake this exists to avoid.
	//
	// CreatedAt and InvalidatedAt are record time: when this agent learned
	// something and when it stopped believing it. ValidFrom and ValidUntil
	// are valid time: when the thing was true in the world.
	//
	// They come apart constantly. "The API was v1 until March" is a fact
	// learned in June about a period that ended in March, and a store with
	// one timeline has to pick which of those two dates to lose.
	CreatedAt time.Time

	// InvalidatedAt and SupersededBy record the moment this agent stopped
	// believing something. It is not deleted: "what is true now" and "what
	// was true then" are different questions and both have answers.
	InvalidatedAt *time.Time
	SupersededBy  MemoryID

	// ValidFrom is when the thing became true. Defaults to when it was
	// learned, which is the honest answer when nobody said otherwise.
	ValidFrom time.Time

	// ValidUntil is when it stopped being true, where that is known in
	// advance — a freeze that ends on a date, a version supported until a
	// release. Nil means it is still true, or nobody said.
	//
	// Distinct from InvalidatedAt: a fact that expires on Friday is not one
	// this agent was wrong about, and a correction is not a thing that was
	// scheduled.
	ValidUntil *time.Time
}

// IsCurrent reports whether a memory is still believed.
//
// Believed, not merely present: a memory that was superseded, that expired,
// or whose validity has run out is all still in the store, and none of them
// should be put in front of a model as though it were true now.
func (m Memory) IsCurrent() bool { return m.CurrentAt(time.Now()) }

// CurrentAt reports whether a memory was believed at a moment.
//
// Takes the time rather than reading the clock, because "was this true when
// that run happened" is a question worth being able to ask, and a function
// that reads the clock cannot answer it.
func (m Memory) CurrentAt(at time.Time) bool {
	if m.InvalidatedAt != nil && !at.Before(*m.InvalidatedAt) {
		return false
	}
	if !m.ValidFrom.IsZero() && at.Before(m.ValidFrom) {
		return false
	}
	if m.ValidUntil != nil && !at.Before(*m.ValidUntil) {
		return false
	}
	return true
}

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
	EventQuestionAsked             EventKind = "question.asked"
	EventQuestionAnswered          EventKind = "question.answered"
	EventSkillActivated            EventKind = "skill.activated"
)

// FoldNotice stands where a compaction folded turns away.
//
// One string for every client that draws a session, because the alternative
// is each of them wording it differently and a reader learning a new phrase
// per surface for the same thing. What it has to convey is that the turns
// were replaced rather than lost — a session that simply started shorter
// reads as the agent having forgotten.
const FoldNotice = "[earlier turns were folded into a summary]"

// ToolCallID identifies one tool invocation within a run.
type ToolCallID string

type Event struct {
	ID        EventID
	SessionID SessionID
	RunID     RunID
	Seq       Seq

	// GlobalSeq is where this event sits in the whole log, as against Seq,
	// which is where it sits in its own session.
	//
	// For anything watching every session at once, which cannot resume from
	// Seq: two sessions both at 50 make "I have read up to 50" mean nothing.
	// Zero on an event that came from somewhere with no whole log to be
	// positioned in.
	GlobalSeq Seq

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

	// Foreign says the result carried text somebody else wrote — a page, a
	// tool server's output.
	//
	// Recorded on the event rather than looked up from the tool's spec when
	// it is wanted. A tool removed from the configuration would otherwise
	// make an old event unclassifiable, and "was this run reading somebody
	// else's words" is a question about history that history should answer.
	Foreign bool

	// From says who wrote what came back: nothing for the operator's own
	// words, local_unknown for this machine, external for somebody else's.
	//
	// Recorded for the same reason as Foreign and answering a wider question.
	// Foreign is shown to a person deciding whether to allow a call, and has
	// to stay rare to stay worth reading; this is what a privileged sink
	// consults, where the honest answer about a command's output matters
	// even though it is nothing to interrupt anybody about.
	From Provenance

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
		EventQuestionAsked,
		EventQuestionAnswered,
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
func (QuestionAsked) isEventPayload()             {}
func (QuestionAnswered) isEventPayload()          {}

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

	// ReadForeign says this run had taken in text somebody else wrote before
	// it asked for this.
	//
	// Not a judgement about the call, which may be perfectly ordinary. It is
	// the one thing a person deciding cannot see for themselves: the request
	// in front of them looks the same whether the agent thought of it or a
	// web page suggested it, and only the log knows which.
	//
	// It does not gate anything. What it does is put the question in front of
	// the person already being asked one.
	ReadForeign bool

	Status ApprovalStatus
	Scope  RememberScope

	CreatedAt time.Time
	DecidedAt *time.Time

	// DecidedBy is who allowed or refused it, and from where.
	DecidedBy RunOrigin
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

	// ReadForeign says this run had taken in text somebody else wrote before
	// it asked for this. See Approval.ReadForeign.
	ReadForeign bool
}

// ErrApprovalDecided is returned when a decision arrives for something that
// has already been settled, which in practice means two people answered the
// same prompt at the same moment.
//
// Here rather than in the runtime because every caller that has to tell the
// second person what happened would otherwise have to depend on the runtime
// to name it, and a gateway that imports the agent loop to read one error
// value has imported the agent loop.
var ErrApprovalDecided = errors.New("domain: approval has already been decided")

// QuestionID identifies one thing the agent asked a person.
type QuestionID string

// Question is a paused run waiting for an answer from a person.
//
// Not an approval. An approval asks whether something may happen and is
// answered yes or no; this asks what the person wants and is answered with
// their words. Sharing the machinery would mean one of the two pretending to
// be the other, and the difference matters at exactly the moment it is being
// read: "allow this?" and "which of these?" are not the same question.
//
// Persisted for the same reason approvals are: the pause has to survive a
// restart. A run stopped on a question is not an orphan to clean up, it is
// work waiting for an answer that may come hours later.
type Question struct {
	ID        QuestionID
	SessionID SessionID
	RunID     RunID

	// ToolCallID ties this to the call that asked, so a resumed run finds the
	// answer for the call it is settling rather than for some other question.
	ToolCallID ToolCallID

	// Prompt is what is being asked, in the model's own words.
	Prompt string

	Kind QuestionKind

	// Options are what may be chosen, for a choice. Empty for free text.
	Options []QuestionOption

	Status QuestionStatus

	// Answer is what came back: the chosen option's id for a choice, or the
	// text as typed.
	Answer string

	// AnsweredBy is who answered it, and from where.
	AnsweredBy RunOrigin

	CreatedAt  time.Time
	AnsweredAt *time.Time
}

// IsPending reports whether this is still waiting on somebody.
func (q Question) IsPending() bool { return q.Status == AnswerPending }

// QuestionKind is what shape of answer is wanted.
type QuestionKind string

const (
	// QuestionChoice offers a fixed set. A model that knows the options
	// should say so rather than hoping the answer matches one.
	QuestionChoice QuestionKind = "choice"

	// QuestionText asks for words — a branch name, a path, a reason.
	QuestionText QuestionKind = "text"
)

// IsValid reports whether this is a kind of question that can be asked.
func (k QuestionKind) IsValid() bool {
	return k == QuestionChoice || k == QuestionText
}

// QuestionOption is one thing that may be chosen.
type QuestionOption struct {
	ID    string
	Label string

	// Detail is what a person needs to tell two options apart when the labels
	// alone do not. Optional.
	Detail string
}

// QuestionStatus is where a question has got to.
//
// Named for the answer rather than for the question — AnswerGiven rather than
// QuestionAnswered — because QuestionAnswered is the event that says it
// happened, and one name for both would make "the status" and "the thing that
// announced it" indistinguishable at every call site.
type QuestionStatus string

const (
	AnswerPending QuestionStatus = "pending"
	AnswerGiven   QuestionStatus = "answered"

	// AnswerAbandoned is a question nobody will answer, because the run it
	// belonged to ended. Recorded rather than deleted: a question that
	// vanishes leaves a log where a run paused for no visible reason.
	AnswerAbandoned QuestionStatus = "cancelled"
)

// QuestionAsked is the run stopping to ask. Every client sees it, so whoever
// is nearest can answer.
type QuestionAsked struct {
	QuestionID QuestionID
	CallID     ToolCallID
	Prompt     string
	Kind       QuestionKind
	Options    []QuestionOption
}

// QuestionAnswered records the answer and who gave it.
type QuestionAnswered struct {
	QuestionID QuestionID
	CallID     ToolCallID
	Status     QuestionStatus
	Answer     string
	AnsweredBy RunOrigin
}

// SkillActivated records that the model asked for an installed skill and was
// given its instructions.
//
// So that "why did it decide to run that" can be answered afterwards with the
// instructions it was following, rather than with a guess about which file
// was on disk at the time.
type SkillActivated struct {
	Name string

	// Version is what the file claims, for a person reading a listing.
	Version string

	// Digest is what was actually read, and is the identity. A file edited
	// without touching its version line is different instructions wearing the
	// same number, and only this tells them apart.
	Digest string
}

func (SkillActivated) isEventPayload() {}

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
	DecidedBy  RunOrigin
}

func (ApprovalRequested) isEventPayload() {}
func (ApprovalResolved) isEventPayload()  {}

// ScheduleID names a schedule.
type ScheduleID string

// Schedule is a standing instruction to do something at a time.
//
// Not a run, and not a session: it is the thing that makes them. What it
// holds is everything needed to decide, from the log alone, which firings are
// owed — because a timer inside a process is not the truth. A laptop that
// slept through three in the morning has to work out on waking what it
// missed, and a process that only knows "the next tick is in sixty seconds"
// cannot.
type Schedule struct {
	ID ScheduleID

	// Revision counts changes to when or what this does.
	//
	// Part of a firing's identity, so that editing a schedule cannot make an
	// already-resolved firing look unresolved and run a second time.
	Revision int

	// Expression is the five-field cron the firings come from, and Zone the
	// place its hours are in.
	//
	// A zone by name rather than an offset: "every day at nine" means nine
	// where somebody is, and an offset is wrong twice a year.
	Expression string
	Zone       string

	// Prompt is what the agent is asked, each time.
	Prompt string

	// SessionID is the conversation the firings belong to.
	//
	// One session per schedule rather than one per firing, so a daily
	// question can be answered with reference to yesterday's answer. What
	// stops it growing forever is the same compaction every other session
	// gets.
	SessionID SessionID

	// CreatedBy is who set this up. Attribution, and only that: it is not who
	// acts when the schedule fires.
	CreatedBy RunOrigin

	// Deliver is where the answer goes, said outright.
	//
	// Not inferred from where the schedule was created. A schedule made in a
	// channel may default to that channel, but "made here once" is not the
	// same statement as "answer here every day for a year", and only the
	// second one should be able to send messages somewhere.
	//
	// Empty delivers nowhere, which is not the same as producing nothing: the
	// run and its answer are in the log either way.
	Deliver []DeliveryTarget

	// Missed says what to do about firings that came due while nothing was
	// running.
	Missed MissedPolicy

	// Paused stops it firing without forgetting it.
	Paused bool

	CreatedAt time.Time
}

// MissedPolicy is what happens to firings nothing was there for.
type MissedPolicy string

const (
	// MissedCoalesce runs once, however many were missed.
	//
	// The default, and what launchd and systemd both do: a machine that slept
	// through four hourly firings wakes up owing one answer, not four agents
	// arriving at once to do yesterday's work.
	MissedCoalesce MissedPolicy = ""

	// MissedSkip runs nothing that was missed, and waits for the next one.
	//
	// For a question whose answer is only about now. "Is the disk full" asked
	// about eleven o'clock last night is not worth the tokens.
	MissedSkip MissedPolicy = "skip"
)

// Firing is one occasion a schedule was due.
//
// Identified by the schedule, its revision and the time it was due — never by
// when it actually ran. That triple is what makes reconciling idempotent: a
// daemon restarting, or waking, works out what is owed from the log and
// cannot create a second run for a firing already resolved.
type Firing struct {
	ScheduleID ScheduleID
	Revision   int

	// For is the time this firing was due, which is a fact about the
	// schedule. Observed is when something noticed, which is a fact about the
	// machine. On a laptop that slept they are hours apart, and confusing
	// them is how a log stops being able to say what happened.
	For      time.Time
	Observed time.Time

	// Missed counts firings coalesced into this one, so an answer that is
	// late can say so.
	Missed int

	// RunID is the run this became, once there is one.
	RunID RunID
}

// Provenance says who wrote a piece of text, which is a different question
// from whether it may be believed.
//
// The two were one field for a while, and the field was wrong. "Is this
// foreign" cannot separate a compiler diagnostic from a web page: neither was
// written by the operator, but only one of them is somebody else's words. So
// a run that listed a directory looked exactly like a run that read a
// stranger's page, and the only way to keep the warning meaningful was to not
// raise it for commands at all — which is the hole this closes.
//
// The invariant, which is what all of this is for:
//
//	Text the model can see may lose authority, and may never gain it
//	without an explicit trusted principal.
//
// A summary, a rewrite, a cat, a curl, another model, a skill — every one of
// them can only hold or lower what it was given. The only things that raise
// it are a person saying so and the runtime's own policy.
type Provenance string

const (
	// ProvenanceOperator is the operator's own words: what they typed, and
	// the files they wrote to be read as instructions.
	//
	// The zero value, because a tool that reads nothing produces nothing to
	// doubt. A tool that does read has to say so.
	ProvenanceOperator Provenance = ""

	// ProvenanceLocalUnknown is text from this machine that nobody vouched
	// for: a file in the workspace, the output of a command.
	//
	// Not suspicious, and not the operator either. The output of `ls` is
	// perfectly honest and is still not somebody asking for something, which
	// is the distinction that matters at a privileged sink.
	ProvenanceLocalUnknown Provenance = "local_unknown"

	// ProvenanceExternal is somebody else's words: a page, a tool server's
	// answer, a message from a platform.
	ProvenanceExternal Provenance = "external"
)

// worseThan orders provenance from most to least accountable, so that a run
// can carry the worst of what it has read.
var worseThan = map[Provenance]int{
	ProvenanceOperator:     0,
	ProvenanceLocalUnknown: 1,
	ProvenanceExternal:     2,
}

// Worse is whichever of two provenances is less accountable.
//
// A run takes the worst of everything it has read, and never improves: that
// is the invariant above, expressed as a function.
func (p Provenance) Worse(other Provenance) Provenance {
	if worseThan[other] > worseThan[p] {
		return other
	}
	return p
}

// IsOperator reports whether this is the operator's own words.
//
// Asked rather than compared, because the zero value means operator and a
// comparison against the constant reads as though it means "unset".
func (p Provenance) IsOperator() bool { return p == ProvenanceOperator }

// Describe is what to tell a person who is being asked to allow something.
func (p Provenance) Describe() string {
	switch p {
	case ProvenanceExternal:
		return "this run has read text from outside this machine"
	case ProvenanceLocalUnknown:
		return "this run has read files or command output from this machine"
	default:
		return ""
	}
}
