package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
}

// newAdapter builds the platform named in the configuration.
func newAdapter(cfg config.Config, into sink, logger *slog.Logger) (adapter, error) {
	switch cfg.Gateway.Platform {
	case "discord":
		token, err := loadToken(cfg.Gateway.Discord.TokenEnv, cfg.Gateway.Discord.TokenFile)
		if err != nil {
			return nil, err
		}
		return discord.New(discord.Config{
			Token:              token.Reveal(),
			AccountID:          cfg.Gateway.AccountID,
			MaxMessages:        cfg.Gateway.Discord.MaxMessages,
			MaxAttachmentBytes: cfg.Gateway.Discord.MaxAttachmentBytes,
			Logger:             logger,
		}, into), nil

	case "telegram":
		token, err := loadToken(cfg.Gateway.Telegram.TokenEnv, cfg.Gateway.Telegram.TokenFile)
		if err != nil {
			return nil, err
		}
		return telegram.New(telegram.Config{
			Token:          token.Reveal(),
			AccountID:      cfg.Gateway.AccountID,
			APIBase:        cfg.Gateway.Telegram.APIBase,
			MaxUploadBytes: cfg.Gateway.Telegram.MaxUploadBytes,
			Logger:         logger,
		}, into), nil

	default:
		// Validation has already refused an unknown platform, so reaching
		// here means one was added to the configuration and not to this
		// switch — which is worth saying plainly rather than starting a
		// process that serves nothing.
		return nil, fmt.Errorf("gatewayd: no adapter for platform %q", cfg.Gateway.Platform)
	}
}

// loadToken finds a bot credential without it passing through the shell
// history or a command line.
func loadToken(envVars []string, tokenFile string) (secret.Value, error) {
	files, err := secret.DefaultFiles(tokenFile)
	if err != nil {
		return secret.Value{}, err
	}

	token, err := secret.Load(secret.LoadOptions{EnvVars: envVars, Files: files})
	if err != nil {
		return secret.Value{}, err
	}
	if !token.IsSet() {
		return secret.Value{}, fmt.Errorf(
			"no bot token: set %s, or write it with mode 600 to one of: %v",
			strings.Join(envVars, " or "), files)
	}

	return token, nil
}
