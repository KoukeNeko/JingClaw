package gateway

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Provider and Model are what this daemon answers with by default,
	// reported in a run's summary.
	//
	// The provider is daemon-wide, because that is what it is. The model is a
	// default: a session may name its own, and the summary has to say the one
	// that actually answered — a line naming the wrong model is worse than no
	// line, because it is the thing somebody reads to work out why an answer
	// was poor.
	Provider string
	Model    string

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

	// File is content to post as an attachment rather than as text.
	//
	// Only ever set for something somebody asked for by name. Nothing is
	// pushed: a run that stores a large result says so, and the bytes cross
	// into a channel when a person asks for them and not before.
	File *MessageFile `json:"file,omitempty"`
}

// MessageFile is an attachment to post.
type MessageFile struct {
	Name string `json:"name"`

	// Content is the bytes, base64 in JSON. Bounded well below what a
	// platform accepts, because this travels through the dispatch queue and a
	// queue is a poor place to keep megabytes.
	Content []byte `json:"content"`

	MediaType string `json:"media_type,omitempty"`
}

// LogPayload is one thing that happened, for a channel that wants the detail.
//
// Only sent to a console. A platform is expected to show these as they arrive
// and leave them, unlike a status line, which is the answer to "what is it
// doing now" and is rewritten when that changes.
type LogPayload struct {
	Tool    string `json:"tool"`
	Summary string `json:"summary,omitempty"`

	DurationMS int64 `json:"duration_ms,omitempty"`
	IsError    bool  `json:"is_error,omitempty"`

	// Output is what the tool actually printed, bounded.
	//
	// Only a console gets this. A summary says a command exited zero; the
	// output says which test failed and on what line, and that is the thing
	// somebody watching a console is watching for. Bounded here rather than
	// where it is rendered, so a build log does not travel through the
	// dispatch queue to be thrown away at the far end.
	Output string `json:"output,omitempty"`

	// OutputTruncated says the tool printed more than was carried.
	OutputTruncated bool `json:"output_truncated,omitempty"`

	// Artifact names stored output, so somebody can ask for it by name.
	Artifact string `json:"artifact,omitempty"`
}

// maxLoggedOutput bounds what one log line carries.
//
// Enough for a failing test's output and the line above it, and well short of
// what a platform accepts: a console is a place to notice things, and the
// whole of a build log belongs in the artifact it is already stored as.
const maxLoggedOutput = 1200

// ApprovalPayload asks a conversation to decide about a tool call.
type ApprovalPayload struct {
	ApprovalID string   `json:"approval_id"`
	ToolName   string   `json:"tool_name"`
	Summary    string   `json:"summary"`
	Effects    []string `json:"effects,omitempty"`

	// Route is how a decision may be made where this is going.
	//
	// Carried rather than worked out by whatever renders the message: which
	// channels are consoles and who may press a button are facts about the
	// deployment, and a renderer that guessed would eventually invite
	// somebody to do something that does nothing.
	Route ApprovalRoute `json:"route,omitempty"`
}

// ApprovalRoute is how an approval may be answered from a conversation.
//
// The three differ in what identifies the person deciding, which is the whole
// question an approval asks.
type ApprovalRoute string

const (
	// ApprovalElsewhere is the default: the message says where to go and
	// offers nothing here.
	ApprovalElsewhere ApprovalRoute = ""

	// ApprovalByReply is a console, where the channel is the credential.
	// Nobody but its owner can read or type in it, so a typed command is as
	// good as anything else.
	ApprovalByReply ApprovalRoute = "reply"

	// ApprovalByPress is a shared room, where the person is the credential.
	//
	// Typing is not enough here and never becomes enough: a message in a room
	// other people can type in says which account posted it and nothing more.
	// A button press is delivered by the platform with the presser's
	// authenticated identity attached, which is a different claim entirely.
	ApprovalByPress ApprovalRoute = "press"
)

// QuestionPayload is the agent asking a person something.
//
// Separate from an approval because the two want different controls: an
// approval is allowed or denied, and this is answered with words or a choice.
type QuestionPayload struct {
	QuestionID string `json:"question_id"`
	Prompt     string `json:"prompt"`

	// Kind is "choice" or "text".
	Kind string `json:"kind"`

	Options []QuestionOptionPayload `json:"options,omitempty"`

	// AnswerableHere says this conversation may answer, which is true of a
	// console channel and not of an ordinary one. Carried rather than worked
	// out by whatever renders the message, for the same reason an approval
	// carries it: which channels are consoles is a fact about the deployment.
	AnswerableHere bool `json:"answerable_here,omitempty"`
}

// QuestionOptionPayload is one thing a person may choose.
type QuestionOptionPayload struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

func optionPayloads(options []domain.QuestionOption) []QuestionOptionPayload {
	out := make([]QuestionOptionPayload, 0, len(options))
	for _, option := range options {
		out = append(out, QuestionOptionPayload{
			ID: option.ID, Label: option.Label, Detail: option.Detail,
		})
	}
	return out
}

// StatusPayload reports a change a reader would notice.
//
// A platform is expected to keep one of these visible at a time and rewrite it
// rather than posting each: they are what the agent is doing now, and the
// previous answer to that question is of no interest once it changes.
type StatusPayload struct {
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`

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

	case domain.PlanChanged:
		// A channel is told what the agent is doing, not shown the list every
		// time a step moves. A plan of six steps posted on each change is six
		// messages saying almost the same thing, above the answer somebody is
		// waiting for.
		//
		// The status line is where "what is it doing now" already lives, so
		// that is where this goes.
		if !p.shouldSayWorking(run.ID) {
			return nil
		}
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:  "working",
			Detail: describePlan(payload.Items),
		})

	case domain.AssistantReasoningDelta:
		// Never carried to a platform, console channel included, and refused
		// here rather than left to a missing case. A platform account is
		// somebody's account, and the working-out quotes whatever the run has
		// been reading — a file it opened, a page it fetched, a memory. The
		// answer is what was asked for; this is not.
		return nil

	case domain.AssistantMessageCompleted:
		text := p.take(payload.MessageID)
		if strings.TrimSpace(text) != "" {
			p.record(run.ID).said = true
		}
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
		if payload.Name == "web_read" {
			return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "network_started"})
		}
		if payload.Name == "recall" {
			return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "memory_started"})
		}

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
		p.record(run.ID).completed(payload, event.Seq)
		if payload.Name == "web_read" {
			return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "network_finished"})
		}
		if payload.Name == "recall" {
			return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "memory_finished"})
		}

		// A finished call is not news to a room full of people, and it is
		// exactly what the operator of a private one wants: what ran, how long
		// it took, and whether it worked. A console is a log; an ordinary
		// channel is a conversation.
		if !p.isConsole(ctx, target) {
			return nil
		}
		output, cut := boundOutput(payload.Content, maxLoggedOutput)
		return p.enqueue(ctx, run, target, DispatchLog, LogPayload{
			Tool:            payload.Name,
			Summary:         payload.Summary,
			Output:          output,
			OutputTruncated: cut || payload.Truncated,
			DurationMS:      payload.DurationMS,
			IsError:         payload.IsError,
			Artifact:        artifactID(payload.Artifact),
		})

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
			ApprovalID: string(payload.ApprovalID),
			ToolName:   payload.ToolName,
			Summary:    payload.Summary,
			Effects:    payload.Effects,
			Route:      p.approvalRoute(ctx, target),
		})

	case domain.QuestionAsked:
		// Carried to every channel, unlike an approval's detail. A question
		// is what the run is waiting on, and a channel that showed nothing
		// would show a conversation that had simply stopped.
		//
		// Whether it can be answered from here is another matter, and it is
		// said rather than left to be discovered by typing.
		return p.enqueue(ctx, run, target, DispatchQuestion, QuestionPayload{
			QuestionID:     string(payload.QuestionID),
			Prompt:         payload.Prompt,
			Kind:           string(payload.Kind),
			Options:        optionPayloads(payload.Options),
			AnswerableHere: p.isConsole(ctx, target),
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
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{State: "provider_started"})

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
		summary := p.close(ctx, run)

		// A console gets what actually happened. The redaction exists because
		// a provider's own words land in a room other people read; where the
		// only reader is the operator, hiding the reason from them is not
		// protecting anybody, it is making their own failure harder to fix.
		detail := explainFailure(payload.FailureKind)
		if p.isConsole(ctx, target) && payload.Reason != "" {
			detail = payload.Reason
		}

		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:   string(payload.Status),
			Detail:  detail,
			Summary: summary,
		})

	case domain.RunCompleted:
		// The answer says what happened; this replaces the line that was
		// saying what it was doing, which would otherwise sit above the answer
		// claiming the agent is still busy.
		p.forget(run.ID)
		summary := p.close(ctx, run)
		duration := p.Now().Sub(run.CreatedAt)
		return p.enqueue(ctx, run, target, DispatchStatus, StatusPayload{
			State:      "completed",
			Detail:     duration.Round(100 * time.Millisecond).String(),
			DurationMS: duration.Milliseconds(),
			Summary:    summary,
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

// boundOutput keeps the end of what a tool printed.
//
// The end rather than the start: a command that failed says why on its last
// lines, and a build log's first thousand characters are the compiler
// announcing itself.
func boundOutput(content string, limit int) (string, bool) {
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return "", false
	}

	runes := []rune(trimmed)
	if len(runes) <= limit {
		return trimmed, false
	}
	return string(runes[len(runes)-limit:]), true
}

func artifactID(ref *domain.Artifact) string {
	if ref == nil {
		return ""
	}
	return ref.ID
}

// isConsole reports whether this room is one an operator controls.
//
// Read for several unrelated decisions — whether a finished tool call is worth
// logging, whether a question may be answered here, whether a failure's own
// words may be shown — so it stays its own question rather than becoming a
// case of the approval route below.
func (p *Projector) isConsole(ctx context.Context, target ConversationRef) bool {
	binding, err := p.Store.Binding(ctx,
		target.Platform, target.AccountID, target.TenantID, target.ChannelID)
	if err != nil {
		return false
	}
	return binding.PermissionProfile == ConsoleProfileName
}

// approvalRoute settles how the room this is going to may answer it.
//
// A binding that cannot be read offers nothing, which is the safe direction:
// the message then says to go and decide it somewhere else, and that is always
// true.
func (p *Projector) approvalRoute(ctx context.Context, target ConversationRef) ApprovalRoute {
	binding, err := p.Store.Binding(ctx,
		target.Platform, target.AccountID, target.TenantID, target.ChannelID)
	if err != nil {
		return ApprovalElsewhere
	}

	if binding.PermissionProfile == ConsoleProfileName {
		return ApprovalByReply
	}
	if len(binding.ApprovingPrincipals) > 0 || len(binding.ApprovingClaims) > 0 {
		return ApprovalByPress
	}
	return ApprovalElsewhere
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
func (p *Projector) close(ctx context.Context, run domain.Run) *RunSummary {
	p.mu.Lock()
	existing, ok := p.records[run.ID]
	if ok {
		delete(p.records, run.ID)
	}
	p.mu.Unlock()

	if !ok {
		return nil
	}

	summary := existing.summarise(p.Provider, p.modelFor(ctx, run.SessionID))
	return &summary
}

// modelFor is the model this session answered with.
//
// Read here rather than taken from the daemon's default, because a session may
// name its own and the summary is what somebody reads to work out why an
// answer was poor. Falling back on any failure: an unreadable session row is a
// reason to name the default, not a reason to leave the line out.
func (p *Projector) modelFor(ctx context.Context, sessionID domain.SessionID) string {
	session, err := p.Store.Session(ctx, sessionID)
	if err != nil || session.Model == "" {
		return p.Model
	}
	return session.Model
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

// describePlan is one line about where a plan has got to.
//
// A count and the step in hand. The titles of the others are not useful to
// somebody watching a channel: what they want to know is whether it is
// getting on with it and what it is on now.
func describePlan(items []domain.PlanItem) string {
	var done int
	current := ""
	for _, item := range items {
		if item.Status.IsTerminal() {
			done++
		}
		if item.Status == domain.PlanInProgress && current == "" {
			current = item.Title
		}
	}

	if current == "" {
		return fmt.Sprintf("plan: %d of %d done", done, len(items))
	}
	return fmt.Sprintf("plan: %d of %d done — %s", done, len(items), current)
}
