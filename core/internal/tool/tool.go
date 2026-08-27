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

	// LevelWorkspaceWrite modifies the workspace.
	LevelWorkspaceWrite

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
	case LevelWorkspaceRead:
		return "workspace_read"
	case LevelWorkspaceWrite:
		return "workspace_write"
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
		LevelInternal, LevelWorkspaceRead, LevelWorkspaceWrite, LevelExecute, LevelHighImpact,
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
}

// Tool is a capability the model can invoke.
type Tool interface {
	Spec() Spec

	// Execute runs the tool. It returns an error only for failures the model
	// cannot act on; anything the model should see and retry differently
	// belongs in a Result with IsError set.
	Execute(ctx context.Context, call Call) (Result, error)
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
		return Result{Content: e.Error(), Summary: e.Message, IsError: true}
	}

	return Result{
		Content: string(encoded),
		Summary: e.Message,
		IsError: true,
	}
}

func Errorf(code ErrorCode, suggested string, format string, args ...any) *Error {
	return &Error{
		Code:            code,
		Message:         fmt.Sprintf(format, args...),
		SuggestedAction: suggested,
	}
}
