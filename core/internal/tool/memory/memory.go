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
	"log/slog"
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
	TouchMemories(ctx context.Context, ids []domain.MemoryID, at time.Time) error
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

	// Clock is what these tools read the time from. Left nil, the wall
	// clock is used.
	//
	// One field read through one accessor: a memory's validity has to be
	// judged against the same moment as its expiry, and two places reading
	// two clocks is how those come apart.
	Clock func() time.Time

	// Log is where a failure that must not stop the work is reported. Left
	// nil, those go to the default logger.
	Log *slog.Logger

	// RetrievalTTL is how long a retrieval memory stays offered without
	// being used. Zero means they never expire.
	//
	// Inactivity rather than age: a convention this project has followed for
	// a year is not stale, and a note about a branch that was merged last
	// week is. Age cannot tell those apart and use can.
	RetrievalTTL time.Duration
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

// Now is the clock these tools read.
//
// One accessor rather than each caller checking for nil: a memory's validity
// has to be judged against the same moment as its expiry, and two places
// reading two clocks is how those come apart.
func (o Options) Now() time.Time {
	if o.Clock != nil {
		return o.Clock()
	}
	return time.Now()
}

// Logger is where a best-effort failure goes.
func (o Options) Logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
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
      "description": "What to remember, as one self-contained claim. State it plainly and completely; it will be read without this conversation around it. One claim per memory: a memory that says three things cannot be corrected without discarding the two that were still true."
    },
    "activation": {
      "type": "string",
      "enum": ["retrieval", "standing"],
      "description": "retrieval is looked up when it is wanted, and is what almost everything should be. standing is put in front of the model on every future turn, shapes every run, and needs a person to agree. Defaults to retrieval."
    },
    "scope": {
      "type": "string",
      "enum": ["workspace", "person"],
      "description": "workspace for something true of this project, person for something true of whoever is asking. Defaults to workspace."
    },
    "supersedes": {
      "type": "string",
      "description": "The id of a memory this corrects. Use it instead of writing a second, contradictory memory."
    },
    "valid_until": {
      "type": "string",
      "description": "When this stops being true, as a date (2026-09-15) or an RFC 3339 time. Only for something you already know has an end — a freeze that lifts, a version supported until a release. Leave it out for anything that is simply true."
    }
  },
  "required": ["text"],
  "additionalProperties": false
}`),
		// The floor is what a retrieval-only write costs: it changes the
		// agent's own state and nothing else until somebody asks for it.
		// LevelFor raises this to LevelRemember for a standing memory, which
		// is the write that shapes every future run and is worth stopping a
		// person for.
		Level: tool.LevelInternal,
		Capabilities: tool.Capabilities{
			// Not idempotent: asking twice writes twice, which is why
			// corrections name what they replace.
			Destructive: true,
		},
	}
}

type rememberArgs struct {
	Text       string `json:"text"`
	Activation string `json:"activation"`
	Scope      string `json:"scope"`
	Supersedes string `json:"supersedes"`
	ValidUntil string `json:"valid_until"`
}

// LevelFor separates writing something down from giving it authority.
//
// A retrieval-only memory changes nothing until somebody asks for it, and
// stopping for a person on every one of those produces "permanently asking
// whether to permanently remember" — an approval that fires constantly is one
// people learn to click through, which is worse than not asking.
//
// A standing memory goes in front of the model on every future run. That is
// the privileged operation, and it is the one worth a person's attention.
func (t *Remember) LevelFor(call tool.Call) tool.Level {
	var args rememberArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		// Unreadable arguments are judged at the higher level. Guessing the
		// cheap one for something we cannot parse is the wrong way to be
		// wrong.
		return tool.LevelRemember
	}

	if activation, err := memoryActivation(args.Activation); err == nil &&
		activation == domain.MemoryStanding {
		return tool.LevelRemember
	}
	return tool.LevelInternal
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

	activation, err := memoryActivation(args.Activation)
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

	now := t.Now()

	validUntil, err := parseValidUntil(args.ValidUntil, now)
	if err != nil {
		return tool.Result{}, err
	}

	written := domain.Memory{
		ID:         domain.MemoryID(t.NewID()),
		Scope:      scope,
		ScopeRef:   ref,
		Activation: activation,
		Text:       text,
		// The trust of the turn that produced it, which for anything arriving
		// through a gateway is the lowest there is — and stays that way
		// however many times it is summarised afterwards.
		Trust:         trustOf(call.Context),
		Origin:        call.Context.Origin,
		SourceSession: domain.SessionID(call.Context.SessionID),
		SourceSeq:     call.Context.Seq,
		ApprovedBy:    approverOf(call.Context),
		CreatedAt:     now,

		// Valid from when it was learned unless somebody said otherwise,
		// which is the honest default: a fact stated now with no history
		// attached is a fact about now.
		ValidFrom:  now,
		ValidUntil: validUntil,
		ExpiresAt:  t.expiryFor(activation, now),
	}

	if err := t.Store.Remember(ctx, written, supersedes); err != nil {
		return tool.Result{}, tool.Errorf(tool.CodeInternal, "", "%v", err)
	}

	summary := fmt.Sprintf("remembered as %s", written.ID)
	if supersedes != "" {
		summary = fmt.Sprintf("replaced %s with %s", supersedes, written.ID)
	}

	content := fmt.Sprintf("%s (%s, %s)\n%s", summary, activation, scope, text)
	if validUntil != nil {
		content += fmt.Sprintf("\nStops being true on %s.", validUntil.Format("2006-01-02"))
	}

	return tool.Result{Content: content, Summary: summary}, nil
}

// expiryFor is when a memory stops being offered unless something uses it.
//
// Retrieval memories only. A standing direction was approved by a person and
// is put in front of the model every turn — expiring one silently would
// remove an instruction somebody deliberately gave, which is worse than the
// bloat it would save. They are also few, and bounded by their own byte
// limit.
//
// Retrieval memories are neither: nobody approved them individually, there is
// no ceiling on how many accumulate, and the cost of the ones nobody wants is
// a corpus that answers worse as it grows.
func (t *Remember) expiryFor(activation domain.MemoryActivation, now time.Time) *time.Time {
	if t.RetrievalTTL <= 0 || activation != domain.MemoryRetrieval {
		return nil
	}
	at := now.Add(t.RetrievalTTL)
	return &at
}

// parseValidUntil reads a date or a timestamp.
//
// A plain date is the common case — "until the fifteenth" — and it means the
// end of that day rather than its start: a freeze that lifts on the fifteenth
// is in force on the fifteenth.
// Returns a plain error rather than *tool.Error: assigned into an error
// variable, a typed nil pointer is not nil, and the caller would report every
// success as a failure with no message.
func parseValidUntil(text string, now time.Time) (*time.Time, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}

	if at, err := time.Parse(time.RFC3339, text); err == nil {
		return checkFuture(at, now)
	}
	// In this machine's zone, not UTC. Somebody writing "the fifteenth"
	// means their own fifteenth, and parsing it as UTC moves the end of the
	// window by the offset — which for anywhere east of London means the
	// freeze lifts on the wrong day.
	if day, err := time.ParseInLocation("2006-01-02", text, time.Local); err == nil {
		at := day.AddDate(0, 0, 1)
		return checkFuture(at, now)
	}

	return nil, tool.Errorf(tool.CodeInvalidArguments,
		"Use a date like 2026-09-15, or an RFC 3339 time.",
		"%q is not a date this understands", text)
}

// checkFuture refuses an end that has already passed.
//
// Writing a memory that is not true when it is written is a model working
// from a wrong idea of the date, and storing it would mean a memory that can
// never be recalled and nobody can explain.
func checkFuture(at, now time.Time) (*time.Time, error) {
	if !at.After(now) {
		return nil, tool.Errorf(tool.CodeInvalidArguments,
			"Record what is true now, or supersede the memory this replaces.",
			"%s has already passed, so this would be remembered as already untrue",
			at.Format("2006-01-02"))
	}
	return &at, nil
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

// memoryScope settles who a memory belongs to, and refuses the one combination
// that is not a matter of permission.
//
// A turn from outside this machine may write about the person it came from and
// nothing else. What the *project* knows is read by every local run, including
// ones that can execute programs, so letting a chat account write there would
// make a message somebody sent into something the machine believes about its
// own work. Promoting anything to that scope is the operator's to do, from a
// control-plane client.
func (t *Remember) memoryScope(
	requested string,
	call tool.CallContext,
) (domain.MemoryScope, string, error) {
	switch requested {
	case "", "workspace":
		if call.FromGateway() {
			// Default to the one scope it may write, rather than refusing a
			// turn that never named a scope at all.
			if requested == "" {
				return domain.ScopePrincipal, call.PrincipalKey(), nil
			}
			return "", "", tool.Errorf(tool.CodePermissionDenied,
				"Remember it about the person instead, or ask the operator to record it for the project.",
				"a turn from outside this machine cannot write what the project knows")
		}
		return domain.ScopeWorkspace, t.WorkspaceRef, nil

	case "person":
		return domain.ScopePrincipal, call.PrincipalKey(), nil

	default:
		return "", "", tool.Errorf(tool.CodeInvalidArguments,
			"Use workspace or person.", "%q is not a scope", requested)
	}
}

func memoryActivation(requested string) (domain.MemoryActivation, error) {
	switch requested {
	case "", "retrieval":
		return domain.MemoryRetrieval, nil
	case "standing":
		return domain.MemoryStanding, nil
	default:
		return "", tool.Errorf(tool.CodeInvalidArguments,
			"Use retrieval or standing.", "%q is not a kind of activation", requested)
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
		// The tool's own clock, not the store's. Everything here takes its
		// time from one place so that a memory's validity is judged against
		// the same moment its expiry is.
		At: t.Now(),
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

	t.touch(ctx, found)

	return tool.Result{
		Content: render(found),
		Summary: fmt.Sprintf("recall: %d", len(found)),
	}, nil
}

// touch records that these were wanted, which is what keeps them alive.
//
// Without it the expiry is a countdown from the moment a memory was written,
// and the thing the agent reaches for constantly dies on the same schedule as
// the thing nobody has ever wanted.
//
// Best-effort: a recall that answered is a recall that worked, and failing it
// because the bookkeeping did not land would trade something a person asked
// for against something nobody did.
func (t *Recall) touch(ctx context.Context, found []domain.Memory) {
	ids := make([]domain.MemoryID, 0, len(found))
	for _, memory := range found {
		ids = append(ids, memory.ID)
	}
	if err := t.Store.TouchMemories(ctx, ids, t.Now()); err != nil {
		t.Logger().Warn("could not record that memories were used", "error", err)
	}
}

// render lists memories with where each came from.
//
// The provenance is shown rather than kept for auditing alone: a model reading
// "somebody on Discord said this" should weigh it differently from something
// the operator wrote, and it can only do that if it is told.
func render(memories []domain.Memory) string {
	var out strings.Builder

	for _, memory := range memories {
		fmt.Fprintf(&out, "%s [%s, %s", memory.ID, memory.Activation, memory.Scope)
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
		Scopes:     options.scopesFor(contextForRun(run)),
		Activation: domain.MemoryStanding,
		Limit:      maxRecallLimit,
		At:         options.Now(),
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
