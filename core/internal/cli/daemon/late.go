package daemon

import (
	"context"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/runtime"
)

// theRuntime is the runtime before there is one.
//
// A handful of tools have the runtime itself as their collaborator, and the
// runtime is built with the prompt, and the prompt lists the tools. Something
// has to give, and it is this: the tools are registered against a handle that
// is filled in as soon as the runtime exists.
//
// The alternative was registering them afterwards, which is what this
// replaces. It worked, and it quietly left four tools out of the list the
// model is given — including the one for delegating, which a model that is
// never told about will never use.
type theRuntime struct{ is *runtime.Runtime }

// UpdatePlan is builtin.Planner.
func (r *theRuntime) UpdatePlan(
	ctx context.Context, session domain.SessionID, ops []domain.PlanOpRequest,
) ([]domain.PlanItem, error) {
	return r.is.UpdatePlan(ctx, session, ops)
}

// SkillActivated is builtin.Activations.
func (r *theRuntime) SkillActivated(
	ctx context.Context, session domain.SessionID, run domain.RunID,
	activated domain.SkillActivated,
) error {
	return r.is.SkillActivated(ctx, session, run, activated)
}

// Investigate is builtin.Delegator.
func (r *theRuntime) Investigate(
	ctx context.Context, parent domain.RunID, question string,
) (string, error) {
	return r.is.Investigate(ctx, parent, question)
}
