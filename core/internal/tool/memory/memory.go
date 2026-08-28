// Package memory gives the agent tools for what it should carry between
// sessions.
//
// Two rules shape everything here, and both come from what has actually gone
// wrong in shipped systems rather than from taste.
//
// Writing stops for a person. A memory is read into every later conversation
// by an agent that no longer knows where it came from, so an unattended write
// turns one piece of untrusted text — a Discord message, a file the run
// happened to read — into a permanent instruction. That is the difference
// between a prompt injection that ends with the session and one that does not.
//
// Reading is scoped by who is asking. What a Discord account told the agent is
// not recalled for the operator of this machine, and the operator's notes are
// not read out into a channel. Collapsing those two people into one is how a
// memory feature becomes a way to exfiltrate.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

const (
	// maxTextBytes bounds one memory. Something longer is a document, and a
	// document belongs in the workspace where it can be read on purpose.
	maxTextBytes = 2000

	defaultRecallLimit = 10
	maxRecallLimit     = 50
)

// Store is what these tools need from storage.
type Store interface {
	Remember(ctx context.Context, memory domain.Memory, supersedes domain.MemoryID) error
	Memories(ctx context.Context, query storage.MemoryQuery) ([]domain.Memory, error)
	SearchMemories(ctx context.Context, text string, query storage.MemoryQuery) ([]domain.Memory, error)
	Memory(ctx context.Context, id domain.MemoryID) (domain.Memory, error)
}

// Options are what both tools share.
type Options struct {
	Store Store

	// WorkspaceRef identifies the project these memories belong to.
	WorkspaceRef string

	NewID func() string
	Now   func() time.Time
}

// scopesFor decides which memories a turn may see.
//
// A turn always sees what the project knows and what the person driving it
// said. It never sees what a different person said: a Discord account and the
// operator of this machine are not the same principal, and the whole point of
// recording that is to be able to act on it here.
func (o Options) scopesFor(call tool.CallContext) []storage.MemoryScopeRef {
	return []storage.MemoryScopeRef{
		{Scope: domain.ScopeWorkspace, Ref: o.WorkspaceRef},
		{Scope: domain.ScopePrincipal, Ref: call.PrincipalKey()},
	}
}

// Remember writes something down for later sessions.
type Remember struct {
	Options
}

func (t *Remember) Spec() tool.Spec {
	return tool.Spec{
		Name: "remember",
		Description: "Write something down so later sessions know it. " +
			"For durable facts and preferences, not for notes about the task in hand: " +
			"this conversation already remembers itself. " +
			"A person is asked before anything is written.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {
      "type": "string",
      "minLength": 1,
      "description": "What to remember, in one or two sentences. State it plainly and completely; it will be read without this conversation around it."
    },
    "kind": {
      "type": "string",
      "enum": ["fact", "instruction"],
      "description": "A fact is looked up when wanted. An instruction is put in front of the model on every turn, so use it only for standing direction. Defaults to fact."
    },
    "scope": {
      "type": "string",
      "enum": ["workspace", "person"],
      "description": "workspace for something true of this project, person for something true of whoever is asking. Defaults to workspace."
    },
    "supersedes": {
      "type": "string",
      "description": "The id of a memory this corrects. Use it instead of writing a second, contradictory memory."
    }
  },
  "required": ["text"],
  "additionalProperties": false
}`),
		// Its own level, and one both profiles stop for. What is written here
		// is read by every later session.
		Level: tool.LevelRemember,
		Capabilities: tool.Capabilities{
			// Not idempotent: asking twice writes twice, which is why
			// corrections name what they replace.
			Destructive: true,
		},
	}
}

type rememberArgs struct {
	Text       string `json:"text"`
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	Supersedes string `json:"supersedes"`
}

func (t *Remember) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args rememberArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	text := strings.TrimSpace(args.Text)
	if text == "" {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "there is nothing to remember")
	}
	if len(text) > maxTextBytes {
		return tool.Result{}, tool.Errorf(tool.CodeTooLarge,
			"Say the durable part in a sentence or two; put anything longer in a file.",
			"a memory may be %d bytes and this is %d", maxTextBytes, len(text))
	}

	kind, err := memoryKind(args.Kind)
	if err != nil {
		return tool.Result{}, err
	}
	scope, ref, err := t.memoryScope(args.Scope, call.Context)
	if err != nil {
		return tool.Result{}, err
	}

	supersedes, err := t.checkSupersedes(ctx, args.Supersedes, call.Context)
	if err != nil {
		return tool.Result{}, err
	}

	written := domain.Memory{
		ID:       domain.MemoryID(t.NewID()),
		Scope:    scope,
		ScopeRef: ref,
		Kind:     kind,
		Text:     text,
		// The trust of the turn that produced it, which for anything arriving
		// through a gateway is the lowest there is — and stays that way
		// however many times it is summarised afterwards.
		Trust:         trustOf(call.Context),
		Origin:        call.Context.Origin,
		SourceSession: domain.SessionID(call.Context.SessionID),
		SourceSeq:     call.Context.Seq,
		ApprovedBy:    approverOf(call.Context),
		CreatedAt:     t.Now(),
	}

	if err := t.Store.Remember(ctx, written, supersedes); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	summary := fmt.Sprintf("remembered as %s", written.ID)
	if supersedes != "" {
		summary = fmt.Sprintf("replaced %s with %s", supersedes, written.ID)
	}

	return tool.Result{
		Content: fmt.Sprintf("%s (%s, %s)\n%s", summary, kind, scope, text),
		Summary: summary,
	}, nil
}

// checkSupersedes refuses a correction to something the caller cannot see.
//
// Without this, naming an id is a way to invalidate another person's memory
// without ever being able to read it.
func (t *Remember) checkSupersedes(
	ctx context.Context,
	id string,
	call tool.CallContext,
) (domain.MemoryID, error) {
	if id == "" {
		return "", nil
	}

	existing, err := t.Store.Memory(ctx, domain.MemoryID(id))
	if errors.Is(err, storage.ErrMemoryNotFound) {
		return "", tool.Errorf(tool.CodeNotFound,
			"Use an id that recall returned.", "there is no memory %s", id)
	}
	if err != nil {
		return "", tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	for _, scope := range t.scopesFor(call) {
		if existing.Scope == scope.Scope && existing.ScopeRef == scope.Ref {
			return domain.MemoryID(id), nil
		}
	}

	// Same answer as a missing one, so that guessing ids tells nobody whether
	// they guessed right.
	return "", tool.Errorf(tool.CodeNotFound,
		"Use an id that recall returned.", "there is no memory %s", id)
}

func (t *Remember) memoryScope(
	requested string,
	call tool.CallContext,
) (domain.MemoryScope, string, error) {
	switch requested {
	case "", "workspace":
		return domain.ScopeWorkspace, t.WorkspaceRef, nil
	case "person":
		return domain.ScopePrincipal, call.PrincipalKey(), nil
	default:
		return "", "", tool.Errorf(tool.CodeInvalidArguments,
			"Use workspace or person.", "%q is not a scope", requested)
	}
}

func memoryKind(requested string) (domain.MemoryKind, error) {
	switch requested {
	case "", "fact":
		return domain.MemoryFact, nil
	case "instruction":
		return domain.MemoryInstruction, nil
	default:
		return "", tool.Errorf(tool.CodeInvalidArguments,
			"Use fact or instruction.", "%q is not a kind of memory", requested)
	}
}

// trustOf is the trust a memory written from this turn carries.
//
// Anything arriving through a gateway is untrusted whatever it says about
// itself, and that never improves: a fact derived from untrusted text is
// untrusted however many summaries it has been through.
func trustOf(call tool.CallContext) domain.TrustLevel {
	if call.FromGateway() {
		return domain.TrustUntrusted
	}
	return domain.TrustUser
}

// approverOf records who let it in.
//
// The approval itself happens in the permission engine before this runs; what
// is recorded here is which client the turn belonged to, so a listing can say
// where each memory came from.
func approverOf(call tool.CallContext) string {
	if call.FromGateway() {
		return "operator (proposed from " + call.PrincipalKey() + ")"
	}
	if call.Origin.ClientID != "" {
		return call.Origin.ClientID
	}
	return "operator"
}

// Recall looks up what was remembered.
type Recall struct {
	Options
}

func (t *Recall) Spec() tool.Spec {
	return tool.Spec{
		Name: "recall",
		Description: "Look up what was remembered in earlier sessions about this project " +
			"or about the person asking. Returns each memory with an id, which is what a " +
			"correction names.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Words to look for. Omit to list what is remembered, newest first."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "description": "How many to return. Defaults to 10."
    }
  },
  "additionalProperties": false
}`),
		// It reads back what this agent was told, within the boundary of who
		// is asking. That grants no reach the turn did not already have.
		Level: tool.LevelInternal,
		Capabilities: tool.Capabilities{
			Idempotent:   true,
			ParallelSafe: true,
		},
	}
}

type recallArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (t *Recall) Execute(ctx context.Context, call tool.Call) (tool.Result, error) {
	var args recallArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInvalidArguments, "", "%v", err)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	if limit > maxRecallLimit {
		limit = maxRecallLimit
	}

	// The scopes are decided here and never taken from the arguments. A model
	// that could name its own scope could ask for somebody else's memories.
	query := storage.MemoryQuery{
		Scopes: t.scopesFor(call.Context),
		Limit:  limit,
	}

	var (
		found []domain.Memory
		err   error
	)
	if strings.TrimSpace(args.Query) == "" {
		found, err = t.Store.Memories(ctx, query)
	} else {
		found, err = t.Store.SearchMemories(ctx, args.Query, query)
	}
	if err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	if len(found) == 0 {
		return tool.Result{
			Content: "Nothing has been remembered that matches.",
			Summary: "recall: nothing",
		}, nil
	}

	return tool.Result{
		Content: render(found),
		Summary: fmt.Sprintf("recall: %d", len(found)),
	}, nil
}

// render lists memories with where each came from.
//
// The provenance is shown rather than kept for auditing alone: a model reading
// "somebody on Discord said this" should weigh it differently from something
// the operator wrote, and it can only do that if it is told.
func render(memories []domain.Memory) string {
	var out strings.Builder

	for _, memory := range memories {
		fmt.Fprintf(&out, "%s [%s, %s", memory.ID, memory.Kind, memory.Scope)
		if memory.Trust == domain.TrustUntrusted {
			fmt.Fprintf(&out, ", from outside this machine")
		}
		fmt.Fprintf(&out, "]\n%s\n\n", memory.Text)
	}

	return strings.TrimRight(out.String(), "\n")
}

// Instructions renders the standing directions a run should start with.
//
// Only instruction-kind memories, only from scopes this run may see, and only
// as much as the bound allows: everything put here is context the work does not
// get, so it has to earn its place.
//
// Anything that arrived from outside this machine is excluded, however it was
// approved. An untrusted memory can be looked up deliberately, with a label
// saying where it came from; putting one in front of the model on every turn
// is how a message somebody sent once becomes a standing instruction.
func Instructions(
	ctx context.Context,
	store Store,
	options Options,
	run domain.Run,
	maxBytes int,
) (string, error) {
	if maxBytes <= 0 {
		return "", nil
	}

	found, err := store.Memories(ctx, storage.MemoryQuery{
		Scopes: options.scopesFor(contextForRun(run)),
		Kind:   domain.MemoryInstruction,
		Limit:  maxRecallLimit,
	})
	if err != nil {
		return "", err
	}

	var (
		out   strings.Builder
		wrote int
	)
	for _, instruction := range found {
		if instruction.Trust == domain.TrustUntrusted {
			continue
		}

		line := "- " + strings.TrimSpace(instruction.Text) + "\n"
		if wrote+len(line) > maxBytes {
			break
		}
		out.WriteString(line)
		wrote += len(line)
	}

	if wrote == 0 {
		return "", nil
	}

	return "Standing directions you were given in earlier sessions:\n\n" +
		strings.TrimRight(out.String(), "\n"), nil
}

// contextForRun is the same scoping a tool call would get, for a run that has
// not made one yet.
func contextForRun(run domain.Run) tool.CallContext {
	return tool.CallContext{
		SessionID: string(run.SessionID),
		RunID:     string(run.ID),
		Origin:    run.Origin,
	}
}
