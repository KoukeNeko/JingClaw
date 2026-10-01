package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
	memorytool "github.com/KoukeNeko/JingClaw/core/internal/tool/memory"
)

// Noter is told how many notes a run left behind, so the channel the run
// answered can say so under the answer.
type Noter interface {
	Noted(ctx context.Context, run domain.Run, count int) error
}

// notesRecorder puts what was noted in the session's log, where a console
// watching the session sees it. A channel only hears the count, under the
// answer; somebody at this machine sees the notes themselves.
type notesRecorder interface {
	MemoriesNoted(ctx context.Context, run domain.Run, notes []string) error
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
	recorded notesRecorder,
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

		notes := make([]string, 0, len(written))
		for _, memory := range written {
			notes = append(notes, memory.Text)
		}
		if err := recorded.MemoriesNoted(ctx, run, notes); err != nil {
			curator.Logger().Warn("could not record what was noted",
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

// buildEmbedder is the endpoint memories are embedded through, or nil when no
// model is named.
func buildEmbedder(cfg config.Embedding) (memorytool.Embedder, error) {
	if cfg.Model == "" {
		return nil, nil
	}
	key, err := optionalKey(cfg.APIKeyEnv, cfg.APIKeyFile)
	if err != nil {
		return nil, err
	}
	embedder, err := openaicompat.NewEmbedder(openaicompat.Config{
		BaseURL: cfg.BaseURL,
		APIKey:  key,
		Name:    "embedding",
	}, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("memory.embedding: %w", err)
	}
	return embedder, nil
}

// embedMissingMemories catches the store up with the embedder once, at start.
// A failure is a line in the log: what is not embedded is still found by its
// words, and the next start tries again.
func embedMissingMemories(ctx context.Context, options memorytool.Options, logger *slog.Logger) {
	if options.Embedder == nil {
		return
	}
	done, err := options.EmbedMissing(ctx)
	if err != nil {
		logger.Warn("could not embed every memory; the rest are found by their words",
			"model", options.Embedder.Model(), "embedded", done, "error", err)
		return
	}
	if done > 0 {
		logger.Info("embedded memories", "model", options.Embedder.Model(), "count", done)
	}
}
