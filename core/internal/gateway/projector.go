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

	// pending accumulates assistant text per message until it completes, and
	// lastWorking remembers when each run last said something, so the throttle
	// is per run rather than global.
	//
	// Guarded because runs execute concurrently: two conversations producing
	// output at the same moment reach this from different goroutines.
	mu          sync.Mutex
	pending     map[domain.MessageID]*strings.Builder
	lastWorking map[domain.RunID]time.Time
}

// defaultWorkingInterval is roughly what a person reads at, and comfortably
// inside what a platform will accept as edits to one message.
const defaultWorkingInterval = 2 * time.Second

func NewProjector(store Store, newID func() string, now func() time.Time) *Projector {
	if now == nil {
		now = time.Now
	}
	return &Projector{
		Store:           store,
		NewID:           newID,
		Now:             now,
		WorkingInterval: defaultWorkingInterval,
		pending:         make(map[domain.MessageID]*strings.Builder),
		lastWorking:     make(map[domain.RunID]time.Time),
	}
}

// MessagePayload is what a gateway posts for agent output.
type MessagePayload struct {
	Text string `json:"text"`
}

// ApprovalPayload asks a conversation to decide about a tool call.
type ApprovalPayload struct {
	ApprovalID string   `json:"approval_id"`
	ToolName   string   `json:"tool_name"`
	Summary    string   `json:"summary"`
	Effects    []string `json:"effects,omitempty"`
}

// StatusPayload reports a change a reader would notice.
//
// A platform is expected to keep one of these visible at a time and rewrite it
// rather than posting each: they are what the agent is doing now, and the
// previous answer to that question is of no interest once it changes.
type StatusPayload struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Observe queues whatever this event warrants saying.
func (p *Projector) Observe(ctx context.Context, run domain.Run, event domain.Event) error {
	target, ok := externalTarget(run)
	if !ok {
		return nil
	}

	switch payload := event.Payload.(type) {
	case domain.AssistantTextDelta:
		// Held until the message completes. A channel wants the reply, not
		// the reply being typed.
		p.accumulate(payload.MessageID, payload.Text)
		return nil

	case domain.AssistantMessageCompleted:
		text := p.take(payload.MessageID)
		if strings.TrimSpace(text) == "" {
			// A turn that only asked for tools has nothing to say yet.
			return nil
		}
		return p.enqueue(ctx, run, target, DispatchMessage, MessagePayload{Text: text})

	case domain.ToolCallRequested:
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

	case domain.ApprovalRequested:
		return p.enqueue(ctx, run, target, DispatchApproval, ApprovalPayload{
			ApprovalID: string(payload.ApprovalID),
			ToolName:   payload.ToolName,
			Summary:    payload.Summary,
			Effects:    payload.Effects,
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
		// Long tasks are the ones where silence is worst: a channel that hears
		// nothing for a minute cannot tell working from broken.
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "running"})

	case domain.RunFailed, domain.RunCancelled:
		// A run that ends badly must say so. Ending in silence looks exactly
		// like still working.
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:  string(payload.Status),
			Detail: payload.Reason,
		})

	case domain.RunCompleted:
		// The answer says what happened; this replaces the line that was
		// saying what it was doing, which would otherwise sit above the answer
		// claiming the agent is still busy.
		p.forget(run.ID)
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:  "completed",
			Detail: p.Now().Sub(run.CreatedAt).Round(time.Second).String(),
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

func (p *Projector) take(id domain.MessageID) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	builder, ok := p.pending[id]
	if !ok {
		return ""
	}
	delete(p.pending, id)
	return builder.String()
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
