package daemon

import (
	"context"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
)

// notesAfterRuns is the runtime hook that notes what a person said once they
// have been answered, or nil when the operator has that off.
func notesAfterRuns(
	cfg config.Config,
	options memorytool.Options,
	events memorytool.EventReader,
	model memorytool.Completer,
) func(context.Context, domain.Run) {
	if !cfg.Memory.Enabled || !cfg.Memory.Curate {
		return nil
	}
	curator := &memorytool.Curator{Options: options, Events: events, Model: model}
	return curator.AfterRun
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
