package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"

	controlv1 "github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1"
	"github.com/KoukeNeko/JingClaw/core/gen/go/jingclaw/control/v1/controlv1connect"
	"github.com/KoukeNeko/JingClaw/core/internal/config"
	"github.com/KoukeNeko/JingClaw/core/internal/control"
	"github.com/KoukeNeko/JingClaw/core/internal/discovery"
	"github.com/KoukeNeko/JingClaw/core/internal/home"
)

// running is a daemon's running configuration with something in every place the
// gateway reads, and with the things it must not be told filled in as well so
// that being told them would show.
func running() config.Config {
	cfg := config.Defaults()
	cfg.Server.LogLevel = "debug"

	cfg.Gateway.Platform = config.PlatformDiscord
	cfg.Gateway.Discord = config.Discord{
		AccountID:          "main-discord",
		TokenEnv:           []string{"JINGCLAW_TEST_NO_SUCH_DISCORD_TOKEN", "JINGCLAW_TEST_NO_SUCH_OTHER_TOKEN"},
		TokenFile:          "no-such-discord.token",
		MaxMessages:        7,
		MaxAttachmentBytes: 1 << 20,
		TablesAsImages:     true,
		Channels:           []config.Channel{{ChannelIDs: []string{"channel-111"}, Users: []string{"somebody"}}},
		Consoles:           []config.Channel{{ChannelIDs: []string{"console-222"}}},
	}
	cfg.Gateway.Telegram = config.Telegram{
		AccountID:      "main-telegram",
		TokenEnv:       []string{"JINGCLAW_TEST_NO_SUCH_TELEGRAM_TOKEN"},
		TokenFile:      "no-such-telegram.token",
		MaxUploadBytes: 2 << 20,
		APIBase:        "http://localhost:8081",
		Channels:       []config.Channel{{ChannelIDs: []string{"chat-333"}}},
	}
	return cfg
}

// A daemon the gateway can dial: the ingress, answering from the given running
// configuration, with the discovery file that says where it is. The environment
// is set the way a gateway started by hand would have it.
func aDaemon(t *testing.T, running config.Config, mode string) {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(controlv1connect.NewGatewayIngressServiceHandler(control.NewGatewayServer(nil, nil, running, nil)))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	publish(t, discovery.File{BaseURL: server.URL, Token: "control", GatewayToken: "gateway", Mode: mode})
}

// publish writes the discovery file where a gateway will look for it, and
// points the environment there the way the settings cannot: they are in the
// daemon, which this process has not reached yet.
func publish(t *testing.T, file discovery.File) {
	t.Helper()

	dir := t.TempDir()
	file.PID = os.Getpid()
	file.ProtocolVersion = discovery.ProtocolVersion
	if err := discovery.Write(filepath.Join(dir, "daemon.json"), file); err != nil {
		t.Fatalf("publish: %v", err)
	}

	t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", dir)
	t.Setenv("JINGCLAW_RUNTIME_DIR", "")
	t.Setenv(home.EnvVar, t.TempDir())
}

// What the daemon sends is what the gateway reads, field for field.
//
// Asked of the real server rather than of a message written by hand, so that a
// field added to what the daemon sends and not to what is read here is a
// failure. The channels and the users who may speak in them are in the running
// configuration and are not among the things the gateway is told, so they come
// back empty.
func TestWhatTheDaemonSendsIsTheConfigurationTheGatewayReads(t *testing.T) {
	sent := running()
	aDaemon(t, sent, "")

	client, err := dialAgent(os.Getenv("JINGCLAW_SERVER_RUNTIME_DIR"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	told, err := client.GetGatewaySettings(context.Background(),
		connect.NewRequest(&controlv1.GetGatewaySettingsRequest{}))
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	got := settingsFromProto(told.Msg)

	want := sent.Gateway
	want.WorkingInterval, want.StreamInterval = 0, 0
	want.Discord.Channels, want.Discord.Consoles = nil, nil
	want.Telegram.Channels, want.Telegram.Consoles = nil, nil
	if !reflect.DeepEqual(got.Gateway, want) {
		t.Errorf("the gateway reads\n  %+v\nthe daemon runs\n  %+v", got.Gateway, want)
	}

	if got.LogLevel() != sent.LogLevel() {
		t.Errorf("the gateway logs at %v, the daemon at %v", got.LogLevel(), sent.LogLevel())
	}
	if got.Gateway.Selected().AccountID != "main-discord" {
		t.Errorf("the account it serves is %q", got.Gateway.Selected().AccountID)
	}
	if bound := got.Gateway.Selected(); len(bound.Channels) != 0 || len(bound.Consoles) != 0 {
		t.Errorf("the gateway knows which channels are bound: %+v", bound)
	}
}

// Nothing sent is not a configuration with something in it: the fields that
// were not sent read as unset, not as the defaults.
func TestASettingTheDaemonDidNotSendIsNotADefault(t *testing.T) {
	cfg := settingsFromProto(&controlv1.GetGatewaySettingsResponse{Platform: "telegram"})

	if cfg.Gateway.Platform != "telegram" {
		t.Errorf("the platform is %q", cfg.Gateway.Platform)
	}
	if cfg.Gateway.Telegram.MaxUploadBytes != 0 || cfg.Gateway.Discord.MaxMessages != 0 {
		t.Errorf("a limit nobody sent has a value: %+v", cfg.Gateway)
	}
	if cfg.Server.RuntimeDir != "" || cfg.Provider.Backend != "" {
		t.Errorf("a setting outside the gateway's was filled in: %+v", cfg)
	}
}

// A gateway with no token is not a fault in the gateway: it is a setting to put
// right, and it says which.
func TestAGatewayWithNoTokenIsNotConfigured(t *testing.T) {
	aDaemon(t, running(), "")

	err := Run(context.Background(), nil)

	var unusable *notConfigured
	if !errors.As(err, &unusable) {
		t.Fatalf("a missing token came back as an ordinary failure: %v", err)
	}
	for _, want := range []string{"discord", "JINGCLAW_TEST_NO_SUCH_DISCORD_TOKEN", "gateway.discord.token_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("it does not say %q:\n%v", want, err)
		}
	}
}

// --platform picks which of the daemon's two sections is read, so a gateway
// started for Telegram on a daemon set up for Discord asks for Telegram's token.
func TestThePlatformFlagChoosesWhichSectionIsRead(t *testing.T) {
	aDaemon(t, running(), "")

	err := Run(context.Background(), []string{"--platform", "telegram", "--account", "elsewhere"})

	var unusable *notConfigured
	if !errors.As(err, &unusable) {
		t.Fatalf("a missing token came back as an ordinary failure: %v", err)
	}
	if !strings.Contains(err.Error(), "telegram") ||
		!strings.Contains(err.Error(), "JINGCLAW_TEST_NO_SUCH_TELEGRAM_TOKEN") {
		t.Errorf("it did not ask for telegram's token:\n%v", err)
	}
	if strings.Contains(err.Error(), "DISCORD") {
		t.Errorf("it asked for discord's token as well:\n%v", err)
	}
}

// A platform nobody serves is the operator's to correct, and says what is
// served.
func TestAPlatformNobodyServesIsNotConfigured(t *testing.T) {
	aDaemon(t, running(), "")

	err := Run(context.Background(), []string{"--platform", "slack"})

	var unusable *notConfigured
	if !errors.As(err, &unusable) {
		t.Fatalf("an unknown platform came back as an ordinary failure: %v", err)
	}
	if !strings.Contains(err.Error(), `"slack"`) || !strings.Contains(err.Error(), "discord") ||
		!strings.Contains(err.Error(), "telegram") {
		t.Errorf("it does not say what is wrong and what is served:\n%v", err)
	}
}

// A daemon that is up and is running no agent has nothing for a gateway to
// reach, and the fix is to repair it rather than to start a gateway again.
func TestADaemonWithNoAgentIsNotConfigured(t *testing.T) {
	publish(t, discovery.File{BaseURL: "http://127.0.0.1:1", Token: "control", Mode: discovery.ModeRepair})

	err := Run(context.Background(), nil)

	var unusable *notConfigured
	if !errors.As(err, &unusable) {
		t.Fatalf("a daemon with no agent came back as an ordinary failure: %v", err)
	}
	if !strings.Contains(err.Error(), "jingclaw dashboard") {
		t.Errorf("it does not say where to go:\n%v", err)
	}
}

// Everything else stays what it was: a failure, not a setting.
//
// The supervisor ends its run over those, and it must go on ending it over them.
// Marking a daemon that is not running as a configuration problem would make a
// crashed daemon look like a missing token.
func TestAFailureThatIsNotTheSettingsIsNotConfiguration(t *testing.T) {
	t.Run("no daemon", func(t *testing.T) {
		t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", t.TempDir())
		t.Setenv("JINGCLAW_RUNTIME_DIR", "")

		err := Run(context.Background(), nil)

		var unusable *notConfigured
		if err == nil || errors.As(err, &unusable) {
			t.Fatalf("a daemon that is not running gave %v", err)
		}
		if !strings.Contains(err.Error(), "is jingclaw running?") {
			t.Errorf("it does not say to look at the daemon: %v", err)
		}
	})

	t.Run("a daemon that cannot answer", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.Handle(controlv1connect.NewGatewayIngressServiceHandler(
			controlv1connect.UnimplementedGatewayIngressServiceHandler{}))
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		publish(t, discovery.File{BaseURL: server.URL, Token: "control", GatewayToken: "gateway"})

		err := Run(context.Background(), nil)

		var unusable *notConfigured
		if err == nil || errors.As(err, &unusable) {
			t.Fatalf("a daemon that would not answer gave %v", err)
		}
		if !strings.Contains(err.Error(), "ask the daemon for the gateway settings") {
			t.Errorf("it does not say what failed: %v", err)
		}
	})

	t.Run("an environment that cannot be read", func(t *testing.T) {
		// The one failure before the daemon is reached, and it is the
		// operator's variable that is wrong.
		t.Setenv("JINGCLAW_AGENT_MAX_ITERATIONS", "not-a-number")

		var unusable *notConfigured
		if err := Run(context.Background(), nil); !errors.As(err, &unusable) {
			t.Fatalf("an unreadable environment came back as %v", err)
		}
	})
}

// There is no file to point the gateway at any more, and saying so is a refusal
// of the flag rather than a quiet ignoring of it: a gateway started with the
// old flag would otherwise run on settings its operator believes it is not.
func TestTheConfigFlagIsGone(t *testing.T) {
	err := Run(context.Background(), []string{"--config", "/etc/jingclaw/config.toml"})

	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Errorf("--config was accepted: %v", err)
	}
}

// The status is what the supervisor reads, so it is checked at the process
// level: a Main that returned an error and let the program's own main exit with
// 1 would pass every test above.
func TestTheProcessExitsWithTheConfigurationStatusOnlyForConfiguration(t *testing.T) {
	if os.Getenv("JINGCLAW_TEST_RUN_MAIN") != "" {
		if err := Main(nil); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	exitOf := func(t *testing.T) (int, string) {
		t.Helper()

		command := exec.Command(os.Args[0], "-test.run=^TestTheProcessExitsWithTheConfigurationStatusOnlyForConfiguration$")
		command.Env = append(os.Environ(), "JINGCLAW_TEST_RUN_MAIN=1")
		out, err := command.CombinedOutput()

		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the process did not exit with a status: %v\n%s", err, out)
		}
		return exit.ExitCode(), string(out)
	}

	t.Run("a missing token", func(t *testing.T) {
		aDaemon(t, running(), "")

		status, out := exitOf(t)

		if status != ExitConfiguration {
			t.Errorf("exited with %d, want %d:\n%s", status, ExitConfiguration, out)
		}
		if !strings.Contains(out, "error: ") || !strings.Contains(out, "JINGCLAW_TEST_NO_SUCH_DISCORD_TOKEN") {
			t.Errorf("it did not say what to put right:\n%s", out)
		}
	})

	t.Run("no daemon", func(t *testing.T) {
		t.Setenv("JINGCLAW_SERVER_RUNTIME_DIR", t.TempDir())
		t.Setenv("JINGCLAW_RUNTIME_DIR", "")

		status, out := exitOf(t)

		if status != 1 {
			t.Errorf("exited with %d, want 1:\n%s", status, out)
		}
	})
}

// A panic is an error that names what was panicked with by its type and not by
// its value, because the value can be a request's URL and the URL holds the
// token.
func TestAPanicIsContainedWithoutItsMessage(t *testing.T) {
	err := Contain(func() error {
		panic(fmt.Errorf("Post https://api.telegram.org/botSECRET-123/getUpdates: refused"))
	})
	if err == nil {
		t.Fatal("a panic was not turned into an error")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("the error does not say what happened: %.200s", err)
	}
	if strings.Contains(err.Error(), "SECRET-123") {
		t.Error("the panic's message was kept")
	}

	if err := Contain(func() error { return nil }); err != nil {
		t.Errorf("a function that did not panic returned %v", err)
	}
}
