package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/KoukeNeko/JingClaw/core/internal/adapter/discord"
	"github.com/KoukeNeko/JingClaw/core/internal/adapter/telegram"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/secret"
)

// adapter is everything this process needs a platform to do.
//
// Two methods, because two is all a gateway is: bring messages in, put
// dispatches out. Everything else a platform offers — threads, reactions,
// edits, uploads — is that platform's way of doing one of those, and belongs
// on the far side of this line.
//
// Written as an interface only once there was a second implementation. Before
// that it would have been a guess about what varies, and the guess would have
// been wrong: what Telegram actually changed was not the shape of Post but
// where the message rendering lived.
type adapter interface {
	// Run serves until the context ends.
	Run(ctx context.Context) error

	// Post delivers one dispatch and returns the ids the platform gave the
	// messages, which the outbox records.
	Post(ctx context.Context, dispatch gateway.Dispatch) ([]string, error)
}

// sink is where an adapter hands a message it has accepted.
type sink interface {
	Deliver(ctx context.Context, message gateway.InboundMessage) error
	Withdraw(ctx context.Context, withdrawal gateway.Withdrawal) error
}

// decider is where an adapter hands a press on one of its controls.
//
// A separate interface from sink because they are separate powers: one starts
// work, the other permits it. An adapter given the first and not the second
// cannot decide anything, whatever anybody presses.
type decider interface {
	Decide(ctx context.Context, decision gateway.ApprovalDecision) (gateway.DecisionOutcome, error)
}

// newAdapter builds the platform adapter.
//
// A platform that is not served, and a credential that cannot be found, are
// failures of the settings rather than of this process: they come back as
// notConfigured, which is what lets whatever started the gateway leave it off
// without treating the whole deployment as broken.
//
// The relay is passed twice under two names on purpose: one is the way work
// gets started, the other the way a decision gets recorded. They are different
// powers, and an adapter is handed each of them explicitly.
// unbound is whether no room is bound on the selected platform.
func unbound(cfg config.Config) bool {
	bound := cfg.Gateway.Selected()
	return len(bound.Channels) == 0 && len(bound.Consoles) == 0
}

func newAdapter(cfg config.Config, into sink, decisions decider, logger *slog.Logger) (adapter, error) {
	switch cfg.Gateway.Platform {
	case "discord":
		token, err := secret.Find(cfg.Gateway.Discord.TokenEnv, cfg.Gateway.Discord.TokenFile, "gateway.discord.token_file")
		if err != nil {
			return nil, &notConfigured{err: fmt.Errorf("the discord bot token: %w", err), idle: unbound(cfg)}
		}
		return discord.New(discord.Config{
			Token:              token.Reveal(),
			AccountID:          cfg.Gateway.Selected().AccountID,
			MaxMessages:        cfg.Gateway.Discord.MaxMessages,
			MaxAttachmentBytes: cfg.Gateway.Discord.MaxAttachmentBytes,
			TablesAsImages:     cfg.Gateway.Discord.TablesAsImages,
			Logger:             logger,
		}, into, decisions), nil

	case "telegram":
		token, err := secret.Find(cfg.Gateway.Telegram.TokenEnv, cfg.Gateway.Telegram.TokenFile, "gateway.telegram.token_file")
		if err != nil {
			return nil, &notConfigured{err: fmt.Errorf("the telegram bot token: %w", err), idle: unbound(cfg)}
		}
		return telegram.New(telegram.Config{
			Token:          token.Reveal(),
			AccountID:      cfg.Gateway.Selected().AccountID,
			APIBase:        cfg.Gateway.Telegram.APIBase,
			MaxUploadBytes: cfg.Gateway.Telegram.MaxUploadBytes,
			Logger:         logger,
		}, into), nil

	default:
		// The daemon refused an unknown platform in its settings, so reaching
		// here means --platform named one, or one was added to the settings
		// and not to this switch — which is worth saying plainly rather than
		// starting a process that serves nothing.
		return nil, &notConfigured{err: fmt.Errorf(
			"no adapter for platform %q; the platforms are %s and %s (gateway.platform, or --platform)",
			cfg.Gateway.Platform, config.PlatformDiscord, config.PlatformTelegram)}
	}
}
