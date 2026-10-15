package gateway

import (
	"log/slog"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/config"
)

// A missing bot token is "chat is off" for everybody, but only somebody who
// bound a room to the chat is told to restart over it.
func TestAMissingTokenIsIdleOnlyWhenNoRoomIsBound(t *testing.T) {
	cfg := config.Defaults()
	cfg.Gateway.Platform = config.PlatformDiscord
	cfg.Gateway.Discord.TokenEnv = []string{"JINGCLAW_TEST_NO_SUCH_DISCORD_TOKEN"}
	cfg.Gateway.Discord.TokenFile = "no-such-discord.token"
	t.Chdir(t.TempDir())

	_, err := newAdapter(cfg, nil, nil, slog.Default())
	if !IsUnconfigured(err) || !IsIdle(err) {
		t.Fatalf("nothing bound: unconfigured %v, idle %v (%v)", IsUnconfigured(err), IsIdle(err), err)
	}

	cfg.Gateway.Discord.Channels = []config.Channel{{ChannelIDs: []string{"111"}}}
	_, err = newAdapter(cfg, nil, nil, slog.Default())
	if !IsUnconfigured(err) || IsIdle(err) {
		t.Fatalf("a room bound: unconfigured %v, idle %v (%v)", IsUnconfigured(err), IsIdle(err), err)
	}
}
