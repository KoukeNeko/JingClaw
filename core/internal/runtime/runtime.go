// Package runtime owns session and run lifecycle, the agent loop, and
// cancellation. It knows nothing about the wire protocol.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/event"
)

var (
	ErrSessionNotFound = errors.New("runtime: session not found")
	ErrRunNotFound     = errors.New("runtime: run not found")
	ErrShuttingDown    = errors.New("runtime: shutting down")

	// ErrUserInterrupted is the cancellation cause set by InterruptRun. It
	// distinguishes a deliberate stop from a failure, which is what decides
	// whether the run ends CANCELLED or FAILED.
	ErrUserInterrupted = errors.New("runtime: interrupted by user")
)

// IDGenerator produces identifiers. Injected so tests can be deterministic.
type IDGenerator func() string

type Options struct {
	Store    event.Store
	Hub      *event.Hub
	Provider Provider

	NewSessionID IDGenerator
	NewRunID     IDGenerator
	NewMessageID IDGenerator
	NewEventID   IDGenerator

	Now func() time.Time
}

type Runtime struct {
	opts Options

	mu       sync.RWMutex
	sessions map[domain.SessionID]*domain.Session
	runs     map[domain.RunID]*managedRun
	draining bool

	// group owns every run goroutine, so shutdown can wait for them instead of
	// hoping they finish. No bare `go f()` anywhere in this package.
	group    *errgroup.Group
	groupCtx context.Context
}

type managedRun struct {
	run    *domain.Run
	cancel context.CancelCauseFunc
	done   chan struct{}
}

func New(ctx context.Context, opts Options) *Runtime {
	group, groupCtx := errgroup.WithContext(ctx)

	return &Runtime{
		opts:     opts,
		sessions: make(map[domain.SessionID]*domain.Session),
		runs:     make(map[domain.RunID]*managedRun),
		group:    group,
		groupCtx: groupCtx,
	}
}

func (r *Runtime) CreateSession(ctx context.Context, title string) (*domain.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := r.opts.Now()
	session := &domain.Session{
		ID:        domain.SessionID(r.opts.NewSessionID()),
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}

	r.mu.Lock()
	if r.draining {
		r.mu.Unlock()
		return nil, ErrShuttingDown
	}
	r.sessions[session.ID] = session
	r.mu.Unlock()

	// Register the session in the log so subscribing before the first turn is
	// not mistaken for a missing session.
	if ensurer, ok := r.opts.Store.(interface{ EnsureSession(domain.SessionID) }); ok {
		ensurer.EnsureSession(session.ID)
	}

	return session, nil
}

func (r *Runtime) Session(id domain.SessionID) (*domain.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, ok := r.sessions[id]
	return session, ok
}

func (r *Runtime) Run(id domain.RunID) (*domain.Run, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	managed, ok := r.runs[id]
	if !ok {
		return nil, false
	}
	return managed.run, true
}

// SendTurn records the user's message and starts a run. It returns as soon as
// the run is accepted; the answer arrives over the event stream, so a client
// disconnecting never cancels an in-flight generation.
func (r *Runtime) SendTurn(ctx context.Context, sessionID domain.SessionID, text string, origin domain.RunOrigin) (domain.RunID, domain.MessageID, error) {
	r.mu.Lock()
	if r.draining {
		r.mu.Unlock()
		return "", "", ErrShuttingDown
	}
	if _, ok := r.sessions[sessionID]; !ok {
		r.mu.Unlock()
		return "", "", ErrSessionNotFound
	}
	r.mu.Unlock()

	runID := domain.RunID(r.opts.NewRunID())
	messageID := domain.MessageID(r.opts.NewMessageID())

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

	managed := &managedRun{
		run: &domain.Run{
			ID:              runID,
			SessionID:       sessionID,
			Status:          domain.RunQueued,
			Origin:          origin,
			DeliveryTargets: []domain.DeliveryTarget{{Kind: domain.DeliveryLocalClient, Ref: origin.ClientID}},
			CreatedAt:       r.opts.Now(),
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}

	r.mu.Lock()
	r.runs[runID] = managed
	r.mu.Unlock()

	r.group.Go(func() error {
		defer close(managed.done)
		defer cancel(nil)

		// A failing run is a normal outcome recorded in the log, not a reason
		// to tear down the whole daemon, so this never returns a non-nil error.
		r.execute(runCtx, managed, text)
		return nil
	})

	return runID, messageID, nil
}

func (r *Runtime) execute(ctx context.Context, managed *managedRun, userText string) {
	sessionID := managed.run.SessionID
	runID := managed.run.ID

	r.setStatus(managed, domain.RunRunning)
	if err := r.append(ctx, sessionID, runID, domain.EventRunStateChanged, domain.RunStateChanged{
		Status: domain.RunRunning,
	}); err != nil {
		r.finish(context.WithoutCancel(ctx), managed, domain.RunFailed, err.Error())
		return
	}

	messageID := domain.MessageID(r.opts.NewMessageID())

	// M0 runs a single model turn. The step sequence is what matters: tools
	// slot in between the stream and the terminal state without reshaping this.
	stream, err := r.opts.Provider.Generate(ctx, ModelRequest{LastUserText: userText})
	if err != nil {
		r.finishFromError(ctx, managed, err)
		return
	}
	defer func() { _ = stream.Close() }()

	for {
		ev, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.finishFromError(ctx, managed, err)
			return
		}

		delta, ok := ev.(TextDelta)
		if !ok {
			continue
		}

		if err := r.append(ctx, sessionID, runID, domain.EventAssistantTextDelta, domain.AssistantTextDelta{
			MessageID: messageID,
			Text:      delta.Text,
		}); err != nil {
			r.finishFromError(ctx, managed, err)
			return
		}
	}

	if err := r.append(ctx, sessionID, runID, domain.EventAssistantMessageCompleted, domain.AssistantMessageCompleted{
		MessageID: messageID,
	}); err != nil {
		r.finishFromError(ctx, managed, err)
		return
	}

	r.finish(ctx, managed, domain.RunCompleted, "")
}

func (r *Runtime) finishFromError(ctx context.Context, managed *managedRun, err error) {
	// The run's own context is already dead by this point, so the terminal
	// event has to be written with a context that outlives it. Without this,
	// an interrupted run would leave no record of how it ended.
	writeCtx := context.WithoutCancel(ctx)

	if cause := context.Cause(ctx); errors.Is(cause, ErrUserInterrupted) {
		r.finish(writeCtx, managed, domain.RunCancelled, cause.Error())
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		r.finish(writeCtx, managed, domain.RunCancelled, err.Error())
		return
	}

	r.finish(writeCtx, managed, domain.RunFailed, err.Error())
}

func (r *Runtime) finish(ctx context.Context, managed *managedRun, status domain.RunStatus, reason string) {
	now := r.opts.Now()

	r.mu.Lock()
	managed.run.Status = status
	managed.run.FinishedAt = &now
	r.mu.Unlock()

	// Best effort: if the log itself is failing there is nowhere left to
	// report it, and the in-memory status above is already correct.
	_ = r.append(ctx, managed.run.SessionID, managed.run.ID, domain.EventRunStateChanged, domain.RunStateChanged{
		Status: status,
		Reason: reason,
	})
}

// InterruptRun asks a run to stop. It is cooperative: the run transitions to
// CANCELLING, the cancellation propagates into the provider and, in M1, into
// tools and subprocesses, and the terminal event is written by the run itself.
func (r *Runtime) InterruptRun(ctx context.Context, id domain.RunID, reason string) (domain.RunStatus, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	r.mu.Lock()
	managed, ok := r.runs[id]
	if !ok {
		r.mu.Unlock()
		return "", ErrRunNotFound
	}
	if managed.run.Status.IsTerminal() {
		status := managed.run.Status
		r.mu.Unlock()
		return status, nil
	}
	managed.run.Status = domain.RunCancelling
	r.mu.Unlock()

	cause := ErrUserInterrupted
	if reason != "" {
		cause = fmt.Errorf("%w: %s", ErrUserInterrupted, reason)
	}
	managed.cancel(cause)

	return domain.RunCancelling, nil
}

// Wait blocks until a run reaches a terminal state. Used by tests and by
// shutdown.
func (r *Runtime) Wait(ctx context.Context, id domain.RunID) error {
	r.mu.RLock()
	managed, ok := r.runs[id]
	r.mu.RUnlock()

	if !ok {
		return ErrRunNotFound
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-managed.done:
		return nil
	}
}

// Shutdown stops accepting work, interrupts everything still running, and
// waits for every run goroutine to return.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.draining = true
	active := make([]*managedRun, 0, len(r.runs))
	for _, managed := range r.runs {
		if !managed.run.Status.IsTerminal() {
			active = append(active, managed)
		}
	}
	r.mu.Unlock()

	for _, managed := range active {
		managed.cancel(ErrShuttingDown)
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

func (r *Runtime) setStatus(managed *managedRun, status domain.RunStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	managed.run.Status = status
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
