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
// replaces. It worked, and it quietly left every one of them out of the list
// the model is given — three tools the agent had and was never told about.
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
