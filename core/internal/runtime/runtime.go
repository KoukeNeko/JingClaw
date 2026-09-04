// Package runtime owns session and run lifecycle, the agent loop, and
// cancellation. It knows nothing about the wire protocol.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/provider"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

var (
	ErrRunNotFound  = errors.New("runtime: run not found")
	ErrShuttingDown = errors.New("runtime: shutting down")

	// ErrUserInterrupted is the cancellation cause set by InterruptRun. It
	// distinguishes a deliberate stop from a failure, which is what decides
	// whether the run ends CANCELLED or FAILED.
	ErrUserInterrupted = errors.New("runtime: interrupted by user")
)

// orphanReason is recorded on runs that were live when the process stopped.
const orphanReason = "runtime: interrupted by daemon restart"

// Coalescing bounds how often provider deltas become events.
//
// Providers stream in whatever granularity suits them, sometimes a few
// characters or one token-count update at a time. Persisting each delta as its
// own event makes the log unreadable, turns every keystroke into a database
// write, and buries the events that carry meaning. These bound the log by the
// clock rather than by the provider's chunk rate.
type Coalescing struct {
	TextFlushBytes    int
	TextFlushInterval time.Duration

	// UsageFlushInterval paces token accounting. Usage is cumulative, so
	// intermediate values are only progress indicators.
	UsageFlushInterval time.Duration
}

func DefaultCoalescing() Coalescing {
	return Coalescing{
		TextFlushBytes:     240,
		TextFlushInterval:  200 * time.Millisecond,
		UsageFlushInterval: 2 * time.Second,
	}
}

// withDefaults fills anything left unset, so a caller can set one field
// without the others silently becoming zero and flushing on every delta.
func (c Coalescing) withDefaults() Coalescing {
	defaults := DefaultCoalescing()

	if c.TextFlushBytes <= 0 {
		c.TextFlushBytes = defaults.TextFlushBytes
	}
	if c.TextFlushInterval <= 0 {
		c.TextFlushInterval = defaults.TextFlushInterval
	}
	if c.UsageFlushInterval <= 0 {
		c.UsageFlushInterval = defaults.UsageFlushInterval
	}
	return c
}

// defaultMaxIterations is deliberately modest. Real work rarely needs more,
// and a runaway loop is expensive in both tokens and wall-clock.
const defaultMaxIterations = 12

// isDurablyPaused reports whether a run is waiting on a human rather than
// abandoned by a crash.
func isDurablyPaused(status domain.RunStatus) bool {
	return status == domain.RunAwaitingApproval || status == domain.RunAwaitingInput
}

// DeliveryObserver receives events for runs with an external delivery target.
//
// A failure here must not fail the run: the work happened, and being unable to
// tell a chat channel about it is a delivery problem, not a reason to discard
// what was done.
type DeliveryObserver interface {
	Observe(ctx context.Context, run domain.Run, event domain.Event) error
}

// AttachmentReader reads back a file that arrived with a message.
type AttachmentReader interface {
	ReadRange(id string, offset, limit int64) ([]byte, int64, error)
}

// IDGenerator produces identifiers. Injected so tests can be deterministic.
type IDGenerator func() string

type Options struct {
	Store storage.Store
	Hub   *event.Hub

	Provider provider.Provider

	// Model names the model each run uses. The daemon owns this; clients ask
	// rather than deciding for themselves.
	Model string

	// Permissions gates every tool call. Nil means every call runs, which is
	// only appropriate when the tools are read-only.
	Permissions *permission.Engine

	// Tools available to a run. Nil means the model answers from context
	// alone, which is exactly what the walking skeleton did.
	Tools *tool.Registry

	// SystemPrompt is prepended to every request.
	SystemPrompt string

	// WorkerSystemPrompt replaces it for a delegated search.
	//
	// Replaces rather than adds. A worker is not the assistant: most of what
	// the standing prompt says is addressed to something having a
	// conversation, and the operator's own instructions in it are authority a
	// search has no use for and should not be carrying while it reads files
	// nobody vetted. Empty falls back to SystemPrompt, so a deployment that
	// has not thought about it is no worse off than before.
	WorkerSystemPrompt string

	// SystemPromptFor contributes text that depends on the run, appended after
	// SystemPrompt once when the run starts.
	//
	// Appended rather than mixed in, so the part that never changes stays a
	// stable prefix — which is what prompt caching needs. It is per run and
	// not per process because what belongs here can differ by who is asking:
	// a turn from a chat account and one typed at this machine are not owed
	// the same recollections.
	SystemPromptFor func(ctx context.Context, run domain.Run) string

	// Recall puts what this machine noted in earlier conversations in front
	// of the turn being answered: a label, written by this machine, listing
	// claims and where each came from.
	//
	// Called once per run with the words of that turn, and what it returns
	// is reused for every request the run makes. The turn is the last thing
	// before the run's own calls and results, and a label that changed
	// between requests would change the prefix everything after it is cached
	// against. It is never put on an earlier turn: those are history, and
	// history is not edited on the way out. Nil puts nothing.
	Recall func(ctx context.Context, run domain.Run, said string) string

	// AfterRun is told when a run that was having the conversation has
	// completed, on a goroutine of its own, so nothing it does holds the
	// session's turn. A worker is not reported, nor a run that failed or was
	// cancelled: what is worth noting was said by somebody who got an answer.
	// Nil is told nothing.
	AfterRun func(ctx context.Context, run domain.Run)

	// Delivery is told about events on runs whose output belongs somewhere
	// other than a control-plane client.
	//
	// The interface is declared here and implemented elsewhere, so the runtime
	// never learns what a Discord channel is. It forwards; whoever implements
	// this decides what is worth sending and how to render it.
	Delivery DeliveryObserver

	// Coalescing paces how provider deltas become events.
	Coalescing Coalescing

	// Attachments reads back the files that arrived with a message, so the
	// images among them can be put in front of the model.
	//
	// Left nil, attachments are described in words rather than shown. That is
	// a working agent rather than a broken one, which is the right behaviour
	// for a provider or a deployment that cannot take images anyway.
	Attachments AttachmentReader

	// MaxImageBytes bounds one image put in front of the model. Something
	// larger is described instead: a provider will refuse the request, and a
	// refused request is worse than a described picture.
	MaxImageBytes int64

	// ContextBudget bounds how much of a session is sent to the model. Left
	// zero, history is replayed in full and a long session eventually stops
	// working; see ContextBudget for why that is the default rather than a
	// guess at the window.
	ContextBudget ContextBudget

	// KeepAfterFold is how many events before a summary are kept anyway.
	//
	// Zero discards everything the summary replaced, which is correct and
	// leaves nothing to look at when somebody asks what actually happened. A
	// margin costs a little space and keeps the tail of the folded
	// conversation readable. Negative turns pruning off entirely.
	KeepAfterFold int

	// MaxIterations bounds the tool loop. A model that keeps calling tools
	// without converging must stop somewhere, and stopping with a recorded
	// reason beats burning quota until a human notices.
	MaxIterations int

	NewSessionID  IDGenerator
	NewRunID      IDGenerator
	NewMessageID  IDGenerator
	NewEventID    IDGenerator
	NewApprovalID IDGenerator

	// NewPlanItemID names a step in the plan. Its own generator because these
	// ids are shown to the model and typed back by it, so they want to be
	// short and readable rather than shaped like everything else.
	NewPlanItemID IDGenerator

	// NewQuestionID names a question a person will be asked.
	NewQuestionID IDGenerator

	// NewScheduleID names a standing instruction.
	NewScheduleID IDGenerator

	Now    func() time.Time
	Logger *slog.Logger
}

type Runtime struct {
	opts Options

	mu     sync.RWMutex
	active map[domain.RunID]*activeRun

	// queued holds the runs waiting their turn, per session, oldest first;
	// busy names the sessions with a run executing. One run at a time per
	// session: a conversation answers in the order it was spoken to, and two
	// people typing into one channel got two answers written at once, each
	// with a status line of its own.
	queued   map[domain.SessionID][]queuedRun
	busy     map[domain.SessionID]bool
	draining bool

	// group owns every run goroutine, so shutdown can wait for them instead of
	// hoping they finish. No bare `go f()` anywhere in this package.
	group    *errgroup.Group
	groupCtx context.Context
}

// activeRun tracks a run this process is currently driving. Runs that have
// finished, or that belong to a previous process, live only in storage.
type activeRun struct {
	// session is which conversation this run belongs to, so a tool call can
	// find the run it is inside without being handed it.
	session domain.SessionID

	cancel context.CancelCauseFunc
	done   chan struct{}

	// owner says this run holds its session's turn — it came in through admit
	// and must hand the session on when it is done. A worker shares its
	// parent's session and a resumed run rejoins one; neither owns the turn,
	// and a hand-off from either would start the next message while the
	// conversation is still being answered.
	owner bool

	// recalled is what Recall said for this run, once it has been asked. Nil
	// until then; kept so every request the run makes carries the same
	// label. Read and written under the runtime's lock.
	recalled *string
}

// New builds a runtime.
//
// Missing collaborators are fatal here rather than at first use. A nil ID
// generator discovered halfway through a run crashes the daemon in the middle
// of someone's work, and the stack trace points at the symptom rather than at
// the wiring that caused it.
func New(ctx context.Context, opts Options) *Runtime {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	required := map[string]any{
		"Store":         opts.Store,
		"Hub":           opts.Hub,
		"Provider":      opts.Provider,
		"NewSessionID":  opts.NewSessionID,
		"NewRunID":      opts.NewRunID,
		"NewMessageID":  opts.NewMessageID,
		"NewEventID":    opts.NewEventID,
		"NewApprovalID": opts.NewApprovalID,
		"NewPlanItemID": opts.NewPlanItemID,
		"NewQuestionID": opts.NewQuestionID,
		"NewScheduleID": opts.NewScheduleID,
	}
	for name, value := range required {
		if isNilOption(value) {
			panic("runtime: Options." + name + " is required")
		}
	}

	group, groupCtx := errgroup.WithContext(ctx)

	return &Runtime{
		opts:     opts,
		active:   make(map[domain.RunID]*activeRun),
		queued:   make(map[domain.SessionID][]queuedRun),
		busy:     make(map[domain.SessionID]bool),
		group:    group,
		groupCtx: groupCtx,
	}
}

// RecoverOrphanedRuns resolves runs that were still live when the process last
// stopped.
//
// Without this, a crash leaves runs marked running forever and every client
// watches a spinner that will never resolve. They are marked failed rather
// than cancelled because nobody chose to stop them, and the distinction is
// what someone reading the log later needs.
//
// A run parked on a human is not an orphan. Waiting for an answer is its
// correct state, and the answer may arrive hours later, from a different
// client, after this daemon has restarted twice.
func (r *Runtime) RecoverOrphanedRuns(ctx context.Context) (int, error) {
	unfinished, err := r.opts.Store.UnfinishedRuns(ctx)
	if err != nil {
		return 0, err
	}

	orphans := make([]domain.Run, 0, len(unfinished))
	var waiting []domain.Run
	for _, run := range unfinished {
		if isDurablyPaused(run.Status) {
			r.opts.Logger.Info("leaving a paused run alone",
				"run_id", string(run.ID), "status", string(run.Status))
			continue
		}
		if run.Status == domain.RunQueued {
			// It never started, so nothing about it was lost with the
			// process. Its message is in the log; it goes back in line.
			waiting = append(waiting, run)
			continue
		}
		orphans = append(orphans, run)
	}
	sort.Slice(waiting, func(a, b int) bool { return waiting[a].CreatedAt.Before(waiting[b].CreatedAt) })
	for _, run := range waiting {
		if err := r.admit(ctx, run); err != nil {
			return 0, fmt.Errorf("runtime: requeue run %s: %w", run.ID, err)
		}
		r.opts.Logger.Info("put a waiting run back in line",
			"run_id", string(run.ID), "session_id", string(run.SessionID))
	}

	for _, run := range orphans {
		now := r.opts.Now()
		run.Status = domain.RunFailed
		run.FinishedAt = &now

		if err := r.opts.Store.UpdateRun(ctx, run); err != nil {
			return 0, fmt.Errorf("runtime: recover run %s: %w", run.ID, err)
		}

		// The terminal event matters as much as the row: a client resuming
		// from its last sequence number learns the outcome the same way it
		// learns everything else.
		if err := r.append(ctx, run.SessionID, run.ID, domain.EventRunStateChanged, domain.RunStateChanged{
			Status: domain.RunFailed,
			Reason: orphanReason,
		}); err != nil {
			return 0, fmt.Errorf("runtime: record recovery for run %s: %w", run.ID, err)
		}

		r.opts.Logger.Warn("recovered orphaned run",
			"run_id", string(run.ID),
			"session_id", string(run.SessionID),
		)
	}

	return len(orphans), nil
}

func (r *Runtime) CreateSession(ctx context.Context, title string) (domain.Session, error) {
	r.mu.RLock()
	draining := r.draining
	r.mu.RUnlock()

	if draining {
		return domain.Session{}, ErrShuttingDown
	}

	now := r.opts.Now()
	session := domain.Session{
		ID:        domain.SessionID(r.opts.NewSessionID()),
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := r.opts.Store.CreateSession(ctx, session); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

// SetSessionModel chooses which model answers in a session.
//
// Recorded rather than applied to whatever is running: a run already
// generating has a request open with a model in it, and changing that
// underneath would produce a turn attributed to a model that did not write
// it. The next run picks it up.
//
// An empty model goes back to the daemon's configured one, which is how a
// session gets undone rather than needing a separate call for it.
func (r *Runtime) SetSessionModel(
	ctx context.Context,
	id domain.SessionID,
	model string,
) (domain.Session, error) {
	if err := r.opts.Store.SetSessionModel(ctx, id, model, r.opts.Now()); err != nil {
		return domain.Session{}, err
	}
	return r.opts.Store.Session(ctx, id)
}

func (r *Runtime) Session(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	return r.opts.Store.Session(ctx, id)
}

func (r *Runtime) ListSessions(ctx context.Context) ([]domain.Session, error) {
	return r.opts.Store.ListSessions(ctx)
}

func (r *Runtime) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	return r.opts.Store.Run(ctx, id)
}

// SendTurn starts a run whose output goes back to the client that asked for
// it.
//
// It returns as soon as the run is accepted; the answer arrives over the event
// stream, so a client disconnecting never cancels an in-flight generation.
func (r *Runtime) SendTurn(
	ctx context.Context,
	sessionID domain.SessionID,
	text string,
	origin domain.RunOrigin,
) (domain.RunID, domain.MessageID, error) {
	return r.SendTurnTo(ctx, sessionID, domain.Turn{
		Text:    text,
		Origin:  origin,
		Targets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: origin.ClientID}},
	})
}

// SendTurnTo starts a run whose output is delivered somewhere named by the
// caller.
//
// Targets belong to the run rather than the session, so taking over a
// conversation from a GUI does not echo the operator's own notes back to the
// channel it came from.
func (r *Runtime) SendTurnTo(
	ctx context.Context,
	sessionID domain.SessionID,
	turn domain.Turn,
) (domain.RunID, domain.MessageID, error) {
	r.mu.RLock()
	draining := r.draining
	r.mu.RUnlock()

	if draining {
		return "", "", ErrShuttingDown
	}

	// Reading the session first turns an unknown ID into a clean not-found
	// rather than a failure partway through writing the log.
	if _, err := r.opts.Store.Session(ctx, sessionID); err != nil {
		return "", "", err
	}

	runID := domain.RunID(r.opts.NewRunID())
	messageID := domain.MessageID(r.opts.NewMessageID())

	run := domain.Run{
		ID:              runID,
		SessionID:       sessionID,
		Status:          domain.RunQueued,
		Origin:          turn.Origin,
		DeliveryTargets: turn.Targets,
		CreatedAt:       r.opts.Now(),
	}
	if err := r.opts.Store.CreateRun(ctx, run); err != nil {
		return "", "", err
	}

	if err := r.append(ctx, sessionID, runID, domain.EventUserMessageAdded, domain.UserMessageAdded{
		MessageID: messageID,
		Text:      turn.Text,
		// Trust follows where the turn came from. A message that arrived
		// through a gateway is text from an account on somebody else's
		// service, and recording it as though the operator typed it would
		// throw away the one thing that distinguishes them.
		Trust:       trustForOrigin(turn.Origin),
		Origin:      turn.Origin,
		Attachments: turn.Attachments,
	}); err != nil {
		return "", "", err
	}

	// The run's context descends from the runtime group rather than the
	// request, so the run outlives the RPC that started it.
	if err := r.admit(ctx, run); err != nil {
		return "", "", err
	}
	return runID, messageID, nil
}

// queuedRun is a run waiting for the one before it in its session to finish.
type queuedRun struct {
	run     domain.Run
	ctx     context.Context
	cancel  context.CancelCauseFunc
	tracked *activeRun
}

// admit starts a run if its session is idle and queues it otherwise.
//
// Tracked either way, from the moment it is admitted: Wait blocks on it and
// InterruptRun can reach it whether it is executing or still in line. A run
// that has to wait says so in the log — the Queued transition is what a
// channel reacts to — and one that can start at once does not, so a lone
// message never flickers through a queue it was never in.
func (r *Runtime) admit(ctx context.Context, run domain.Run) error {
	runCtx, cancel := context.WithCancelCause(r.groupCtx)
	tracked := &activeRun{session: run.SessionID, cancel: cancel, done: make(chan struct{}), owner: true}
	waiting := queuedRun{run: run, ctx: runCtx, cancel: cancel, tracked: tracked}

	r.mu.Lock()
	r.active[run.ID] = tracked
	if r.busy[run.SessionID] {
		r.queued[run.SessionID] = append(r.queued[run.SessionID], waiting)
		r.mu.Unlock()
		return r.transition(ctx, run, domain.RunQueued, "")
	}
	r.busy[run.SessionID] = true
	r.mu.Unlock()

	r.launch(waiting)
	return nil
}

// launch executes a run in the group, and hands the session to the next in
// line when it is done.
func (r *Runtime) launch(waiting queuedRun) {
	r.group.Go(func() error {
		defer r.releaseRun(waiting.run.ID, waiting.tracked)
		defer waiting.cancel(nil)
		if waiting.ctx.Err() != nil {
			// Interrupted while it waited. Said so, rather than executed
			// against a context that is already gone.
			r.finish(context.WithoutCancel(waiting.ctx), waiting.run, domain.RunCancelled, "cancelled while queued")
			return nil
		}
		// A failing run is a normal outcome recorded in the log, not a reason
		// to tear down the whole daemon, so this never returns a non-nil error.
		r.execute(waiting.ctx, waiting.run)
		return nil
	})
}

// dequeue takes a run out of its session's line, if it is there.
func (r *Runtime) dequeue(session domain.SessionID, id domain.RunID) (queuedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := r.queued[session]
	for index, waiting := range line {
		if waiting.run.ID == id {
			r.queued[session] = append(line[:index:index], line[index+1:]...)
			return waiting, true
		}
	}
	return queuedRun{}, false
}

// next hands the session to the run that has waited longest, or frees it.
func (r *Runtime) next(session domain.SessionID) (queuedRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := r.queued[session]
	if len(line) == 0 {
		delete(r.busy, session)
		delete(r.queued, session)
		return queuedRun{}, false
	}
	r.queued[session] = line[1:]
	return line[0], true
}

func (r *Runtime) execute(ctx context.Context, run domain.Run) {
	maxIterations := r.opts.MaxIterations
	if maxIterations <= 0 {
		maxIterations = defaultMaxIterations
	}

	// A delegated search is bounded harder than a conversation: what it was
	// asked is a question with an answer, and one that needs more turns than
	// this is one the run should be doing itself where somebody can watch.
	if run.Kind == domain.RunWorker && maxIterations > workerIterations {
		maxIterations = workerIterations
	}

	declarations := r.declarationsFor(run)
	system, err := r.systemPrompt(ctx, run)
	if err != nil {
		r.finishFromError(ctx, run, err)
		return
	}

	// Fixed for the life of the run, and not small: a dozen tool schemas is
	// real weight, and history must not be allowed to grow into it.
	//
	// A deferred tool the session loads mid-run is declared on top of this
	// per request and, like the plan, is not counted here: a load is one
	// schema, bounded and rare, where history is what grows without limit.
	overhead := estimateRequestOverhead(system, declarations)

	// The loop is the agent: settle whatever is outstanding, generate, act on
	// what the model asked for, generate again with what was observed.
	//
	// Settling comes first so that resuming after an approval is the same code
	// path as running normally. A resumed run simply starts with calls already
	// requested and now answered.
	for {
		outstanding, err := r.outstandingCalls(ctx, run)
		if err != nil {
			r.finishFromError(ctx, run, err)
			return
		}

		if len(outstanding) > 0 {
			suspended, err := r.runTools(ctx, run, outstanding)
			if err != nil {
				r.finishFromError(ctx, run, err)
				return
			}
			if suspended {
				// Parked on a human. The run continues when the answer
				// arrives, from whichever process is alive then.
				return
			}
			continue
		}

		// The budget counts model turns already in the log rather than
		// iterations of this loop, so a run resumed in a new process cannot
		// quietly start its allowance over.
		turns, err := r.modelTurns(ctx, run)
		if err != nil {
			r.finishFromError(ctx, run, err)
			return
		}
		if turns >= maxIterations {
			// Say why it stopped. A run that simply ends after N silent
			// iterations is indistinguishable from one that finished.
			r.finish(ctx, run, domain.RunFailed,
				fmt.Sprintf("runtime: stopped after %d model turns without a final answer", maxIterations))
			return
		}

		// Here, and only here: every call the model asked for has a recorded
		// result, so folding history cannot separate a call from its result.
		r.compactIfNeeded(ctx, run, overhead)

		messages, err := r.buildConversation(ctx, run)
		if err != nil {
			r.finishFromError(ctx, run, err)
			return
		}

		if turns == 0 {
			if err := r.transition(ctx, run, domain.RunRunning, ""); err != nil {
				r.finish(context.WithoutCancel(ctx), run, domain.RunFailed, err.Error())
				return
			}
		}

		calls, err := r.generateTurn(ctx, run, provider.Request{
			Model:    r.modelFor(ctx, run.SessionID),
			System:   r.withPlan(ctx, run.SessionID, system),
			Messages: r.withRecalled(ctx, run, messages),
			Tools:    r.withLoaded(ctx, run, declarations),
		})
		if err != nil {
			// generateTurn has already recorded the outcome.
			return
		}

		if len(calls) == 0 {
			r.finish(ctx, run, domain.RunCompleted, "")
			return
		}
	}
}

// abort ends a run that could not continue, preserving whatever the model
// already produced.
//
// Buffered text has to be written even when the run is being cancelled:
// interrupting a reply and finding the words already on screen have vanished
// is worse than never having coalesced at all. The write uses a context that
// outlives the cancellation, since the run's own context is already dead.
func (r *Runtime) abort(ctx context.Context, run domain.Run, output *coalescer, cause error) {
	if flushErr := output.flush(context.WithoutCancel(ctx)); flushErr != nil {
		r.opts.Logger.Error("failed to persist partial output",
			"run_id", string(run.ID),
			"error", flushErr,
		)
	}
	r.finishFromError(ctx, run, cause)
}

func (r *Runtime) finishFromError(ctx context.Context, run domain.Run, err error) {
	// The run's own context is already dead by this point, so the terminal
	// state has to be written with a context that outlives it. Without this,
	// an interrupted run would leave no record of how it ended.
	writeCtx := context.WithoutCancel(ctx)

	if cause := context.Cause(ctx); errors.Is(cause, ErrUserInterrupted) {
		r.finish(writeCtx, run, domain.RunCancelled, cause.Error())
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		r.finish(writeCtx, run, domain.RunCancelled, err.Error())
		return
	}

	r.finish(writeCtx, run, domain.RunFailed, err.Error(), classifyFailure(err))
}

// classifyFailure names what went wrong in a form a client can branch on.
//
// The alternative is every client matching on the sentence in Reason, which is
// often a provider's own prose: written for a person, changed without notice,
// and in at least one case posted verbatim into a chat channel because nothing
// downstream had any other way to tell one failure from another.
func classifyFailure(err error) string {
	if errors.Is(err, ErrUserInterrupted) {
		return "interrupted"
	}

	var exhausted *provider.ErrRetryBudgetExhausted
	if errors.As(err, &exhausted) {
		// Retryable on its merits, abandoned because the wait was longer than
		// this request was allowed to take. That is a different thing to tell
		// somebody than a failure, so it keeps its own name.
		return "retry_budget_exhausted"
	}

	if kind := provider.KindOf(err); kind != provider.KindUnknown {
		return string(kind)
	}
	return ""
}

func (r *Runtime) finish(
	ctx context.Context,
	run domain.Run,
	status domain.RunStatus,
	reason string,
	failureKind ...string,
) {
	var kind string
	if len(failureKind) > 0 {
		kind = failureKind[0]
	}

	if err := r.transition(ctx, run, status, reason, kind); err != nil {
		// Nowhere left to report this: the log itself is the reporting
		// channel. Surfacing it in the daemon log is all that remains.
		r.opts.Logger.Error("failed to record terminal run state",
			"run_id", string(run.ID),
			"status", string(status),
			"error", err,
		)
		return
	}

	r.tellAfterRun(run, status)
}

// tellAfterRun reports a completed conversation turn to whoever asked to
// hear about them, in the group so shutdown waits for it, and never from the
// run's own goroutine so the session is handed on the moment the answer is.
func (r *Runtime) tellAfterRun(run domain.Run, status domain.RunStatus) {
	if r.opts.AfterRun == nil || status != domain.RunCompleted || run.Kind == domain.RunWorker {
		return
	}
	// transition wrote the new state onto its own copy; the one told about
	// the run should not be reading a state it has already left.
	run.Status = status
	r.group.Go(func() error {
		r.opts.AfterRun(r.groupCtx, run)
		return nil
	})
}

// transition writes the run's new state, then the event announcing it. Order
// matters: a client that sees the event and immediately queries the run must
// not find it still in the old state.
func (r *Runtime) transition(
	ctx context.Context,
	run domain.Run,
	status domain.RunStatus,
	reason string,
	failureKind ...string,
) error {
	run.Status = status
	if status.IsTerminal() {
		now := r.opts.Now()
		run.FinishedAt = &now
	}

	if err := r.opts.Store.UpdateRun(ctx, run); err != nil {
		return err
	}

	var kind string
	if len(failureKind) > 0 {
		kind = failureKind[0]
	}

	return r.append(ctx, run.SessionID, run.ID, domain.EventRunStateChanged, domain.RunStateChanged{
		Status:      status,
		Reason:      reason,
		FailureKind: kind,
	})
}

// InterruptRun asks a run to stop. It is cooperative: the cancellation
// propagates into the provider and, in M1, into tools and subprocesses, and
// the terminal event is written by the run itself.
func (r *Runtime) InterruptRun(ctx context.Context, id domain.RunID, reason string) (domain.RunStatus, error) {
	run, err := r.opts.Store.Run(ctx, id)
	if err != nil {
		return "", err
	}
	if run.Status.IsTerminal() {
		return run.Status, nil
	}

	r.mu.RLock()
	tracked, ok := r.active[id]
	r.mu.RUnlock()

	if !ok {
		// Known to storage but not driven by this process: it was orphaned by
		// a restart and recovery has not resolved it yet.
		return run.Status, ErrRunNotFound
	}

	cause := ErrUserInterrupted
	if reason != "" {
		cause = fmt.Errorf("%w: %s", ErrUserInterrupted, reason)
	}

	// Still in line: it never started, so there is nothing to stop — it is
	// taken out of the line and finished as cancelled here and now, rather
	// than left to discover the cancellation when its turn comes.
	if waiting, queued := r.dequeue(run.SessionID, id); queued {
		waiting.cancel(cause)
		r.finish(ctx, waiting.run, domain.RunCancelled, reason)
		r.releaseRun(id, waiting.tracked)
		return domain.RunCancelled, nil
	}
	tracked.cancel(cause)

	return domain.RunCancelling, nil
}

// releaseRun unregisters a run whose goroutine is finishing.
//
// This has to happen whether the run ended or merely parked on an approval.
// Leaving the entry behind would make a later resume believe the run was
// already moving, and a run waiting on a human would never continue.
func (r *Runtime) releaseRun(id domain.RunID, tracked *activeRun) {
	r.mu.Lock()
	if current, ok := r.active[id]; ok && current == tracked {
		delete(r.active, id)
	}
	r.mu.Unlock()

	close(tracked.done)

	// The session is this run's to hand on. Whoever waited longest goes
	// next; nobody waiting frees the session for the next message to start
	// at once.
	if !tracked.owner {
		return
	}
	if following, ok := r.next(tracked.session); ok {
		r.launch(following)
	}
}

// Wait blocks until a run stops moving: finished, failed, or parked on a
// human. It is not an error to wait for a run this process is no longer
// driving, because parking is a normal outcome.
func (r *Runtime) Wait(ctx context.Context, id domain.RunID) error {
	r.mu.RLock()
	tracked, ok := r.active[id]
	r.mu.RUnlock()

	if !ok {
		run, err := r.opts.Store.Run(ctx, id)
		if err != nil {
			return err
		}
		if run.Status.IsTerminal() || isDurablyPaused(run.Status) {
			return nil
		}
		return ErrRunNotFound
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tracked.done:
		return nil
	}
}

// Shutdown stops accepting work, interrupts everything still running, and
// waits for every run goroutine to return.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.draining = true
	active := make([]*activeRun, 0, len(r.active))
	for _, tracked := range r.active {
		active = append(active, tracked)
	}
	r.mu.Unlock()

	for _, tracked := range active {
		tracked.cancel(ErrShuttingDown)
	}

	waited := make(chan error, 1)
	go func() { waited <- r.group.Wait() }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-waited:
		return err
	}
}

// append writes to the log and then wakes subscribers. Order matters: the
// event must be readable before anyone is told to go read it.
func (r *Runtime) append(ctx context.Context, sessionID domain.SessionID, runID domain.RunID, kind domain.EventKind, payload domain.EventPayload) error {
	event := domain.Event{
		ID:         domain.EventID(r.opts.NewEventID()),
		SessionID:  sessionID,
		RunID:      runID,
		OccurredAt: r.opts.Now(),
		Kind:       kind,
		Payload:    payload,
	}

	seq, err := r.opts.Store.Append(ctx, event)
	if err != nil {
		return err
	}
	event.Seq = seq

	r.opts.Hub.Publish(sessionID)
	r.notifyDelivery(ctx, runID, event)

	return nil
}

// notifyDelivery forwards an event to the delivery observer, if the run has
// somewhere external to send it.
//
// Errors are logged rather than returned. The work already happened; failing
// the run because a chat channel could not be told about it would discard
// something that succeeded.
func (r *Runtime) notifyDelivery(ctx context.Context, runID domain.RunID, event domain.Event) {
	if r.opts.Delivery == nil || runID == "" {
		return
	}

	run, err := r.opts.Store.Run(ctx, runID)
	if err != nil || !hasExternalTarget(run) {
		return
	}

	if err := r.opts.Delivery.Observe(ctx, run, event); err != nil {
		r.opts.Logger.Error("failed to queue a delivery",
			"run_id", string(runID),
			"event", string(event.Kind),
			"error", err,
		)
	}
}

// hasExternalTarget reports whether a run's output belongs anywhere other than
// the control client that started it.
func hasExternalTarget(run domain.Run) bool {
	for _, target := range run.DeliveryTargets {
		if target.Kind != domain.DeliveryLocalClient {
			return true
		}
	}
	return false
}

// coalescer batches provider deltas into events worth persisting.
type coalescer struct {
	rt        *Runtime
	run       domain.Run
	messageID domain.MessageID

	limits Coalescing

	text          strings.Builder
	lastTextFlush time.Time

	reasoning          strings.Builder
	lastReasoningFlush time.Time

	usage          domain.Usage
	usagePending   bool
	lastUsageFlush time.Time
}

func (r *Runtime) newCoalescer(run domain.Run, messageID domain.MessageID) *coalescer {
	now := r.opts.Now()
	return &coalescer{
		rt:                 r,
		run:                run,
		messageID:          messageID,
		limits:             r.opts.Coalescing.withDefaults(),
		lastTextFlush:      now,
		lastReasoningFlush: now,
		lastUsageFlush:     now,
	}
}

func (c *coalescer) addText(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	c.text.WriteString(text)

	elapsed := c.rt.opts.Now().Sub(c.lastTextFlush)
	if c.text.Len() < c.limits.TextFlushBytes && elapsed < c.limits.TextFlushInterval {
		return nil
	}
	return c.flushText(ctx)
}

// addReasoning buffers working-out on the same cadence as the answer.
//
// Its own buffer rather than the text one: interleaved, a flush would emit a
// chunk that is part answer and part reasoning under one kind, and no reader
// could separate them again.
func (c *coalescer) addReasoning(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	c.reasoning.WriteString(text)

	elapsed := c.rt.opts.Now().Sub(c.lastReasoningFlush)
	if c.reasoning.Len() < c.limits.TextFlushBytes && elapsed < c.limits.TextFlushInterval {
		return nil
	}
	return c.flushReasoning(ctx)
}

func (c *coalescer) setUsage(ctx context.Context, usage domain.Usage) error {
	c.usage = usage
	c.usagePending = true

	if c.rt.opts.Now().Sub(c.lastUsageFlush) < c.limits.UsageFlushInterval {
		return nil
	}
	return c.flushUsage(ctx)
}

// flush emits everything still buffered.
//
// Reasoning first, then the answer, then the accounting that describes it: a
// model thinks before it replies, and a log that shows the working-out after
// the conclusion reads as a justification written afterwards.
func (c *coalescer) flush(ctx context.Context) error {
	if err := c.flushReasoning(ctx); err != nil {
		return err
	}
	if err := c.flushText(ctx); err != nil {
		return err
	}
	return c.flushUsage(ctx)
}

func (c *coalescer) flushReasoning(ctx context.Context) error {
	if c.reasoning.Len() == 0 {
		return nil
	}

	text := c.reasoning.String()
	c.reasoning.Reset()
	c.lastReasoningFlush = c.rt.opts.Now()

	return c.rt.append(ctx, c.run.SessionID, c.run.ID, domain.EventAssistantReasoningDelta,
		domain.AssistantReasoningDelta{MessageID: c.messageID, Text: text})
}

func (c *coalescer) flushText(ctx context.Context) error {
	if c.text.Len() == 0 {
		return nil
	}

	text := c.text.String()
	c.text.Reset()
	c.lastTextFlush = c.rt.opts.Now()

	return c.rt.append(ctx, c.run.SessionID, c.run.ID, domain.EventAssistantTextDelta,
		domain.AssistantTextDelta{MessageID: c.messageID, Text: text})
}

func (c *coalescer) flushUsage(ctx context.Context) error {
	if !c.usagePending {
		return nil
	}

	c.usagePending = false
	c.lastUsageFlush = c.rt.opts.Now()

	return c.rt.append(ctx, c.run.SessionID, c.run.ID, domain.EventUsageChanged,
		domain.UsageChanged{Usage: c.usage})
}

// modelFor is the model this session answers with.
//
// The session's own choice where it has one, otherwise the configured
// default. Read per turn rather than held for the run, so a change takes
// effect at the next turn rather than at the next conversation — and read
// through a failure to a sensible answer, because a session row that cannot
// be read is a reason to use the default rather than a reason not to answer.
func (r *Runtime) modelFor(ctx context.Context, sessionID domain.SessionID) string {
	session, err := r.opts.Store.Session(ctx, sessionID)
	if err != nil || session.Model == "" {
		return r.opts.Model
	}
	return session.Model
}

// generateTurn runs one model call, recording its output, and returns any
// tools the model asked for.
//
// On failure it records the terminal state itself and returns an error purely
// to tell the caller to stop; the outcome is already in the log.
func (r *Runtime) generateTurn(
	ctx context.Context,
	run domain.Run,
	request provider.Request,
) ([]provider.ToolCallRequested, error) {
	messageID := domain.MessageID(r.opts.NewMessageID())
	output := r.newCoalescer(run, messageID)

	stream, err := r.opts.Provider.Generate(ctx, request)
	if err != nil {
		r.finishFromError(ctx, run, err)
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var (
		calls      []provider.ToolCallRequested
		stopReason = domain.StopEndTurn
	)

	for {
		ev, recvErr := stream.Recv(ctx)
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			r.abort(ctx, run, output, recvErr)
			return nil, recvErr
		}

		switch e := ev.(type) {
		case provider.TextDelta:
			if err := output.addText(ctx, e.Text); err != nil {
				r.abort(ctx, run, output, err)
				return nil, err
			}

		case provider.ToolCallRequested:
			// Text buffered before the call belongs with the call, in the
			// order the model produced it.
			if err := output.flushText(ctx); err != nil {
				r.abort(ctx, run, output, err)
				return nil, err
			}
			if e.ID == "" {
				// Some providers omit the id. One is still needed to pair the
				// result with its request.
				e.ID = r.opts.NewEventID()
			}
			calls = append(calls, e)

			if err := r.append(ctx, run.SessionID, run.ID, domain.EventToolCallRequested,
				domain.ToolCallRequested{
					CallID:           domain.ToolCallID(e.ID),
					Name:             e.Name,
					Arguments:        string(e.Args),
					ProviderMetadata: string(e.Opaque),
				}); err != nil {
				r.abort(ctx, run, output, err)
				return nil, err
			}

		case provider.UsageDelta:
			// Providers report cumulative totals, so the latest wins rather
			// than being summed.
			if err := output.setUsage(ctx, e.Usage); err != nil {
				r.abort(ctx, run, output, err)
				return nil, err
			}

		case provider.ReasoningDelta:
			// Recorded, and deliberately not as text. This is the path that
			// ends with an answer posted to whoever asked; the working-out
			// travels under its own kind so that everything downstream has to
			// decide about it rather than acquire it by falling through.
			if err := output.addReasoning(ctx, e.Text); err != nil {
				r.abort(ctx, run, output, err)
				return nil, err
			}

		case provider.Completed:
			stopReason = e.StopReason
		}
	}

	// Anything still buffered has to land before the turn is declared over,
	// or a client would see a completed message missing its last words.
	if err := output.flush(ctx); err != nil {
		r.abort(ctx, run, output, err)
		return nil, err
	}

	if err := r.append(ctx, run.SessionID, run.ID, domain.EventAssistantMessageCompleted,
		domain.AssistantMessageCompleted{MessageID: messageID, StopReason: stopReason}); err != nil {
		r.abort(ctx, run, output, err)
		return nil, err
	}

	return calls, nil
}

// runTools executes calls that are outstanding for this run.
//
// It returns true when the run must park: at least one call needs a human, and
// nothing further can happen until someone answers.
//
// Execution is sequential. The registry knows which tools are parallel-safe,
// but two calls can still contend for the same file, and getting that wrong
// costs far more than the latency saved.
func (r *Runtime) runTools(ctx context.Context, run domain.Run, calls []pendingCall) (bool, error) {
	var (
		suspended bool
		why       parked
	)

	for _, pending := range calls {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		// Worked out per call rather than once for the batch: the answer
		// depends on where the call sits, and results landing between them
		// change it.
		trust, err := r.conversationTrust(ctx, run, pending.Seq)
		if err != nil {
			return false, err
		}
		from, err := r.provenanceSoFar(ctx, run, pending.Seq)
		if err != nil {
			return false, err
		}

		call := tool.Call{
			ID:        string(pending.CallID),
			Name:      pending.Name,
			Arguments: pending.Arguments,
			Context: tool.CallContext{
				SessionID: string(run.SessionID),
				RunID:     string(run.ID),
				Origin:    run.Origin,
				Seq:       pending.Seq,
				Trust:     trust,
				From:      from,
			},
		}

		result, park, err := r.settleCall(ctx, run, call)
		if err != nil {
			return false, err
		}
		if park != notParked {
			suspended, why = true, park
			// Later calls in the same turn stay outstanding on purpose: they
			// may well depend on the one being reviewed, and running them
			// first would act on an assumption the human has not confirmed.
			break
		}

		// What the run has spent, said once, when it becomes true. The model
		// cannot see this and has no other way to learn it.
		if notice := r.spendNotice(ctx, run, call.Name); notice != "" {
			result.Content += notice
		}

		if err := r.recordToolResult(ctx, run, call, result, r.opts.Now()); err != nil {
			return false, err
		}
	}

	if suspended {
		r.suspend(ctx, run, why)
	}
	return suspended, nil
}

// parked says why a call produced no result.
//
// Two reasons, and the difference is what a person sees: a run stopped on an
// approval is waiting for somebody to allow something, and one stopped on a
// question is waiting for somebody to say what they want. Reported as one
// state, every client would offer the wrong control.
type parked string

const (
	notParked         parked = ""
	parkedForApproval parked = "approval"
	parkedForAnswer   parked = "answer"
)

// settleCall resolves one call to a result, or to a reason it is waiting.
func (r *Runtime) settleCall(ctx context.Context, run domain.Run, call tool.Call) (tool.Result, parked, error) {
	if r.opts.Tools == nil {
		return tool.Errorf(tool.CodeNotFound, "",
			"no tools are available in this session").Result(), notParked, nil
	}

	// Withheld from a worker, and refused here as well as left out of what it
	// was told about. Leaving it out only decides what a well-behaved model
	// asks for; a name it invented, or one replayed from a conversation this
	// is not, would otherwise reach the registry and run.
	//
	// Before the approval path on purpose. A worker parking on an approval is
	// worse than a worker being refused: nobody is looking at it, it sits
	// underneath a tool call that is itself already waiting, and the person
	// who would decide is being asked about work they never saw begin.
	if run.Kind == domain.RunWorker && !readOnlyForWorkers[call.Name] {
		return tool.Errorf(tool.CodeNotFound,
			"Answer from what you can read, or say you cannot.",
			"no tool named %q", call.Name).Result(), notParked, nil
	}

	// Settled by finding an answer rather than by running anything. This is
	// the one tool whose result a person types.
	if call.Name == AskName {
		return r.settleAsk(ctx, run, call)
	}

	registered, known := r.opts.Tools.Lookup(call.Name)
	if !known {
		// An unknown tool never reaches the policy engine; there is nothing to
		// evaluate, and the model needs to be told it asked for something that
		// does not exist.
		return r.opts.Tools.Execute(ctx, call), notParked, nil
	}

	// A decision already made for this call wins: this is a resumed run.
	decided, err := r.opts.Store.ApprovalForCall(ctx, run.ID, domain.ToolCallID(call.ID))
	switch {
	case err == nil && decided.Status == domain.ApprovalAllowed:
		return r.opts.Tools.Execute(ctx, call), notParked, nil
	case err == nil && decided.Status == domain.ApprovalDenied:
		return deniedResult(call, "a human declined this action"), notParked, nil
	case err == nil && decided.IsPending():
		// Already waiting; do not raise a second prompt for it.
		return tool.Result{}, parkedForApproval, nil
	case err != nil && !errors.Is(err, storage.ErrApprovalNotFound):
		return tool.Result{}, notParked, err
	}

	switch outcome := r.evaluate(ctx, run, registered.Spec(), call); outcome.decision {
	case permission.Allow:
		return r.opts.Tools.Execute(ctx, call), notParked, nil

	case permission.Deny:
		return deniedResult(call, outcome.outcome.Reason), notParked, nil

	default:
		if err := r.requestApproval(ctx, run, registered, call, outcome.outcome); err != nil {
			return tool.Result{}, notParked, err
		}
		return tool.Result{}, parkedForApproval, nil
	}
}

func (r *Runtime) recordToolResult(
	ctx context.Context,
	run domain.Run,
	call tool.Call,
	result tool.Result,
	started time.Time,
) error {
	return r.append(ctx, run.SessionID, run.ID, domain.EventToolCallCompleted,
		domain.ToolCallCompleted{
			CallID:     domain.ToolCallID(call.ID),
			Name:       call.Name,
			Summary:    result.Summary,
			Content:    result.Content,
			IsError:    result.IsError,
			Truncated:  result.Truncated,
			Foreign:    r.returnsForeignContent(call.Name),
			From:       r.provenanceOf(call.Name),
			Artifact:   artifactOf(result),
			DurationMS: r.opts.Now().Sub(started).Milliseconds(),
		})
}

// provenanceOf is who wrote what a tool returns.
//
// Asked of the registry at the moment of the call and written onto the event,
// like Foreign beside it: a tool removed from the configuration must not make
// an old event unclassifiable.
//
// A tool nobody can find is external. Not because it is, but because the
// question here is what may be believed, and the answer for something that
// cannot be identified is the least of the three.
func (r *Runtime) provenanceOf(name string) domain.Provenance {
	if r.opts.Tools == nil {
		return domain.ProvenanceExternal
	}
	registered, ok := r.opts.Tools.Lookup(name)
	if !ok {
		return domain.ProvenanceExternal
	}
	return registered.Spec().Capabilities.Provenance
}

// returnsForeignContent asks whether a tool's results carry somebody else's
// words.
//
// Read from the tool now and written onto the event, so the question is
// answerable later from history alone. A tool removed from the configuration
// would otherwise make an old event unclassifiable.
func (r *Runtime) returnsForeignContent(name string) bool {
	if r.opts.Tools == nil {
		return false
	}
	registered, known := r.opts.Tools.Lookup(name)
	if !known {
		return false
	}
	return registered.Spec().Capabilities.ForeignContent
}

// artifactOf carries a tool's artifact reference into the log.
//
// The reference travels and the bytes do not, which is the whole point: this
// event is replayed into every subsequent request to the model.
func artifactOf(result tool.Result) *domain.Artifact {
	if result.Artifact == nil {
		return nil
	}
	return &domain.Artifact{
		ID:        result.Artifact.ID,
		Size:      result.Artifact.Size,
		MediaType: result.Artifact.MediaType,
	}
}

// trustForOrigin is how much a turn's own text is to be believed.
func trustForOrigin(origin domain.RunOrigin) domain.TrustLevel {
	if origin.Kind == domain.OriginGateway {
		return domain.TrustUntrusted
	}
	return domain.TrustUser
}

func (r *Runtime) toolDeclarations() []provider.ToolDeclaration {
	if r.opts.Tools == nil {
		return nil
	}

	specs := r.opts.Tools.Specs()
	declarations := make([]provider.ToolDeclaration, 0, len(specs))
	for _, spec := range specs {
		declarations = append(declarations, declarationOf(spec))
	}
	return declarations
}

func declarationOf(spec tool.Spec) provider.ToolDeclaration {
	return provider.ToolDeclaration{
		Name:        spec.Name,
		Description: spec.Description,
		InputSchema: spec.InputSchema,
	}
}

// withPlan appends the current plan to the system prompt for one turn.
//
// Fresh each turn rather than pinned with the rest of the prompt, because the
// plan is the one part of it the model itself changes: pinned, the model would
// mark a step done and then be shown the old list on the next turn, which is
// worse than having no plan at all.
//
// Not counted in the overhead estimate above, which is fixed for the run. The
// plan is bounded at twenty short lines, so what it can add is bounded too;
// history is what grows without limit, and that is what the estimate is
// protecting against.
func (r *Runtime) withPlan(
	ctx context.Context,
	session domain.SessionID,
	system []provider.ContentBlock,
) []provider.ContentBlock {
	items, err := r.opts.Store.Plan(ctx, session)
	if err != nil {
		// A plan that cannot be read is a reason to answer without it, not a
		// reason to fail the turn. The model still has its tools.
		r.opts.Logger.Warn("could not read the plan", "session_id", string(session), "error", err)
		return system
	}

	rendered := renderPlan(items)
	if rendered == "" {
		return system
	}
	return append(append([]provider.ContentBlock{}, system...), provider.Text(rendered)...)
}

func (r *Runtime) systemPrompt(ctx context.Context, run domain.Run) ([]provider.ContentBlock, error) {
	prompt := r.opts.SystemPrompt
	if run.Kind == domain.RunWorker && r.opts.WorkerSystemPrompt != "" {
		prompt = r.opts.WorkerSystemPrompt
	}

	extra, err := r.directionsFor(ctx, run)
	if err != nil {
		return nil, err
	}
	if extra != "" {
		prompt = strings.TrimRight(prompt, "\n") + "\n\n" + extra
	}

	if prompt == "" {
		return nil, nil
	}
	return provider.Text(prompt), nil
}

// directionsFor is the run-dependent part of the prompt, assembled once and
// then read back from the log.
//
// Assembled once because it comes from memory, and memory changes. A run that
// resumes after an approval — possibly in another process, hours later — must
// be given the same prompt it started with, or the log stops being enough to
// say what the model was told.
func (r *Runtime) directionsFor(ctx context.Context, run domain.Run) (string, error) {
	events, err := r.opts.Store.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return "", err
	}

	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		if recorded, ok := event.Payload.(domain.RunDirections); ok {
			return recorded.Text, nil
		}
	}

	if r.opts.SystemPromptFor == nil {
		return "", nil
	}

	directions := r.opts.SystemPromptFor(ctx, run)
	if directions == "" {
		// Nothing to say, and nothing worth an event. A later resume assembles
		// nothing again, which is the same answer.
		return "", nil
	}

	if err := r.append(ctx, run.SessionID, run.ID,
		domain.EventRunDirections, domain.RunDirections{Text: directions}); err != nil {
		return "", err
	}
	return directions, nil
}

// isNilOption reports whether an option was left unset, covering both a nil
// interface and a typed nil function.
func isNilOption(value any) bool {
	if value == nil {
		return true
	}

	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Func, reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// SkillActivated records that the model read an installed skill.
//
// On the runtime because appending to the log is what the runtime does, and
// because the alternative is a tool that can write events, which is a wider
// thing to be able to do than this needs.
func (r *Runtime) SkillActivated(
	ctx context.Context,
	session domain.SessionID,
	run domain.RunID,
	activated domain.SkillActivated,
) error {
	return r.append(ctx, session, run, domain.EventSkillActivated, activated)
}
