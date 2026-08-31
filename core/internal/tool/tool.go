// Package tool defines what a tool is and how one is invoked.
//
// The contract has one unusual property worth stating plainly: a tool that
// fails is not an error the runtime reports upward. It is an observation the
// model receives and can act on. Handing back "exit status 1" and expecting
// the model to divine what went wrong is how agents get stuck in loops; a
// structured error with a code and a suggested next step is how they recover.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Level is the base risk of a tool, before its arguments are considered.
//
// The level is only a floor. The same exec_command is harmless running a test
// suite and catastrophic running rm -rf, so the final decision belongs to a
// policy that sees the actual arguments.
type Level int

const (
	// LevelInternal only changes the agent's own state.
	LevelInternal Level = iota

	// LevelWorkspaceRead reads the workspace without side effects.
	LevelWorkspaceRead

	// LevelNetworkRead retrieves something from the network without changing
	// it: fetching a page, not filling in its form.
	//
	// It sits above reading the workspace because the bytes come back chosen
	// by somebody else, and below writing to it because nothing on this
	// machine changes. The risk it carries is not damage but influence: what
	// arrives is attacker-controlled text that the model will read as though
	// it were research.
	LevelNetworkRead

	// LevelWorkspaceWrite modifies the workspace.
	LevelWorkspaceWrite

	// LevelRemember writes something the agent will believe in later sessions.
	//
	// Its own level rather than a workspace write, because the reach is
	// different in kind: a bad edit is recoverable from the workspace's own
	// history, and a bad memory is read into every future conversation by an
	// agent that has forgotten where it came from. Both profiles stop for it.
	LevelRemember

	// LevelExecute runs programs or reaches the network.
	LevelExecute

	// LevelHighImpact touches things outside the workspace, deletes, deploys,
	// or changes system state.
	LevelHighImpact
)

func (l Level) String() string {
	switch l {
	case LevelInternal:
		return "internal"
	case LevelNetworkRead:
		return "network_read"
	case LevelWorkspaceRead:
		return "workspace_read"
	case LevelWorkspaceWrite:
		return "workspace_write"
	case LevelRemember:
		return "remember"
	case LevelExecute:
		return "execute"
	case LevelHighImpact:
		return "high_impact"
	default:
		return "unknown"
	}
}

// LevelByName resolves the name a configuration file uses.
//
// The names are the ones String produces, so what an operator writes and what
// a log prints are the same word.
func LevelByName(name string) (Level, bool) {
	for _, level := range []Level{
		LevelInternal, LevelWorkspaceRead, LevelNetworkRead,
		LevelWorkspaceWrite, LevelRemember, LevelExecute, LevelHighImpact,
	} {
		if level.String() == name {
			return level, true
		}
	}
	return LevelInternal, false
}

// Capabilities describes what a tool can reach. A policy engine reads these
// rather than pattern-matching on tool names.
type Capabilities struct {
	ReadFS  bool
	WriteFS bool
	Execute bool
	Network bool
	Secrets bool

	// Destructive marks effects that cannot be undone by running the tool
	// again with different arguments.
	Destructive bool

	// Idempotent means repeating the call with the same arguments has the same
	// effect as calling it once. Crash recovery depends on knowing this.
	Idempotent bool

	// ParallelSafe means concurrent invocations do not contend for the same
	// resource.
	ParallelSafe bool

	// ForeignContent means the result carries text somebody else wrote.
	//
	// Not the same as Network, and the two must not be merged. Network says
	// where a tool reaches, which is a fact about its power; this says whose
	// words come back, which is a fact about the answer. exec_command reaches
	// the network and returns the output of a program the operator chose to
	// run; web_read returns a page written by whoever owns the address.
	//
	// This used to carry a known hole, recorded here because this comment is
	// what the next person reads. The hole was that "a program the operator
	// chose to run" is true of the program and not of its output: a local run
	// could shell out to something that reaches the network and write what
	// came back into a memory that later carried standing authority — which
	// is the thing this field exists to prevent. Declaring it on exec_command
	// would have closed it and cost too much, because every run that listed a
	// directory would then have looked like one that read a stranger's page,
	// and a warning that is always on is one nobody reads.
	//
	// Provenance below closes it, by asking the same question at a
	// granularity that can tell those two apart.
	//
	// Kept alongside it rather than replaced by it, because they are read by
	// different people for different reasons. This one is shown to somebody
	// deciding whether to allow a call and has to stay rare to stay worth
	// reading; Provenance is the whole truth, and is consulted where the
	// whole truth is what matters.
	ForeignContent bool

	// Provenance says who wrote what this tool returns.
	//
	// The zero value is the operator, which is right for a tool that reads
	// nothing: todo_update returns what it was handed. Anything that reads
	// has to say so, and the honest answer for a command is local_unknown —
	// its output is from this machine and is nobody's request.
	//
	// This is what closed the hole the comment above used to describe. The
	// argument for leaving exec_command unmarked was that marking it would
	// cost too much: every run that listed a directory would look like a run
	// that had read a stranger's page, and a warning that is always on is one
	// nobody reads. That argument was about a single boolean. With two
	// values the cost disappears — listing a directory is local_unknown and
	// reading a page is external, and only the second is worth interrupting
	// somebody about.
	Provenance domain.Provenance
}

// Spec is everything the runtime and the model need to know about a tool.
type Spec struct {
	// Name is what the model calls. Kept in snake_case because provider
	// restrictions on tool names are not identical.
	Name string

	Description string

	// InputSchema is a JSON Schema object. It is both what the model is shown
	// and what arguments are validated against, so the two can never drift.
	InputSchema json.RawMessage

	Level        Level
	Capabilities Capabilities
}

// Call is one invocation requested by the model.
type Call struct {
	ID   string
	Name string

	// Arguments as the model produced them, before validation.
	Arguments json.RawMessage

	// Context is what the runtime knows about the turn this came from.
	//
	// Most tools ignore it: a file read is a file read whoever asked. It is
	// here for the ones where the answer depends on who is asking, which today
	// means memory — what one person told the agent is not recalled for
	// another, and that boundary has to be enforced where the reading happens.
	Context CallContext
}

// Previewer renders what a call will do, for somebody deciding whether to
// allow it.
//
// Optional, and implemented by the tools whose arguments a person cannot
// review as they stand. "edit_file with these 900 characters of old_text and
// these 950 of new_text" is not a thing anybody reads; the diff between them
// is, and it is the same review whichever client is showing it.
//
// Rendered in the daemon rather than by each client on purpose. A client would
// have to know the argument schema of every tool to do it, which means three
// implementations that drift, and a tool server's own tools would have none at
// all. Here the tool that defined the arguments is the one that explains them.
//
// An empty string means there is nothing better to show than the arguments.
type Previewer interface {
	// Preview must not touch anything. It runs before a decision has been
	// made, and a preview with a side effect is the call happening without
	// approval.
	Preview(arguments json.RawMessage) string
}

// CallContext is who and what a call belongs to.
type CallContext struct {
	SessionID string
	RunID     string

	// Trust is the least trusted thing that has reached the model in this run
	// before this call, and it only ever travels downwards.
	//
	// A turn typed here starts trusted and stops being so the moment the
	// model reads somebody else's words — a page, a tool server's output.
	// What the model writes after that may be its own conclusion or may be
	// the page talking, and nothing at this boundary can tell the two apart.
	//
	// Bounded by this call's own position: a memory written before a page was
	// fetched cannot have come from it.
	//
	// Read through TrustOrUntrusted, never directly. An unset level means
	// nobody worked it out, and the safe reading of that is the lowest one:
	// a caller that forgot to fill this in should get a memory it cannot
	// promote rather than one it should not have trusted.
	Trust domain.TrustLevel

	// From is the worst provenance this run has read before this call.
	//
	// Beside Trust rather than replacing it, and the pair is the point: Trust
	// says whether this turn may be believed, and this says whose words the
	// run has been handling. A local turn that ran a command is still the
	// operator asking, and what the command printed is still not the operator
	// speaking — one field could not say both.
	//
	// What consults it is a privileged sink: somewhere text stops being an
	// observation and becomes an instruction that shapes later runs.
	From domain.Provenance

	// Origin says whether the turn came from a control client on this machine
	// or through a gateway from somebody else's platform.
	Origin domain.RunOrigin

	// Seq is where in the session's log this call sits, so anything a tool
	// records can point back at what caused it.
	Seq domain.Seq
}

// TrustOrUntrusted is the trust of this call, failing closed.
//
// An unset level is not "trusted by default"; it is "nobody said". Guessing
// upwards there is the guess that costs the most when it is wrong, and it
// would be invisible: a memory written by a mis-wired path would look exactly
// like one the operator typed.
func (c CallContext) TrustOrUntrusted() domain.TrustLevel {
	if c.Trust == "" {
		return domain.TrustUntrusted
	}
	return c.Trust
}

// PrincipalKey identifies whoever is asking, as a scope for anything kept per
// person.
//
// A gateway principal and the operator of this machine are different people,
// and the key says which so that nothing collapses them together by accident.
func (c CallContext) PrincipalKey() string {
	if c.Origin.Principal != nil {
		return c.Origin.Principal.Platform + ":" + c.Origin.Principal.PrincipalID
	}
	return "local:" + c.Origin.ClientID
}

// FromGateway reports whether this turn arrived from outside.
func (c CallContext) FromGateway() bool {
	return c.Origin.Kind == domain.OriginGateway
}

// Result is what goes back to the model.
type Result struct {
	// Content is the model-visible text. It is deliberately separate from
	// whatever the tool produced in full: a search that matched ten thousand
	// lines must not put ten thousand lines into the context window.
	Content string

	// Summary is a short human-facing line for a UI timeline.
	Summary string

	// IsError marks a failed invocation. The content still goes to the model,
	// because that is how it learns what to do differently.
	IsError bool

	// Truncated records that Content is not the whole output, so a UI can say
	// so rather than implying the result was small.
	Truncated bool

	// OriginalBytes is the untruncated size, for the same reason.
	OriginalBytes int64

	// Artifact points at the whole of what Content is an excerpt of.
	//
	// Truncation without this is destruction: the model is told there was
	// more and given no way to reach it. With it, "the rest" is a tool call
	// away and a client can show the whole thing.
	Artifact *Artifact
}

// Artifact refers to stored content.
//
// It is a reference rather than the bytes because a Result travels through the
// event log and into a request to a model; the point of the artifact store is
// that the bytes do not go there.
type Artifact struct {
	ID        string
	Size      int64
	MediaType string
}

// Tool is a capability the model can invoke.
type Tool interface {
	Spec() Spec

	// Execute runs the tool. It returns an error only for failures the model
	// cannot act on; anything the model should see and retry differently
	// belongs in a Result with IsError set.
	Execute(ctx context.Context, call Call) (Result, error)
}

// Leveled is a tool whose risk depends on what it was asked to do.
//
// Spec.Level is a floor, and always was — the comment on Level has said so
// since it was written. Some tools are genuinely two different things
// depending on their arguments: writing something down that nobody will see
// unless they ask for it, against writing something that goes in front of the
// model on every future turn. A policy that cannot tell those apart has to
// treat the cheap one as expensive, and an approval that fires constantly is
// one people learn to click through.
//
// It may only raise. A tool that could lower its own level would be a tool
// deciding its own permissions.
type Leveled interface {
	LevelFor(call Call) Level
}

// EffectiveLevel is the level a call should actually be judged at.
func EffectiveLevel(t Tool, call Call) Level {
	level := t.Spec().Level

	if leveled, ok := t.(Leveled); ok {
		if raised := leveled.LevelFor(call); raised > level {
			return raised
		}
	}
	return level
}

// ErrorCode identifies a failure in a way the model can pattern-match on.
type ErrorCode string

const (
	CodeInvalidArguments ErrorCode = "invalid_arguments"
	CodeNotFound         ErrorCode = "not_found"
	CodePermissionDenied ErrorCode = "permission_denied"
	CodeOutsideWorkspace ErrorCode = "outside_workspace"
	CodeTooLarge         ErrorCode = "too_large"
	CodeUnsupported      ErrorCode = "unsupported"
	CodeTimeout          ErrorCode = "timeout"
	CodeInternal         ErrorCode = "internal"
)

// Error is a tool failure expressed for a model rather than for a log.
type Error struct {
	Code    ErrorCode
	Message string

	// SuggestedAction tells the model what to do next. Without it a model
	// tends to retry the identical call until it runs out of iterations.
	SuggestedAction string

	// Retryable distinguishes "this will never work" from "try again".
	Retryable bool

	// Artifact points at output the failure produced.
	//
	// A command that timed out or exited non-zero is exactly the one whose
	// output somebody wants, so a failure carries the reference as readily as
	// a success does.
	Artifact *Artifact
}

func (e *Error) Error() string {
	if e.SuggestedAction == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.SuggestedAction)
}

// Result renders the error as an observation for the model.
func (e *Error) Result() Result {
	payload := struct {
		Code            ErrorCode `json:"code"`
		Message         string    `json:"message"`
		SuggestedAction string    `json:"suggested_action,omitempty"`
		Retryable       bool      `json:"retryable"`
	}{e.Code, e.Message, e.SuggestedAction, e.Retryable}

	// Encoded as JSON so the model sees the same shape for every failure
	// rather than having to parse prose.
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Result{Content: e.Error(), Summary: e.Message, IsError: true, Artifact: e.Artifact}
	}

	return Result{
		Content:  string(encoded),
		Summary:  e.Message,
		IsError:  true,
		Artifact: e.Artifact,
	}
}

func Errorf(code ErrorCode, suggested string, format string, args ...any) *Error {
	return &Error{
		Code:            code,
		Message:         fmt.Sprintf(format, args...),
		SuggestedAction: suggested,
	}
}
