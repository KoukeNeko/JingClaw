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

	// SystemPromptFor contributes text that depends on the run, appended after
	// SystemPrompt once when the run starts.
	//
	// Appended rather than mixed in, so the part that never changes stays a
	// stable prefix — which is what prompt caching needs. It is per run and
	// not per process because what belongs here can differ by who is asking:
	// a turn from a chat account and one typed at this machine are not owed
	// the same recollections.
	SystemPromptFor func(ctx context.Context, run domain.Run) string

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

	// MaxIterations bounds the tool loop. A model that keeps calling tools
	// without converging must stop somewhere, and stopping with a recorded
	// reason beats burning quota until a human notices.
	MaxIterations int

	NewSessionID  IDGenerator
	NewRunID      IDGenerator
	NewMessageID  IDGenerator
	NewEventID    IDGenerator
	NewApprovalID IDGenerator

	Now    func() time.Time
	Logger *slog.Logger
}

type Runtime struct {
	opts Options

	mu       sync.RWMutex
	active   map[domain.RunID]*activeRun
	draining bool

	// group owns every run goroutine, so shutdown can wait for them instead of
	// hoping they finish. No bare `go f()` anywhere in this package.
	group    *errgroup.Group
	groupCtx context.Context
}

// activeRun tracks a run this process is currently driving. Runs that have
// finished, or that belong to a previous process, live only in storage.
type activeRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
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
	for _, run := range unfinished {
		if isDurablyPaused(run.Status) {
			r.opts.Logger.Info("leaving a paused run alone",
				"run_id", string(run.ID), "status", string(run.Status))
			continue
		}
		orphans = append(orphans, run)
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
	runCtx, cancel := context.WithCancelCause(r.groupCtx)

	tracked := &activeRun{cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.active[runID] = tracked
	r.mu.Unlock()

	r.group.Go(func() error {
		defer r.releaseRun(runID, tracked)
		defer cancel(nil)

		// A failing run is a normal outcome recorded in the log, not a reason
		// to tear down the whole daemon, so this never returns a non-nil error.
		r.execute(runCtx, run)
		return nil
	})

	return runID, messageID, nil
}

func (r *Runtime) execute(ctx context.Context, run domain.Run) {
	if err := r.transition(ctx, run, domain.RunRunning, ""); err != nil {
		r.finish(context.WithoutCancel(ctx), run, domain.RunFailed, err.Error())
		return
	}

	maxIterations := r.opts.MaxIterations
	if maxIterations <= 0 {
		maxIterations = defaultMaxIterations
	}

	declarations := r.toolDeclarations()
	system, err := r.systemPrompt(ctx, run)
	if err != nil {
		r.finishFromError(ctx, run, err)
		return
	}

	// Fixed for the life of the run, and not small: a dozen tool schemas is
	// real weight, and history must not be allowed to grow into it.
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

		messages, err := r.buildConversation(ctx, run.SessionID)
		if err != nil {
			r.finishFromError(ctx, run, err)
			return
		}

		calls, err := r.generateTurn(ctx, run, provider.Request{
			Model:    r.opts.Model,
			System:   system,
			Messages: messages,
			Tools:    declarations,
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
	}
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

	usage          domain.Usage
	usagePending   bool
	lastUsageFlush time.Time
}

func (r *Runtime) newCoalescer(run domain.Run, messageID domain.MessageID) *coalescer {
	now := r.opts.Now()
	return &coalescer{
		rt:             r,
		run:            run,
		messageID:      messageID,
		limits:         r.opts.Coalescing.withDefaults(),
		lastTextFlush:  now,
		lastUsageFlush: now,
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

func (c *coalescer) setUsage(ctx context.Context, usage domain.Usage) error {
	c.usage = usage
	c.usagePending = true

	if c.rt.opts.Now().Sub(c.lastUsageFlush) < c.limits.UsageFlushInterval {
		return nil
	}
	return c.flushUsage(ctx)
}

// flush emits everything still buffered. Text goes first so the reply reads in
// order before the accounting that describes it.
func (c *coalescer) flush(ctx context.Context) error {
	if err := c.flushText(ctx); err != nil {
		return err
	}
	return c.flushUsage(ctx)
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
	var suspended bool

	for _, pending := range calls {
		if err := ctx.Err(); err != nil {
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
			},
		}

		result, park, err := r.settleCall(ctx, run, call)
		if err != nil {
			return false, err
		}
		if park {
			suspended = true
			// Later calls in the same turn stay outstanding on purpose: they
			// may well depend on the one being reviewed, and running them
			// first would act on an assumption the human has not confirmed.
			break
		}

		if err := r.recordToolResult(ctx, run, call, result, r.opts.Now()); err != nil {
			return false, err
		}
	}

	if suspended {
		r.suspend(ctx, run)
	}
	return suspended, nil
}

// settleCall resolves one call to either a result or a request for approval.
func (r *Runtime) settleCall(ctx context.Context, run domain.Run, call tool.Call) (tool.Result, bool, error) {
	if r.opts.Tools == nil {
		return tool.Errorf(tool.CodeNotFound, "",
			"no tools are available in this session").Result(), false, nil
	}

	registered, known := r.opts.Tools.Lookup(call.Name)
	if !known {
		// An unknown tool never reaches the policy engine; there is nothing to
		// evaluate, and the model needs to be told it asked for something that
		// does not exist.
		return r.opts.Tools.Execute(ctx, call), false, nil
	}

	// A decision already made for this call wins: this is a resumed run.
	decided, err := r.opts.Store.ApprovalForCall(ctx, run.ID, domain.ToolCallID(call.ID))
	switch {
	case err == nil && decided.Status == domain.ApprovalAllowed:
		return r.opts.Tools.Execute(ctx, call), false, nil
	case err == nil && decided.Status == domain.ApprovalDenied:
		return deniedResult(call, "a human declined this action"), false, nil
	case err == nil && decided.IsPending():
		// Already waiting; do not raise a second prompt for it.
		return tool.Result{}, true, nil
	case err != nil && !errors.Is(err, storage.ErrApprovalNotFound):
		return tool.Result{}, false, err
	}

	switch outcome := r.evaluate(ctx, run, registered.Spec(), call); outcome.decision {
	case permission.Allow:
		return r.opts.Tools.Execute(ctx, call), false, nil

	case permission.Deny:
		return deniedResult(call, outcome.outcome.Reason), false, nil

	default:
		if err := r.requestApproval(ctx, run, call, outcome.outcome); err != nil {
			return tool.Result{}, false, err
		}
		return tool.Result{}, true, nil
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
			Artifact:   artifactOf(result),
			DurationMS: r.opts.Now().Sub(started).Milliseconds(),
		})
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
		declarations = append(declarations, provider.ToolDeclaration{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
		})
	}
	return declarations
}

func (r *Runtime) systemPrompt(ctx context.Context, run domain.Run) ([]provider.ContentBlock, error) {
	prompt := r.opts.SystemPrompt

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
