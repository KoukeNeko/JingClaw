package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// Completer asks the model one closed question.
//
// A bare completion: no tools, no conversation, no memories. What it is
// handed is the whole of what the provider sees, which is what makes it safe
// to hand it a person's words and nothing else.
type Completer interface {
	Complete(ctx context.Context, instruction, input string, maxOutputTokens int) (string, error)
}

// EventReader is what the curator needs from the log: one session's events,
// in order.
type EventReader interface {
	ListAfter(ctx context.Context, id domain.SessionID, after domain.Seq, limit int) ([]domain.Event, error)
}

const (
	defaultMaxClaims     = 3
	defaultCurateTimeout = 30 * time.Second

	// curateMaxOutputTokens is room for three short items in JSON, and for a
	// model that cannot resist a preamble the parser then skips.
	curateMaxOutputTokens = 600

	// maxClaimBytes is a sentence. A note longer than this is a paragraph
	// somebody should have been asked about.
	maxClaimBytes = 300

	aboutPerson  = "person"
	aboutProject = "project"
)

// curateInstruction is written as a closed task with a stated output shape,
// because the answer is parsed rather than read. It asks for the quote on
// purpose: a note is only written when its quote is found, verbatim, in the
// message it cites, so a model that wants to write down something nobody said
// has to invent words that are then not there.
const curateInstruction = `You are keeping notes for an assistant about the people it talks to and the project it works on.

Below are the messages one person sent in one conversation, numbered. Pick out
what would still be worth knowing next month: a fact about them (their name,
role, timezone, the tools they use), a preference they stated, a decision they
made about the project, a constraint they set. Not what they asked for this
once, not what the assistant answered, not something mentioned in passing.

Answer with a JSON array and nothing else. Each item is an object:
  "claim": one plain sentence in the third person
  "quote": the exact words from the message that support it, copied verbatim
  "message": the number of the message the quote is from
  "about": "person" or "project"
At most three items. If nothing is worth keeping, answer [].`

// Curator notices what a person said that is worth keeping, once they have
// been answered.
//
// It reads the person's own messages from the run and nothing else — not the
// reply, not what a tool returned, not what a page said — and asks the model
// what in them is worth a note. Every proposal is checked mechanically before
// it is written. What it writes is a retrieval memory: looked up when wanted,
// never carried into every turn, and marked on its face as approved by
// nobody. Standing memories, the ones with authority, are a person's to make.
type Curator struct {
	Options

	Events EventReader
	Model  Completer

	// MaxClaims bounds what one run may leave behind. Zero uses a default.
	MaxClaims int

	// Timeout bounds the model call. Zero uses a default.
	Timeout time.Duration
}

// AfterRun is the runtime's hook. A failure is logged and goes no further:
// the run is already answered, and nothing about it should fail for what
// happens after it.
func (c *Curator) AfterRun(ctx context.Context, run domain.Run) {
	written, err := c.Curate(ctx, run)
	if err != nil {
		c.Logger().Warn("could not note what was said",
			"run_id", string(run.ID), "error", err)
		return
	}
	if len(written) > 0 {
		c.Logger().Info("noted what was said",
			"run_id", string(run.ID), "memories", len(written))
	}
}

// Curate reads the run, asks, checks, and writes what survived.
func (c *Curator) Curate(ctx context.Context, run domain.Run) ([]domain.Memory, error) {
	said, err := c.spokenIn(ctx, run)
	if err != nil {
		return nil, err
	}
	if len(said) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	answer, err := c.Model.Complete(ctx, curateInstruction, renderSpoken(said), curateMaxOutputTokens)
	if err != nil {
		return nil, fmt.Errorf("ask what is worth keeping: %w", err)
	}

	claims := checkProposals(parseProposals(answer), said, c.maxClaims())
	return c.write(ctx, run, claims)
}

// spoken is one message a person sent, with where in the log it is.
type spoken struct {
	number  int
	seq     domain.Seq
	message domain.UserMessageAdded
}

// spokenIn is the person's own messages in this run, and only those.
//
// A reply, a tool's result, a page the model read: none of that is the person
// speaking, and a note taken from it would be the model writing down what it
// read as though somebody had said it. Which is exactly how text from outside
// becomes something the agent believes.
func (c *Curator) spokenIn(ctx context.Context, run domain.Run) ([]spoken, error) {
	events, err := c.Events.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("read the run: %w", err)
	}

	var said []spoken
	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		message, ok := event.Payload.(domain.UserMessageAdded)
		if !ok || !aPersonSent(message.Origin) || strings.TrimSpace(message.Text) == "" {
			continue
		}
		said = append(said, spoken{number: len(said) + 1, seq: event.Seq, message: message})
	}
	return said, nil
}

// aPersonSent says whether a turn came from somebody, as opposed to a schedule.
func aPersonSent(origin domain.RunOrigin) bool {
	switch origin.Kind {
	case domain.OriginLocalClient, domain.OriginGateway:
		return true
	default:
		return false
	}
}

func renderSpoken(said []spoken) string {
	var out strings.Builder
	for _, item := range said {
		fmt.Fprintf(&out, "#%d\n%s\n\n", item.number, item.message.Text)
	}
	return strings.TrimRight(out.String(), "\n")
}

// proposal is one item of the model's answer, as it arrives.
type proposal struct {
	Claim   string `json:"claim"`
	Quote   string `json:"quote"`
	Message int    `json:"message"`
	About   string `json:"about"`
}

// parseProposals finds the array in an answer that may have prose around it.
// Anything unparseable is no proposals at all, not an error: a model that
// answered in the wrong shape has proposed nothing.
func parseProposals(answer string) []proposal {
	start := strings.Index(answer, "[")
	end := strings.LastIndex(answer, "]")
	if start < 0 || end <= start {
		return nil
	}

	var proposals []proposal
	if err := json.Unmarshal([]byte(answer[start:end+1]), &proposals); err != nil {
		return nil
	}
	return proposals
}

// claim is a proposal that held up, and the message it rests on.
type claim struct {
	text   string
	about  string
	source spoken
}

// checkProposals keeps the proposals whose evidence holds up.
//
// The check is mechanical on purpose. The quote has to appear in the cited
// message exactly; the claim has to be a sentence and not an essay; the
// subject has to be one the store knows. Nothing here judges whether the
// claim is true, only whether the person actually said what it rests on —
// which is the one thing a model writing notes cannot be trusted to check
// about itself.
func checkProposals(proposals []proposal, said []spoken, limit int) []claim {
	var kept []claim
	for _, proposed := range proposals {
		if len(kept) >= limit {
			break
		}

		text := strings.TrimSpace(proposed.Claim)
		quote := strings.TrimSpace(proposed.Quote)
		if text == "" || quote == "" || len(text) > maxClaimBytes {
			continue
		}
		if proposed.About != aboutPerson && proposed.About != aboutProject {
			continue
		}
		if proposed.Message < 1 || proposed.Message > len(said) {
			continue
		}

		source := said[proposed.Message-1]
		if !strings.Contains(source.message.Text, quote) {
			continue
		}
		kept = append(kept, claim{text: text, about: proposed.About, source: source})
	}
	return kept
}

func (c *Curator) write(ctx context.Context, run domain.Run, claims []claim) ([]domain.Memory, error) {
	var written []domain.Memory
	for _, claim := range claims {
		memory := c.memoryFor(run, claim)

		known, err := c.alreadyKnown(ctx, memory)
		if err != nil {
			return written, err
		}
		if known {
			continue
		}

		if err := c.Store.Remember(ctx, memory, ""); err != nil {
			return written, fmt.Errorf("note %q: %w", memory.Text, err)
		}
		written = append(written, memory)
	}
	return written, nil
}

// memoryFor is the note as the store will hold it.
//
// Everything about where it came from is read off the event, never off the
// answer: who, their trust, the session and the message. The scope follows
// the rule the remember tool enforces — a turn from outside this machine
// writes only about the person it came from, because project knowledge is
// read by runs that can execute programs, and only a turn typed at this
// machine may add to it.
func (c *Curator) memoryFor(run domain.Run, claim claim) domain.Memory {
	origin := claim.source.message.Origin
	turn := tool.CallContext{Origin: origin, Trust: claim.source.message.Trust}

	scope, ref := domain.ScopePrincipal, turn.PrincipalKey()
	if claim.about == aboutProject && origin.Kind == domain.OriginLocalClient {
		scope, ref = domain.ScopeWorkspace, c.WorkspaceRef
	}

	now := c.Now()
	return domain.Memory{
		ID:         domain.MemoryID(c.NewID()),
		Scope:      scope,
		ScopeRef:   ref,
		Activation: domain.MemoryRetrieval,
		Text:       claim.text,
		Trust:      turn.TrustOrUntrusted(),
		From:       provenanceOfTurn(origin),
		Origin:     origin,

		SourceSession: run.SessionID,
		SourceSeq:     claim.source.seq,
		ApprovedBy:    notedBy(turn),
		CreatedAt:     now,
		ValidFrom:     now,
	}
}

// provenanceOfTurn is whose words a turn is before it has read anything: the
// operator's when typed at this machine, somebody else's when it came through
// a gateway. The same starting point the runtime gives a tool call.
func provenanceOfTurn(origin domain.RunOrigin) domain.Provenance {
	if origin.Kind == domain.OriginGateway {
		return domain.ProvenanceExternal
	}
	return domain.ProvenanceOperator
}

// notedBy says on the memory itself that nobody approved it. A listing puts
// this next to "operator", and the difference is the point.
func notedBy(turn tool.CallContext) string {
	return "nobody (noted from " + turn.PrincipalKey() + ")"
}

// alreadyKnown says whether the same note, word for word, is already believed
// in the same scope.
//
// Exact on purpose. A paraphrase is a second note, and deciding that two
// sentences mean the same thing is a judgement this deliberately does not
// make; superseding is a person's move, with an id on it.
func (c *Curator) alreadyKnown(ctx context.Context, memory domain.Memory) (bool, error) {
	found, err := c.Store.SearchMemories(ctx, memory.Text, storage.MemoryQuery{
		Scopes:     []storage.MemoryScopeRef{{Scope: memory.Scope, Ref: memory.ScopeRef}},
		Activation: domain.MemoryRetrieval,
		Limit:      maxRecallLimit,
		At:         memory.CreatedAt,
	})
	if err != nil {
		return false, fmt.Errorf("look for %q: %w", memory.Text, err)
	}

	want := sameWords(memory.Text)
	for _, existing := range found {
		if sameWords(existing.Text) == want {
			return true, nil
		}
	}
	return false, nil
}

func sameWords(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func (c *Curator) maxClaims() int {
	if c.MaxClaims > 0 {
		return c.MaxClaims
	}
	return defaultMaxClaims
}

func (c *Curator) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultCurateTimeout
}
