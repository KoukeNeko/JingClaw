package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/id"
)

// applyChannels puts the channels the configuration declares into effect.
//
// Applied at startup so a deployment is described in the file rather than in
// commands somebody has to remember running, and idempotent because the store
// keys a binding by its channel rather than by the id given here.
//
// Nothing is removed. A binding decides who may reach the agent, and a daemon
// started once with an incomplete file would take channels away silently;
// leaving one behind is the lesser failure, and the drift is reported instead.
func applyChannels(
	ctx context.Context,
	store gateway.Store,
	cfg config.Config,
	logger *slog.Logger,
) error {
	platform := gateway.Platform(cfg.Gateway.Platform)

	// Only the section belonging to the platform this daemon serves. A file
	// may describe both; binding the rooms of the other one would give a
	// Discord channel id to a Telegram deployment.
	bound := cfg.Gateway.Selected()

	// Which list an entry is in decides what it may do, so the profile is not
	// something the file can misspell.
	lists := []struct {
		key      string
		profile  string
		channels []config.Channel
	}{
		{"gateway." + cfg.Gateway.Platform + ".channels", "gateway", bound.Channels},
		{"gateway." + cfg.Gateway.Platform + ".consoles", "console", bound.Consoles},
	}

	declared := map[string]bool{}
	for _, list := range lists {
		for _, channel := range list.channels {
			for _, channelID := range channel.ChannelIDs {
				binding := gateway.Binding{
					ID:                id.WithPrefix("bnd"),
					Platform:          platform,
					AccountID:         bound.AccountID,
					TenantID:          channel.TenantID,
					ChannelID:         channelID,
					WorkspaceID:       channel.WorkspaceID,
					PermissionProfile: list.profile,
					AllowedPrincipals: channel.Users,
					AllowedClaims:     claimsFor(platform, channel.Roles),

					// Empty stays empty. Nobody gains the power to permit
					// something by being allowed to ask for it.
					ApprovingPrincipals: channel.Approvers,
					ApprovingClaims:     claimsFor(platform, channel.ApproverRoles),
					CreatedAt:           time.Now(),
				}

				if err := store.UpsertBinding(ctx, binding); err != nil {
					return fmt.Errorf("applying %s for %s: %w", list.key, channelID, err)
				}
				declared[channelID] = true

				// Who may speak here, said outright. A binding naming
				// nobody accepts whoever the platform lets into the room,
				// which is a reasonable thing to mean and a bad thing to
				// discover from who turns up.
				who := fmt.Sprintf("%d named", len(binding.AllowedPrincipals)+len(binding.AllowedClaims))
				if binding.OpenToTheRoom() {
					who = "anyone the platform lets in"
				}

				logger.Info("channel bound from the configuration",
					"channel_id", channelID,
					"profile", list.profile,
					"workspace_id", channel.WorkspaceID,
					"who_may_speak", who,
					"who_may_approve", len(binding.ApprovingPrincipals)+len(binding.ApprovingClaims),
				)
			}
		}
	}

	return reportUndeclared(ctx, store, declared, len(declared) > 0, logger)
}

// reportUndeclared names bindings the file does not, so an operator reading
// the configuration is not reading a partial list of who can reach the agent.
func reportUndeclared(
	ctx context.Context,
	store gateway.Store,
	declared map[string]bool,
	fileDeclaresAny bool,
	logger *slog.Logger,
) error {
	if !fileDeclaresAny {
		// A file that declares nothing is not claiming to be the whole list,
		// so there is no drift to report.
		return nil
	}

	existing, err := store.ListBindings(ctx)
	if err != nil {
		return err
	}

	for _, binding := range existing {
		if declared[binding.ChannelID] {
			continue
		}
		logger.Warn("a bound channel is not in the configuration",
			"channel_id", binding.ChannelID,
			"profile", binding.PermissionProfile,
			"binding_id", binding.ID,
			"hint", "it stays bound; remove it with: agent bindings remove",
		)
	}

	return nil
}

// claimsFor turns role ids into the namespaced claims a binding matches on.
func claimsFor(platform gateway.Platform, roles []string) []gateway.Claim {
	if len(roles) == 0 {
		return nil
	}

	claims := make([]gateway.Claim, 0, len(roles))
	for _, role := range roles {
		claims = append(claims, gateway.Claim{
			Namespace: string(platform) + ".role",
			Value:     role,
		})
	}
	return claims
}
