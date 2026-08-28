package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// Projector turns run events into things worth saying in a conversation.
//
// It deliberately does not forward every event. A chat channel is read by
// people, and a play-by-play of tool calls and token counts buries the answer
// they were waiting for. What earns a message is: work started, a decision is
// needed, the answer, and a failure.
//
// Nor does it forward deltas. Discord has no streaming and would have to be
// sent an edit per chunk, which is a rate limit waiting to happen; assembling
// the finished message here costs a little latency and produces something
// readable.
//
// It does say what the agent is doing, though. Silence while a run reads four
// files and waits on a test suite is indistinguishable from silence because
// something broke, and the person watching cannot tell which. Those lines are
// throttled and meant to be rewritten in place rather than accumulated.
type Projector struct {
	Store Store

	NewID func() string
	Now   func() time.Time

	// WorkingInterval is the least time between "what it is doing now" lines.
	// Every tool call producing one would exceed what a platform will accept
	// and bury the channel besides.
	WorkingInterval time.Duration

	// StreamInterval is the least time between versions of an answer that is
	// still being written. Every delta producing one would be a rate limit
	// rather than a feature.
	StreamInterval time.Duration

	// pending accumulates assistant text per message until it completes, and
	// lastWorking remembers when each run last said something, so the throttle
	// is per run rather than global.
	//
	// Guarded because runs execute concurrently: two conversations producing
	// output at the same moment reach this from different goroutines.
	mu           sync.Mutex
	pending      map[domain.MessageID]*strings.Builder
	lastWorking  map[domain.RunID]time.Time
	lastStreamed map[domain.MessageID]time.Time

	// records accumulate what each run has done, for the summary posted when
	// it ends.
	records map[domain.RunID]*runRecord
}

// defaultWorkingInterval is roughly what a person reads at, and comfortably
// inside what a platform will accept as edits to one message.
const (
	defaultWorkingInterval = 2 * time.Second

	// defaultStreamInterval is fast enough to read as writing and slow enough
	// that one answer does not spend a channel's whole edit allowance.
	defaultStreamInterval = 1500 * time.Millisecond
)

func NewProjector(store Store, newID func() string, now func() time.Time) *Projector {
	if now == nil {
		now = time.Now
	}
	return &Projector{
		Store:           store,
		NewID:           newID,
		Now:             now,
		WorkingInterval: defaultWorkingInterval,
		StreamInterval:  defaultStreamInterval,
		pending:         make(map[domain.MessageID]*strings.Builder),
		lastWorking:     make(map[domain.RunID]time.Time),
		lastStreamed:    make(map[domain.MessageID]time.Time),
		records:         make(map[domain.RunID]*runRecord),
	}
}

// MessagePayload is what a gateway posts for agent output.
type MessagePayload struct {
	Text string `json:"text"`

	// MessageID names the answer this is a version of. A platform that can
	// rewrite what it posted uses it to keep one message growing rather than
	// posting the answer again every time there is more of it.
	MessageID string `json:"message_id,omitempty"`

	// Final says this is the whole answer. Anything before it is as much as
	// had been said at the time, and a platform may show it or ignore it.
	Final bool `json:"final,omitempty"`
}

// ApprovalPayload asks a conversation to decide about a tool call.
type ApprovalPayload struct {
	ApprovalID string   `json:"approval_id"`
	ToolName   string   `json:"tool_name"`
	Summary    string   `json:"summary"`
	Effects    []string `json:"effects,omitempty"`

	// DecidableHere says this conversation may answer its own approval,
	// which is true of a console channel and not of an ordinary one.
	//
	// Carried rather than worked out by whatever renders the message: which
	// channels are consoles is a fact about the deployment, and a renderer
	// that guessed would eventually invite somebody to type a command that
	// does nothing.
	DecidableHere bool `json:"decidable_here,omitempty"`
}

// StatusPayload reports a change a reader would notice.
//
// A platform is expected to keep one of these visible at a time and rewrite it
// rather than posting each: they are what the agent is doing now, and the
// previous answer to that question is of no interest once it changes.
type StatusPayload struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`

	// Summary accounts for a run that has ended: what it reached for, what it
	// drew on, and what it cost. Absent while a run is still going, because
	// none of those questions have an answer yet.
	Summary *RunSummary `json:"summary,omitempty"`
}

// Observe queues whatever this event warrants saying.
func (p *Projector) Observe(ctx context.Context, run domain.Run, event domain.Event) error {
	target, ok := externalTarget(run)
	if !ok {
		return nil
	}

	switch payload := event.Payload.(type) {
	case domain.AssistantTextDelta:
		// Sent as it grows, on a cadence. Waiting for the whole answer means a
		// channel watches nothing happen for as long as the model takes to
		// write, which is the difference between an agent that feels alive and
		// one that feels stuck.
		//
		// The first one is a whole interval in, so a short answer never
		// streams: it simply arrives, which is what it should do.
		p.accumulate(payload.MessageID, payload.Text)
		if !p.shouldStream(payload.MessageID) {
			return nil
		}
		return p.enqueue(ctx, run, target, DispatchMessage, MessagePayload{
			Text:      p.peek(payload.MessageID),
			MessageID: string(payload.MessageID),
		})

	case domain.AssistantMessageCompleted:
		text := p.take(payload.MessageID)
		if strings.TrimSpace(text) == "" {
			// A turn that only asked for tools has nothing to say yet.
			return nil
		}
		return p.enqueue(ctx, run, target, DispatchMessage, MessagePayload{
			Text:      text,
			MessageID: string(payload.MessageID),
			Final:     true,
		})

	case domain.ToolCallRequested:
		p.record(run.ID).requested(payload)

		// What it is doing, while it is doing it. Throttled, because a run
		// that reads six files in a second would otherwise send six lines
		// nobody can read and a rate limit nobody wanted.
		if !p.shouldSayWorking(run.ID) {
			return nil
		}
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:  "working",
			Detail: describeCall(payload.Name, payload.Arguments),
		})

	case domain.ToolCallCompleted:
		// Nothing is said now: a finished tool call is not news to a channel.
		// It is recorded so the run can account for itself when it ends.
		p.record(run.ID).completed(payload, event.Seq)
		return nil

	case domain.UsageChanged:
		p.record(run.ID).usage = payload.Usage
		return nil

	case domain.ConversationCompacted:
		// Everything up to here is now a summary rather than itself, which is
		// what decides whether a source was still in front of the model.
		p.record(run.ID).compactedThrough = payload.ThroughSeq
		return nil

	case domain.ApprovalRequested:
		return p.enqueue(ctx, run, target, DispatchApproval, ApprovalPayload{
			ApprovalID:    string(payload.ApprovalID),
			ToolName:      payload.ToolName,
			Summary:       payload.Summary,
			Effects:       payload.Effects,
			DecidableHere: p.isConsole(ctx, target),
		})

	case domain.RunStateChanged:
		return p.observeState(ctx, run, target, payload)
	}

	return nil
}

// observeState reports only the transitions a reader would notice.
func (p *Projector) observeState(
	ctx context.Context,
	run domain.Run,
	target ConversationRef,
	payload domain.RunStateChanged,
) error {
	switch payload.Status {
	case domain.RunRunning:
		// Seeing a run start is what makes its eventual summary complete. A
		// run resumed from an approval after a restart never passes here, and
		// its summary says so rather than under-reporting in silence.
		p.record(run.ID).seen = true

		// Long tasks are the ones where silence is worst: a channel that hears
		// nothing for a minute cannot tell working from broken.
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "running"})

	case domain.RunFailed, domain.RunCancelled:
		// A run that ends badly must say so. Ending in silence looks exactly
		// like still working. It still accounts for itself: work that failed
		// halfway through was paid for, and a reader asking what it cost is
		// asking most often about exactly this case.
		//
		// What it must not say is payload.Reason. That field carries the
		// upstream provider's own words, and this is a channel other people
		// read: it has already put a billing endpoint, a quota metric and an
		// account's limits in front of everybody in a room. The log keeps the
		// whole of it; a stranger gets a sentence.
		summary := p.close(run.ID)
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:   string(payload.Status),
			Detail:  explainFailure(payload.FailureKind),
			Summary: summary,
		})

	case domain.RunCompleted:
		// The answer says what happened; this replaces the line that was
		// saying what it was doing, which would otherwise sit above the answer
		// claiming the agent is still busy.
		p.forget(run.ID)
		summary := p.close(run.ID)
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:   "completed",
			Detail:  p.Now().Sub(run.CreatedAt).Round(time.Second).String(),
			Summary: summary,
		})

	default:
		return nil
	}
}

// shouldSayWorking rate-limits the "what it is doing now" line, per run.
func (p *Projector) shouldSayWorking(run domain.RunID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.Now()
	if last, ok := p.lastWorking[run]; ok && now.Sub(last) < p.WorkingInterval {
		return false
	}

	p.lastWorking[run] = now
	return true
}

// isConsole reports whether this conversation may answer its own approvals.
//
// Best effort: a lookup that fails leaves the message telling somebody to go
// to the machine, which is always true and never misleading. The opposite
// mistake — inviting a reply that will not work — is the one worth avoiding.
func (p *Projector) isConsole(ctx context.Context, target ConversationRef) bool {
	binding, err := p.Store.Binding(ctx,
		target.Platform, target.AccountID, target.TenantID, target.ChannelID)
	if err != nil {
		return false
	}
	return binding.PermissionProfile == ConsoleProfileName
}

// record returns a run's accumulator, creating it if this is the first thing
// seen for that run.
func (p *Projector) record(run domain.RunID) *runRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing, ok := p.records[run]
	if !ok {
		existing = newRunRecord()
		p.records[run] = existing
	}
	return existing
}

// close renders a run's summary and releases what was held for it.
//
// Nothing is kept after a run ends: these records are the one structure here
// that grows with the number of runs a daemon has served rather than with the
// number in flight.
func (p *Projector) close(run domain.RunID) *RunSummary {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing, ok := p.records[run]
	if !ok {
		return nil
	}
	delete(p.records, run)

	summary := existing.summarise()
	return &summary
}

func (p *Projector) forget(run domain.RunID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.lastWorking, run)
}

// describeCall says what a tool call is doing in a few words.
//
// Best-effort and deliberately shallow: it looks for the argument a person
// would have named the action by, and settles for the tool's own name when
// there is not an obvious one. Getting this exactly right for every tool,
// including ones from servers this code has never seen, is not something a
// status line is worth.
// explainFailure says what a channel should be told about a run that ended
// badly.
//
// Three questions, and only three: is it worth trying again, does somebody
// have to go and fix something, or is this simply broken. Anything more
// specific is either the operator's business or the provider's, and this is
// neither's channel.
//
// It reads a classification rather than the failure text, so it cannot be
// steered by wording that arrived from outside this machine.
func explainFailure(kind string) string {
	switch kind {
	case "rate_limited", "overloaded":
		return "the model is busy right now — try again shortly"

	case "retry_budget_exhausted":
		return "the model is rate limited for longer than this was willing to wait — try again in a few minutes"

	case "quota_exhausted", "auth", "not_found":
		// Deliberately vague about which. "Out of credit", "the key is wrong"
		// and "that model does not exist" are all facts about the operator's
		// account, and a channel is the wrong room for any of them.
		return "the model is unavailable until the operator looks at it"

	case "context_overflow":
		return "the conversation got too long for the model to read"

	case "content_filtered":
		return "the model declined to answer that"

	case "interrupted":
		return "stopped"

	case "transient", "invalid_request", "":
		return "something went wrong at the model"

	default:
		return "something went wrong at the model"
	}
}

func describeCall(name, arguments string) string {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		return name
	}

	for _, key := range []string{"path", "pattern", "query", "program", "file_path", "url"} {
		if value, ok := decoded[key].(string); ok && value != "" {
			return name + " " + value
		}
	}
	return name
}

func (p *Projector) enqueue(
	ctx context.Context,
	run domain.Run,
	target ConversationRef,
	kind DispatchKind,
	payload any,
) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	_, err = p.Store.EnqueueDispatch(ctx, Dispatch{
		ID:        p.NewID(),
		AccountID: target.AccountID,
		SessionID: run.SessionID,
		RunID:     run.ID,
		Target:    target,
		Kind:      kind,
		Payload:   string(encoded),
		CreatedAt: p.Now(),
	})
	return err
}

func (p *Projector) accumulate(id domain.MessageID, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	builder, ok := p.pending[id]
	if !ok {
		builder = &strings.Builder{}
		p.pending[id] = builder
	}
	builder.WriteString(text)
}

func (p *Projector) peek(id domain.MessageID) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	builder, ok := p.pending[id]
	if !ok {
		return ""
	}
	return builder.String()
}

func (p *Projector) take(id domain.MessageID) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	builder, ok := p.pending[id]
	if !ok {
		return ""
	}
	delete(p.pending, id)
	delete(p.lastStreamed, id)
	return builder.String()
}

// shouldStream rate-limits versions of an answer that is still being written.
//
// The first delta only starts the clock. An answer finished inside one
// interval is never streamed, which is right: it arrives whole instead of
// appearing and then being rewritten a moment later.
func (p *Projector) shouldStream(id domain.MessageID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	interval := p.StreamInterval
	if interval <= 0 {
		interval = defaultStreamInterval
	}

	now := p.Now()
	last, started := p.lastStreamed[id]
	if !started {
		p.lastStreamed[id] = now
		return false
	}
	if now.Sub(last) < interval {
		return false
	}

	p.lastStreamed[id] = now
	return true
}

// externalTarget returns the first non-local delivery target for a run.
func externalTarget(run domain.Run) (ConversationRef, bool) {
	for _, target := range run.DeliveryTargets {
		if target.Kind == domain.DeliveryLocalClient {
			continue
		}
		if ref, ok := ConversationFromTarget(target); ok {
			return ref, true
		}
	}
	return ConversationRef{}, false
}
