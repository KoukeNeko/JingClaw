package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/permission"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
	"github.com/KoukeNeko/JingClaw/core/internal/tool"
)

// ErrApprovalNotPending is returned when a decision arrives for something that
// has already been settled, which in practice means two clients answered the
// same prompt.
var ErrApprovalNotPending = errors.New("runtime: approval has already been decided")

// gate decides what happens to one tool call.
type gate struct {
	decision permission.Decision
	outcome  permission.Outcome
}

// evaluate asks the policy engine about a call.
//
// With no engine configured every call runs, which is what the read-only
// walking skeleton did. Configuring one is how a deployment opts into being
// asked, and the daemon does so by default.
func (r *Runtime) evaluate(ctx context.Context, run domain.Run, spec tool.Spec, call tool.Call) gate {
	if r.opts.Permissions == nil {
		return gate{decision: permission.Allow}
	}

	// A tool may be two different things depending on what it was asked to do,
	// and the policy is shown the one that applies here rather than the floor.
	if invoked, ok := r.opts.Tools.Lookup(call.Name); ok {
		spec.Level = tool.EffectiveLevel(invoked, call)
	}

	outcome := r.opts.Permissions.Evaluate(ctx, permission.Request{
		Spec:      spec,
		Call:      call,
		SessionID: run.SessionID,
		RunID:     run.ID,
		Origin:    run.Origin,
	})

	return gate{decision: outcome.Decision, outcome: outcome}
}

// requestApproval persists a pending approval and announces it.
//
// The record goes to storage before the event: a client that reacts to the
// event by listing pending approvals must find it there.
func (r *Runtime) requestApproval(
	ctx context.Context,
	run domain.Run,
	call tool.Call,
	outcome permission.Outcome,
) error {
	approval := domain.Approval{
		ID:         domain.ApprovalID(r.opts.NewApprovalID()),
		SessionID:  run.SessionID,
		RunID:      run.ID,
		ToolCallID: domain.ToolCallID(call.ID),
		ToolName:   call.Name,
		Arguments:  string(call.Arguments),
		Summary:    outcome.Summary,
		Effects:    outcome.Effects,
		Status:     domain.ApprovalPending,
		Scope:      domain.RememberOnce,
		CreatedAt:  r.opts.Now(),
	}

	if err := r.opts.Store.CreateApproval(ctx, approval); err != nil {
		return err
	}

	return r.append(ctx, run.SessionID, run.ID, domain.EventApprovalRequested,
		domain.ApprovalRequested{
			ApprovalID: approval.ID,
			CallID:     approval.ToolCallID,
			ToolName:   approval.ToolName,
			Arguments:  approval.Arguments,
			Summary:    approval.Summary,
			Effects:    approval.Effects,
		})
}

// DecideApproval records a human's answer and resumes the run.
//
// The decision is settled in storage first, and storage refuses to settle it
// twice. That is what stops two clients answering the same prompt from
// starting the run twice.
func (r *Runtime) DecideApproval(
	ctx context.Context,
	id domain.ApprovalID,
	allow bool,
	scope domain.RememberScope,
	decidedBy string,
) (domain.Approval, error) {
	status := domain.ApprovalDenied
	if allow {
		status = domain.ApprovalAllowed
	}
	if scope == "" {
		scope = domain.RememberOnce
	}

	approval, err := r.opts.Store.DecideApproval(ctx, id, status, scope, decidedBy, r.opts.Now())
	if err != nil {
		if errors.Is(err, storage.ErrApprovalDecided) {
			return domain.Approval{}, ErrApprovalNotPending
		}
		return domain.Approval{}, err
	}

	// A session-scoped grant is a policy change for the rest of this
	// conversation, so it is recorded before the run continues and sees it.
	if allow && scope == domain.RememberSession && r.opts.Permissions != nil {
		r.opts.Permissions.GrantForSession(approval.SessionID, approval.ToolName)
	}

	if err := r.append(ctx, approval.SessionID, approval.RunID, domain.EventApprovalResolved,
		domain.ApprovalResolved{
			ApprovalID: approval.ID,
			CallID:     approval.ToolCallID,
			ToolName:   approval.ToolName,
			Status:     approval.Status,
			Scope:      approval.Scope,
			DecidedBy:  decidedBy,
		}); err != nil {
		return domain.Approval{}, err
	}

	if err := r.resume(ctx, approval.RunID); err != nil {
		return domain.Approval{}, err
	}

	return approval, nil
}

// PendingApprovals lists what is waiting in a session.
func (r *Runtime) PendingApprovals(ctx context.Context, session domain.SessionID) ([]domain.Approval, error) {
	return r.opts.Store.PendingApprovals(ctx, session)
}

// resume restarts a run that was paused waiting on a human.
//
// Resuming rebuilds everything from the log rather than from anything held in
// memory, so an answer given after a daemon restart works exactly like one
// given a second later.
func (r *Runtime) resume(ctx context.Context, runID domain.RunID) error {
	r.mu.Lock()
	if r.draining {
		r.mu.Unlock()
		return ErrShuttingDown
	}
	if _, alreadyRunning := r.active[runID]; alreadyRunning {
		// Two approvals for the same run can be answered in quick succession;
		// only the first should start it moving again.
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	run, err := r.opts.Store.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.IsTerminal() {
		return nil
	}

	// Still waiting on another prompt: the run stays paused until every
	// outstanding call has an answer.
	pending, err := r.opts.Store.PendingApprovals(ctx, run.SessionID)
	if err != nil {
		return err
	}
	for _, approval := range pending {
		if approval.RunID == runID {
			return nil
		}
	}

	runCtx, cancel := context.WithCancelCause(r.groupCtx)
	tracked := &activeRun{cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.active[runID] = tracked
	r.mu.Unlock()

	r.group.Go(func() error {
		defer r.releaseRun(runID, tracked)
		defer cancel(nil)

		r.execute(runCtx, run)
		return nil
	})

	return nil
}

// suspend parks a run until its approvals are answered.
func (r *Runtime) suspend(ctx context.Context, run domain.Run) {
	if err := r.transition(ctx, run, domain.RunAwaitingApproval, "waiting for approval"); err != nil {
		r.opts.Logger.Error("failed to record a suspended run",
			"run_id", string(run.ID), "error", err)
	}
}

// deniedResult is what the model is told when a human says no.
//
// It has to read as a decision rather than a malfunction, or the model will
// treat it as a transient failure and try the same call again.
func deniedResult(call tool.Call, reason string) tool.Result {
	message := fmt.Sprintf("%s was not permitted", call.Name)
	if reason != "" {
		message += ": " + reason
	}

	return tool.Errorf(tool.CodePermissionDenied,
		"Do not retry this call. Explain what you wanted to do, or propose a different approach.",
		"%s", message).Result()
}

// SetPermissions installs a policy engine after construction.
//
// Wiring normally happens in Options; this exists so a test can take a
// permissive harness and add the gate to it, rather than duplicating the
// whole setup.
func (r *Runtime) SetPermissions(engine *permission.Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.Permissions = engine
}
