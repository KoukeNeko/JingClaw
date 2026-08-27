// Package runtime owns session and run lifecycle, the agent loop, and
// cancellation. It knows nothing about the wire protocol.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
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

// IDGenerator produces identifiers. Injected so tests can be deterministic.
type IDGenerator func() string

type Options struct {
	Store storage.Store
	Hub   *event.Hub

	Provider Provider

	NewSessionID IDGenerator
	NewRunID     IDGenerator
	NewMessageID IDGenerator
	NewEventID   IDGenerator

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

func New(ctx context.Context, opts Options) *Runtime {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
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
func (r *Runtime) RecoverOrphanedRuns(ctx context.Context) (int, error) {
	orphans, err := r.opts.Store.UnfinishedRuns(ctx)
	if err != nil {
		return 0, err
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

// SendTurn records the user's message and starts a run. It returns as soon as
// the run is accepted; the answer arrives over the event stream, so a client
// disconnecting never cancels an in-flight generation.
func (r *Runtime) SendTurn(ctx context.Context, sessionID domain.SessionID, text string, origin domain.RunOrigin) (domain.RunID, domain.MessageID, error) {
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
		Origin:          origin,
		DeliveryTargets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: origin.ClientID}},
		CreatedAt:       r.opts.Now(),
	}
	if err := r.opts.Store.CreateRun(ctx, run); err != nil {
		return "", "", err
	}

	if err := r.append(ctx, sessionID, runID, domain.EventUserMessageAdded, domain.UserMessageAdded{
		MessageID: messageID,
		Text:      text,
		// A turn typed by the operator into a control-plane client. Gateway
		// traffic will arrive as TrustUntrusted once M1b lands.
		Trust:  domain.TrustUser,
		Origin: origin,
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
		defer close(tracked.done)
		defer cancel(nil)

		// A failing run is a normal outcome recorded in the log, not a reason
		// to tear down the whole daemon, so this never returns a non-nil error.
		r.execute(runCtx, run, text)
		return nil
	})

	return runID, messageID, nil
}

func (r *Runtime) execute(ctx context.Context, run domain.Run, userText string) {
	if err := r.transition(ctx, run, domain.RunRunning, ""); err != nil {
		r.finish(context.WithoutCancel(ctx), run, domain.RunFailed, err.Error())
		return
	}

	messageID := domain.MessageID(r.opts.NewMessageID())

	// M0 runs a single model turn. The step sequence is what matters: tools
	// slot in between the stream and the terminal state without reshaping this.
	stream, err := r.opts.Provider.Generate(ctx, ModelRequest{LastUserText: userText})
	if err != nil {
		r.finishFromError(ctx, run, err)
		return
	}
	defer func() { _ = stream.Close() }()

	for {
		ev, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.finishFromError(ctx, run, err)
			return
		}

		delta, ok := ev.(TextDelta)
		if !ok {
			continue
		}

		if err := r.append(ctx, run.SessionID, run.ID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{
			MessageID: messageID,
			Text:      delta.Text,
		}); err != nil {
			r.finishFromError(ctx, run, err)
			return
		}
	}

	if err := r.append(ctx, run.SessionID, run.ID, domain.EventAssistantMessageCompleted, domain.AssistantMessageCompleted{
		MessageID: messageID,
	}); err != nil {
		r.finishFromError(ctx, run, err)
		return
	}

	r.finish(ctx, run, domain.RunCompleted, "")
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

	r.finish(writeCtx, run, domain.RunFailed, err.Error())
}

func (r *Runtime) finish(ctx context.Context, run domain.Run, status domain.RunStatus, reason string) {
	if err := r.transition(ctx, run, status, reason); err != nil {
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
func (r *Runtime) transition(ctx context.Context, run domain.Run, status domain.RunStatus, reason string) error {
	run.Status = status
	if status.IsTerminal() {
		now := r.opts.Now()
		run.FinishedAt = &now
	}

	if err := r.opts.Store.UpdateRun(ctx, run); err != nil {
		return err
	}

	return r.append(ctx, run.SessionID, run.ID, domain.EventRunStateChanged, domain.RunStateChanged{
		Status: status,
		Reason: reason,
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

// Wait blocks until a run this process is driving reaches a terminal state.
func (r *Runtime) Wait(ctx context.Context, id domain.RunID) error {
	r.mu.RLock()
	tracked, ok := r.active[id]
	r.mu.RUnlock()

	if !ok {
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
	_, err := r.opts.Store.Append(ctx, domain.Event{
		ID:         domain.EventID(r.opts.NewEventID()),
		SessionID:  sessionID,
		RunID:      runID,
		OccurredAt: r.opts.Now(),
		Kind:       kind,
		Payload:    payload,
	})
	if err != nil {
		return err
	}

	r.opts.Hub.Publish(sessionID)
	return nil
}
