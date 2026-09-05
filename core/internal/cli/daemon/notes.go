package daemon

import (
	"context"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
)

// Noter is told how many notes a run left behind, so the channel the run
// answered can say so under the answer.
type Noter interface {
	Noted(ctx context.Context, run domain.Run, count int) error
}

// notesAfterRuns is the runtime hook that notes what a person said once they
// have been answered, or nil when the operator has that off.
//
// A failure goes no further than the log: the run is already answered, and
// nothing about it should fail for what happens after it.
func notesAfterRuns(
	cfg config.Config,
	options memorytool.Options,
	events memorytool.EventReader,
	model memorytool.Completer,
	told Noter,
) func(context.Context, domain.Run) {
	if !cfg.Memory.Enabled || !cfg.Memory.Curate {
		return nil
	}
	curator := &memorytool.Curator{Options: options, Events: events, Model: model}
	return func(ctx context.Context, run domain.Run) {
		written, err := curator.Curate(ctx, run)
		if err != nil {
			curator.Logger().Warn("could not note what was said",
				"run_id", string(run.ID), "error", err)
			return
		}
		if len(written) == 0 {
			return
		}
		curator.Logger().Info("noted what was said",
			"run_id", string(run.ID), "memories", len(written))
		if err := told.Noted(ctx, run, len(written)); err != nil {
			curator.Logger().Warn("could not tell the channel what was noted",
				"run_id", string(run.ID), "error", err)
		}
	}
}

// notesBeforeTurns is the runtime hook that puts what was noted in front of
// the turn being answered, or nil when the operator has that off.
func notesBeforeTurns(
	cfg config.Config,
	options memorytool.Options,
) func(context.Context, domain.Run, string) string {
	if !cfg.Memory.Enabled || cfg.Memory.AutoRecall <= 0 {
		return nil
	}
	noted := &memorytool.Noted{
		Options:  options,
		Limit:    cfg.Memory.AutoRecall,
		MaxBytes: cfg.Memory.AutoRecallBytes,
	}
	return noted.For
}
