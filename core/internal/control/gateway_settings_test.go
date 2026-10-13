package control_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
)

// secretLooking is a running configuration where every place a secret could be
// is filled in, so that what a gateway is told can be checked for it.
func secretLooking() config.Config {
	cfg := config.Defaults()
	cfg.Server.LogLevel = "debug"

	cfg.Gateway.Platform = config.PlatformTelegram
	cfg.Gateway.Discord = config.Discord{
		AccountID:          "main-discord",
		TokenEnv:           []string{"MY_DISCORD_TOKEN", "OTHER_DISCORD_TOKEN"},
		TokenFile:          "my-discord.token",
		MaxMessages:        7,
		MaxAttachmentBytes: 1 << 20,
		TablesAsImages:     true,
		Channels: []config.Channel{{
			ChannelIDs: []string{"channel-id-111"}, TenantID: "tenant-id-222",
			Users: []string{"user-who-may-speak"}, Roles: []string{"role-who-may-speak"},
			Approvers: []string{"user-who-may-approve"}, ApproverRoles: []string{"role-who-may-approve"},
		}},
		Consoles: []config.Channel{{ChannelIDs: []string{"console-id-333"}}},
	}
	cfg.Gateway.Telegram = config.Telegram{
		AccountID:      "main-telegram",
		TokenEnv:       []string{"MY_TELEGRAM_TOKEN"},
		TokenFile:      "my-telegram.token",
		MaxUploadBytes: 2 << 20,
		APIBase:        "http://localhost:8081",
		Channels: []config.Channel{{
			ChannelIDs: []string{"telegram-chat-444"}, Approvers: []string{"telegram-approver"},
		}},
	}

	cfg.Provider.Gemini.APIKeyEnv = []string{"GEMINI_KEY_VARIABLE"}
	cfg.Provider.Gemini.APIKeyFile = "gemini-key-file.key"
	cfg.Provider.Anthropic.BaseURL = "https://anthropic.example.test"
	cfg.MCP.Servers = []config.MCPServer{{
		Name: "files", URL: "https://mcp.example.test",
		Headers: map[string]string{"authorization": "Bearer sk-live-MCP-SECRET"},
		Env:     map[string]string{"TOKEN": "env-secret-value"},
		PassEnv: []string{"HOME_SECRET_VARIABLE"},
	}}
	cfg.Web.Search.KeyEnv = []string{"SEARCH_KEY_VARIABLE"}
	cfg.Server.Addr = "127.0.0.1:4242"
	cfg.Server.DataDir = "/srv/jingclaw/data"
	return cfg
}

func newGatewayClient(t *testing.T, running config.Config) controlv1connect.GatewayIngressServiceClient {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewGatewayIngressServiceHandler(control.NewGatewayServer(nil, nil, running, nil)))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return controlv1connect.NewGatewayIngressServiceClient(server.Client(), server.URL)
}

// The gateway learns how to reach its platform and how to shape what it sends,
// from what the daemon is running.
func TestAGatewayIsToldItsPlatformSettingsFromTheRunningConfiguration(t *testing.T) {
	client := newGatewayClient(t, secretLooking())

	resp, err := client.GetGatewaySettings(context.Background(), connect.NewRequest(&controlv1.GetGatewaySettingsRequest{}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := resp.Msg

	if got.GetPlatform() != "telegram" || got.GetLogLevel() != "debug" {
		t.Errorf("platform %q, log level %q", got.GetPlatform(), got.GetLogLevel())
	}

	discord := got.GetDiscord()
	if discord.GetAccountId() != "main-discord" || discord.GetTokenFile() != "my-discord.token" ||
		!slices.Equal(discord.GetTokenEnv(), []string{"MY_DISCORD_TOKEN", "OTHER_DISCORD_TOKEN"}) ||
		discord.GetMaxMessages() != 7 || discord.GetMaxAttachmentBytes() != 1<<20 || !discord.GetTablesAsImages() {
		t.Errorf("discord: %v", discord)
	}

	telegram := got.GetTelegram()
	if telegram.GetAccountId() != "main-telegram" || telegram.GetTokenFile() != "my-telegram.token" ||
		!slices.Equal(telegram.GetTokenEnv(), []string{"MY_TELEGRAM_TOKEN"}) ||
		telegram.GetMaxUploadBytes() != 2<<20 || telegram.GetApiBase() != "http://localhost:8081" {
		t.Errorf("telegram: %v", telegram)
	}
}

// A process that holds somebody else's bot token is told the names of the
// variables and the file the token is read from, and nothing else that could
// carry a secret or a decision: not who may speak or approve, not which
// channels are bound, not the rest of the configuration.
func TestAGatewayIsToldNothingSecretBearingBeyondWhereItsTokenIs(t *testing.T) {
	client := newGatewayClient(t, secretLooking())

	resp, err := client.GetGatewaySettings(context.Background(), connect.NewRequest(&controlv1.GetGatewaySettingsRequest{}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	sent, err := protojson.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, shouldNot := range []string{
		"user-who-may-speak", "role-who-may-speak", "user-who-may-approve", "role-who-may-approve",
		"channel-id-111", "tenant-id-222", "console-id-333", "telegram-chat-444", "telegram-approver",
		"GEMINI_KEY_VARIABLE", "gemini-key-file.key", "anthropic.example.test",
		"sk-live-MCP-SECRET", "env-secret-value", "HOME_SECRET_VARIABLE", "mcp.example.test",
		"SEARCH_KEY_VARIABLE", "4242", "/srv/jingclaw/data",
	} {
		if strings.Contains(string(sent), shouldNot) {
			t.Errorf("a gateway was told %q:\n%s", shouldNot, sent)
		}
	}
	for _, should := range []string{"MY_DISCORD_TOKEN", "my-discord.token", "MY_TELEGRAM_TOKEN", "my-telegram.token"} {
		if !strings.Contains(string(sent), should) {
			t.Errorf("a gateway was not told %q:\n%s", should, sent)
		}
	}
}

// Whatever is put in the reply later is something somebody decided a gateway
// should have. This is where that decision is made on purpose: a field added to
// the message fails here until it is listed, which is the moment to ask whether
// a process holding a bot token should be handed it.
func TestWhatAGatewayCanBeToldIsAListSomebodyChose(t *testing.T) {
	var fields []string

	var walk func(prefix string, message protoreflect.MessageDescriptor)
	walk = func(prefix string, message protoreflect.MessageDescriptor) {
		for i := range message.Fields().Len() {
			field := message.Fields().Get(i)
			name := prefix + string(field.Name())
			if field.Message() != nil {
				walk(name+".", field.Message())
				continue
			}
			fields = append(fields, name)
		}
	}
	walk("", (&controlv1.GetGatewaySettingsResponse{}).ProtoReflect().Descriptor())
	sort.Strings(fields)

	want := []string{
		"discord.account_id", "discord.max_attachment_bytes", "discord.max_messages",
		"discord.tables_as_images", "discord.token_env", "discord.token_file",
		"log_level", "platform",
		"telegram.account_id", "telegram.api_base", "telegram.max_upload_bytes",
		"telegram.token_env", "telegram.token_file",
	}
	if !slices.Equal(fields, want) {
		t.Errorf("a gateway can be told:\n got %v\nwant %v", fields, want)
	}
}
