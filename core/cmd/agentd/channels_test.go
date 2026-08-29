package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/gateway"
	"github.com/KoukeNeko/JingClaw/core/internal/storage/sqlite"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "channels.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func configWith(channels ...config.Channel) config.Config {
	cfg := config.Defaults()
	cfg.Gateway.Platform = "discord"
	cfg.Gateway.Discord.AccountID = "main"
	cfg.Gateway.Discord.Channels = channels
	return cfg
}

func consolesWith(channels ...config.Channel) config.Config {
	cfg := configWith()
	cfg.Gateway.Discord.Consoles = channels
	return cfg
}

// A channel in the file becomes a binding, with the profile the file gave it.
func TestAChannelInTheFileBecomesABinding(t *testing.T) {
	store := openStore(t)

	cfg := consolesWith(config.Channel{
		ChannelIDs:  []string{"channel_1"},
		TenantID:    "guild_1",
		WorkspaceID: "ws",
		Users:       []string{"user_1"},
		Roles:       []string{"role_1"},
	})

	if err := applyChannels(context.Background(), store, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	binding, err := store.Binding(context.Background(), gateway.PlatformDiscord, "main", "guild_1", "channel_1")
	if err != nil {
		t.Fatalf("the channel was not bound: %v", err)
	}
	if binding.PermissionProfile != "console" {
		t.Errorf("profile is %q", binding.PermissionProfile)
	}
	if binding.WorkspaceID != "ws" {
		t.Errorf("workspace is %q", binding.WorkspaceID)
	}
	if len(binding.AllowedPrincipals) != 1 || binding.AllowedPrincipals[0] != "user_1" {
		t.Errorf("allowed principals are %v", binding.AllowedPrincipals)
	}
	// A role becomes the namespaced claim a binding matches on, rather than
	// being confused with an account id.
	if len(binding.AllowedClaims) != 1 || binding.AllowedClaims[0].Namespace != "discord.role" {
		t.Errorf("allowed claims are %+v", binding.AllowedClaims)
	}
}

// Starting twice must not produce two bindings for one channel, and must
// carry an edit through.
func TestApplyingTwiceUpdatesRatherThanDuplicates(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	quiet := slog.New(slog.DiscardHandler)

	channel := config.Channel{
		ChannelIDs: []string{"channel_1"}, TenantID: "guild_1", Users: []string{"user_1"},
	}

	if err := applyChannels(ctx, store, configWith(channel), quiet); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The operator moves it to the console list and adds somebody.
	channel.Users = []string{"user_1", "user_2"}
	if err := applyChannels(ctx, store, consolesWith(channel), quiet); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	bindings, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("one channel produced %d bindings", len(bindings))
	}
	if bindings[0].PermissionProfile != "console" {
		t.Errorf("the edit did not take: profile is %q", bindings[0].PermissionProfile)
	}
	if len(bindings[0].AllowedPrincipals) != 2 {
		t.Errorf("the edit did not take: principals are %v", bindings[0].AllowedPrincipals)
	}
}

// A channel dropped from the file stays bound. A daemon started once with an
// incomplete file would otherwise take away the thing that decides who can
// reach the agent.
func TestAChannelRemovedFromTheFileStaysBound(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()
	quiet := slog.New(slog.DiscardHandler)

	both := configWith(config.Channel{
		ChannelIDs: []string{"channel_1", "channel_2"},
		TenantID:   "guild_1",
		Users:      []string{"user_1"},
	})
	if err := applyChannels(ctx, store, both, quiet); err != nil {
		t.Fatalf("apply: %v", err)
	}

	one := configWith(config.Channel{
		ChannelIDs: []string{"channel_1"}, TenantID: "guild_1", Users: []string{"user_1"},
	})
	if err := applyChannels(ctx, store, one, quiet); err != nil {
		t.Fatalf("apply: %v", err)
	}

	bindings, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("a channel dropped from the file was unbound: %d remain", len(bindings))
	}
}

// A file that declares nothing is not claiming to be the whole list, so a
// deployment bound entirely by hand is not nagged about every channel.
func TestAFileThatDeclaresNothingIsNotAClaim(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	byHand := gateway.Binding{
		ID: "bnd_1", Platform: gateway.PlatformDiscord, AccountID: "main",
		TenantID: "guild_1", ChannelID: "channel_1", PermissionProfile: "gateway",
	}
	if err := store.UpsertBinding(ctx, byHand); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := applyChannels(ctx, store, configWith(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	bindings, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 1 {
		t.Errorf("a hand-made binding was disturbed: %d remain", len(bindings))
	}
}

// Several channels in one entry share its rules, which is the point of writing
// them together.
func TestOneEntryBindsEveryChannelInIt(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	cfg := consolesWith(config.Channel{
		ChannelIDs:  []string{"a", "b", "c"},
		TenantID:    "guild_1",
		WorkspaceID: "ws",
		Users:       []string{"user_1"},
	})
	if err := applyChannels(ctx, store, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	bindings, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 3 {
		t.Fatalf("three channels produced %d bindings", len(bindings))
	}
	for _, binding := range bindings {
		if binding.PermissionProfile != "console" {
			t.Errorf("%s is %q, want console", binding.ChannelID, binding.PermissionProfile)
		}
		if binding.WorkspaceID != "ws" {
			t.Errorf("%s has workspace %q", binding.ChannelID, binding.WorkspaceID)
		}
	}
}

// The list an entry is in is what sets the profile, and nothing else can.
func TestTheListDecidesTheProfile(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	cfg := configWith(config.Channel{
		ChannelIDs: []string{"ordinary"}, Users: []string{"user_1"},
	})
	cfg.Gateway.Discord.Consoles = []config.Channel{{
		ChannelIDs: []string{"private"}, Users: []string{"user_1"},
	}}

	if err := applyChannels(ctx, store, cfg, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	bindings, err := store.ListBindings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	byChannel := map[string]string{}
	for _, binding := range bindings {
		byChannel[binding.ChannelID] = binding.PermissionProfile
	}
	if byChannel["ordinary"] != "gateway" {
		t.Errorf("an ordinary channel became %q", byChannel["ordinary"])
	}
	if byChannel["private"] != "console" {
		t.Errorf("a console became %q", byChannel["private"])
	}
}
