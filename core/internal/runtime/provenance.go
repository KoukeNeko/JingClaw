package runtime

import (
	"context"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
)

// provenanceSoFar is the worst thing this run has read, up to now.
//
// The worst, because the invariant is one-directional: text may lose
// authority and may never gain it without a person or the runtime's own
// policy saying so. A run that read a page and then listed a directory has
// still read a page.
//
// Derived from the log rather than carried in memory, like everything else
// here, so a run resumed in another process reaches the same answer.
func (r *Runtime) provenanceSoFar(
	ctx context.Context, run domain.Run, before domain.Seq,
) (domain.Provenance, error) {
	events, err := r.opts.Store.ListAfter(ctx, run.SessionID, 0, 0)
	if err != nil {
		return domain.ProvenanceExternal, err
	}

	// A turn that came through a gateway is somebody else's words before it
	// has read anything at all.
	worst := domain.ProvenanceOperator
	if run.Origin.Kind == domain.OriginGateway {
		worst = domain.ProvenanceExternal
	}

	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		if before > 0 && event.Seq >= before {
			break
		}
		if done, ok := event.Payload.(domain.ToolCallCompleted); ok {
			worst = worst.Worse(done.From)
		}
	}
	return worst, nil
}

// provenanceNow is the same question asked about the whole run so far.
func (r *Runtime) provenanceNow(
	ctx context.Context, run domain.Run,
) (domain.Provenance, error) {
	head, err := r.opts.Store.Head(ctx, run.SessionID)
	if err != nil {
		return domain.ProvenanceExternal, err
	}
	return r.provenanceSoFar(ctx, run, head+1)
}
