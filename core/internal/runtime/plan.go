package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// maxPlanItems is where a plan stops being a plan and starts being the work.
// A model that writes forty steps has not planned; it has enumerated, and the
// list is then too long to be read by anybody it was written for.
const maxPlanItems = 20

// Plan is what the agent said it was going to do in this session.
func (r *Runtime) Plan(ctx context.Context, session domain.SessionID) ([]domain.PlanItem, error) {
	return r.opts.Store.Plan(ctx, session)
}

// UpdatePlan applies operations and announces the result.
//
// The whole plan goes into the event, not the operations: a client that joined
// late reads one entry and knows the plan, rather than having to replay every
// change since the session started.
func (r *Runtime) UpdatePlan(
	ctx context.Context,
	session domain.SessionID,
	ops []domain.PlanOpRequest,
) ([]domain.PlanItem, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("runtime: no operations")
	}

	items, err := r.opts.Store.Plan(ctx, session)
	if err != nil {
		return nil, err
	}

	for _, op := range ops {
		if items, err = r.applyPlanOp(items, op); err != nil {
			return nil, err
		}
	}

	if err := r.opts.Store.SetPlan(ctx, session, items, r.opts.Now()); err != nil {
		return nil, err
	}

	// Attributed to the run in flight where there is one, so a client can see
	// which turn changed the plan. A plan updated between runs — which nothing
	// does today — would carry an empty run id rather than a wrong one.
	if err := r.append(ctx, session, r.runForSession(session), domain.EventPlanChanged,
		domain.PlanChanged{Items: items}); err != nil {
		return nil, err
	}

	return items, nil
}

func (r *Runtime) applyPlanOp(items []domain.PlanItem, op domain.PlanOpRequest) ([]domain.PlanItem, error) {
	switch op.Op {
	case domain.PlanOpAdd:
		title := strings.TrimSpace(op.Title)
		if title == "" {
			return nil, fmt.Errorf("runtime: a step needs a title")
		}
		if len(items) >= maxPlanItems {
			return nil, fmt.Errorf(
				"runtime: a plan may hold %d steps, and this one is full", maxPlanItems)
		}
		return append(items, domain.PlanItem{
			ID:     r.opts.NewPlanItemID(),
			Title:  title,
			Status: domain.PlanPending,
			Note:   op.Note,
		}), nil

	case domain.PlanOpSetStatus:
		at, err := findPlanItem(items, op.ID)
		if err != nil {
			return nil, err
		}
		if !op.Status.IsValid() {
			return nil, fmt.Errorf("runtime: %q is not a status a step can be in", op.Status)
		}
		items[at].Status = op.Status
		if op.Note != "" {
			items[at].Note = op.Note
		}
		return items, nil

	case domain.PlanOpSetTitle:
		at, err := findPlanItem(items, op.ID)
		if err != nil {
			return nil, err
		}
		title := strings.TrimSpace(op.Title)
		if title == "" {
			return nil, fmt.Errorf("runtime: a step needs a title")
		}
		items[at].Title = title
		return items, nil

	default:
		return nil, fmt.Errorf("runtime: %q is not something that can be done to a plan", op.Op)
	}
}

// findPlanItem is deliberately an error rather than a no-op.
//
// A model that names a step that is not there has lost track of its own plan,
// and silently doing nothing would let it go on believing the step was marked
// done.
func findPlanItem(items []domain.PlanItem, id string) (int, error) {
	for i, item := range items {
		if item.ID == id {
			return i, nil
		}
	}
	return 0, fmt.Errorf("runtime: there is no step %q in the plan", id)
}

// renderPlan is the plan as the model is shown it on the next turn.
//
// Put in front of the model rather than left in the log, because a plan the
// model cannot see is a plan it has to reconstruct from its own earlier output
// — which is the thing this exists to replace.
func renderPlan(items []domain.PlanItem) string {
	if len(items) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString("Your plan for this session, as you last left it:\n")
	for _, item := range items {
		fmt.Fprintf(&out, "- [%s] %s (%s)\n", item.Status.Mark(), item.Title, item.ID)
		if item.Note != "" {
			fmt.Fprintf(&out, "      %s\n", item.Note)
		}
	}
	out.WriteString("Update it with todo_update as you go, rather than describing " +
		"changes to it in your answer.")
	return out.String()
}

// runForSession is the run currently driving this session, if one is.
//
// Read from what this process is running rather than from storage: a plan is
// changed by a tool call, which by definition happens inside a run this
// process owns.
func (r *Runtime) runForSession(session domain.SessionID) domain.RunID {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for id, tracked := range r.active {
		if tracked.session == session {
			return id
		}
	}
	return ""
}
