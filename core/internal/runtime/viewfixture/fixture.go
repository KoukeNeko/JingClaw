// Package viewfixture holds the cases every client must agree on.
//
// Three clients turn the same event log into the same screen, in three
// languages. Nothing stops them drifting except a shared set of examples: an
// event sequence, and the state it must produce. A client that disagrees with
// one of these is wrong, whichever client it is.
//
// The events and the state are written as the wire format rather than as
// anything invented here, because that is already the contract all three
// speak. A fixture nobody can parse without a Go dependency is a fixture only
// Go can check.
package viewfixture

import (
	"encoding/json"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Case is one agreed example.
type Case struct {
	// Name says what the case is about, so a failure names the disagreement
	// rather than an index.
	Name string `json:"name"`

	// Why explains what a client would get wrong without it. A fixture whose
	// purpose nobody remembers is one somebody eventually deletes.
	Why string `json:"why"`

	Events []Event `json:"events"`

	// Expected is the state a client must be in after applying every event.
	Expected State `json:"expected"`
}

// Event is one log entry, in the shape a client receives it.
type Event struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// State is what a client shows.
//
// Deliberately small: the parts three clients must agree on, not everything
// any one of them displays. A field here is a promise that Swift, TypeScript
// and Go all compute it the same way.
type State struct {
	Messages []Message `json:"messages"`

	// PendingApprovals are the ids still waiting, in the order raised.
	PendingApprovals []string `json:"pending_approvals"`

	// ActiveRun is the run in flight, empty when none is.
	ActiveRun string `json:"active_run"`

	// HeadSeq is the last event accounted for, which is where a client
	// resumes.
	HeadSeq uint64 `json:"head_seq"`
}

// Message is one turn as a client draws it.
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`

	// Reasoning is the model's working-out for this turn, where a provider
	// exposed it and the client is one allowed to see it.
	//
	// Its own field rather than part of Text, and the separation is the whole
	// point: a client that joined them would show the working-out as the
	// answer, and a client that forwards the answer would forward it too.
	Reasoning string `json:"reasoning,omitempty"`

	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one tool a turn asked for.
type ToolCall struct {
	Name string `json:"name"`

	// Completed and IsError are what a client shows about how it ended.
	Completed bool `json:"completed"`
	IsError   bool `json:"is_error"`
}

// Roles as clients name them.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// RoleOf renders a domain role for a fixture.
func RoleOf(role domain.MessageRole) string {
	if role == domain.RoleUser {
		return RoleUser
	}
	return RoleAssistant
}
