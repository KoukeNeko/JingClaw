// Package config loads the daemon's settings.
//
// Precedence runs defaults → file → environment → flags, so the more specific
// and more immediate a source is, the more it wins. Everything is decoded into
// a typed struct at startup rather than being queried by key later: a
// configuration library reaching into every corner of a program is how a typo
// in a key name becomes a silent default at runtime.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const (
	appDir   = "JingClaw"
	fileName = "config.toml"

	// envPrefix keeps JingClaw's variables from colliding with anything else
	// in a shell.
	envPrefix = "JINGCLAW_"
)

// Config is the whole of the daemon's settings.
type Config struct {
	Agent     Agent     `koanf:"agent"`
	Model     Model     `koanf:"model"`
	Tools     Tools     `koanf:"tools"`
	Delivery  Delivery  `koanf:"delivery"`
	Workspace Workspace `koanf:"workspace"`
	Server    Server    `koanf:"server"`
	Gateway   Gateway   `koanf:"gateway"`
}

// Agent is who the agent is and what it has been told.
//
// None of this is a security control. It shapes how the agent presents itself
// and what it tries; what it is permitted to do is settled by the policy
// engine, which does not read any of these fields.
type Agent struct {
	// Name is what the agent calls itself. Empty leaves it unnamed, which is
	// better than insisting on one that contradicts whatever the account is
	// called wherever it is deployed.
	Name string `koanf:"name"`

	// Persona is extra identity: tone, stance, what this deployment is for.
	Persona string `koanf:"persona"`

	// Instructions are the operator's standing directions.
	Instructions string `koanf:"instructions"`

	// InstructionFiles are read from the workspace when present. AGENTS.md is
	// a convention several tools already share; adding a private filename as
	// well is cheap, inventing a new one and ignoring the shared one is not.
	InstructionFiles []string `koanf:"instruction_files"`

	// MaxIterations bounds the tool loop per run.
	MaxIterations int `koanf:"max_iterations"`

	// PermissionProfile decides what local sessions may do without asking.
	// Setting it to "gateway" makes even a local operator's runs refuse to
	// execute programs, which is a reasonable choice on a shared machine.
	PermissionProfile string `koanf:"permission_profile"`
}

type Model struct {
	Provider string `koanf:"provider"`
	Model    string `koanf:"model"`

	// APIKeyEnv and APIKeyFile say where the credential comes from. They are
	// settings because deployments differ: a service manager supplies one
	// through the environment, a workstation through a file.
	APIKeyEnv  []string `koanf:"api_key_env"`
	APIKeyFile string   `koanf:"api_key_file"`

	// FakeDelay paces the offline provider, so streaming and interruption can
	// be watched at human speed.
	FakeDelay time.Duration `koanf:"fake_delay"`

	Retry Retry `koanf:"retry"`
}

// Retry governs resending a failed request.
//
// It is configurable because the right values depend on the account: a paid
// plan with generous limits and a free tier hitting a daily quota want very
// different behaviour, and neither is a property of the code.
type Retry struct {
	MaxAttempts int           `koanf:"max_attempts"`
	BaseDelay   time.Duration `koanf:"base_delay"`
	MaxDelay    time.Duration `koanf:"max_delay"`

	// Jitter spreads retries so many clients recovering from one outage do not
	// resend in lockstep.
	Jitter float64 `koanf:"jitter"`
}

// Tools bounds what the built-in tools will read, search and run.
//
// These are product settings rather than protocol constants: the right ceiling
// depends on the model's context window and on what the workspace contains.
type Tools struct {
	// ReadLimit is how much of a file one read returns.
	ReadLimit int64 `koanf:"read_limit"`

	// MaxReadableFile refuses a file too large to be worth reading at all;
	// the answer for one of those is to search it instead.
	MaxReadableFile int64 `koanf:"max_readable_file"`

	// MaxOverwriteBytes caps replacing a file in full. A model rewriting
	// something large is nearly always the wrong shape of edit.
	MaxOverwriteBytes int64 `koanf:"max_overwrite_bytes"`

	// MaxSearchableFile skips files too large to search, which are generated
	// or minified far more often than they are the answer.
	MaxSearchableFile int64 `koanf:"max_searchable_file"`

	GlobResults int `koanf:"glob_results"`
	GrepResults int `koanf:"grep_results"`

	CommandTimeout    time.Duration `koanf:"command_timeout"`
	MaxCommandTimeout time.Duration `koanf:"max_command_timeout"`
	MaxCommandOutput  int           `koanf:"max_command_output"`
}

// Delivery paces how provider output becomes events.
//
// Providers stream in whatever granularity suits them. These bound the log by
// the clock rather than by the provider's chunk rate, which is what stops one
// talkative model turning every few characters into a database write.
type Delivery struct {
	TextFlushBytes     int           `koanf:"text_flush_bytes"`
	TextFlushInterval  time.Duration `koanf:"text_flush_interval"`
	UsageFlushInterval time.Duration `koanf:"usage_flush_interval"`
}

// Gateway is what a gateway process needs.
type Gateway struct {
	Platform string `koanf:"platform"`

	// AccountID names the bot account within JingClaw, so bindings and the
	// delivery queue can be scoped to it.
	AccountID string `koanf:"account_id"`

	TokenEnv  []string `koanf:"token_env"`
	TokenFile string   `koanf:"token_file"`
}

type Workspace struct {
	Root string `koanf:"root"`
}

type Server struct {
	// Addr must stay on the loopback interface. This API can run programs, and
	// Validate refuses anything else rather than trusting that whoever wrote
	// the line understood what it exposes.
	Addr string `koanf:"addr"`

	// DataDir holds the database. Empty means the platform's own location.
	DataDir string `koanf:"data_dir"`

	// RuntimeDir holds the discovery file clients use to find the daemon.
	RuntimeDir string `koanf:"runtime_dir"`

	LogLevel string `koanf:"log_level"`
}

// Defaults are what runs when nothing says otherwise.
func Defaults() Config {
	return Config{
		Agent: Agent{
			Name:              "JingClaw",
			InstructionFiles:  []string{"AGENTS.md", "JINGCLAW.md"},
			MaxIterations:     12,
			PermissionProfile: "local",
		},
		Model: Model{
			Provider:   "fake",
			APIKeyEnv:  []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			APIKeyFile: "gemini.key",
			FakeDelay:  150 * time.Millisecond,
			Retry: Retry{
				MaxAttempts: 4,
				BaseDelay:   500 * time.Millisecond,
				MaxDelay:    30 * time.Second,
				Jitter:      0.3,
			},
		},
		Tools: Tools{
			ReadLimit:         64 * 1024,
			MaxReadableFile:   8 * 1024 * 1024,
			MaxOverwriteBytes: 128 * 1024,
			MaxSearchableFile: 2 * 1024 * 1024,
			GlobResults:       200,
			GrepResults:       100,
			CommandTimeout:    2 * time.Minute,
			MaxCommandTimeout: 10 * time.Minute,
			MaxCommandOutput:  32 * 1024,
		},
		Delivery: Delivery{
			TextFlushBytes:     240,
			TextFlushInterval:  200 * time.Millisecond,
			UsageFlushInterval: 2 * time.Second,
		},
		Workspace: Workspace{
			Root: ".",
		},
		Server: Server{
			Addr:     "127.0.0.1:0",
			LogLevel: "info",
		},
		Gateway: Gateway{
			Platform:  "discord",
			AccountID: "main",
			TokenEnv:  []string{"DISCORD_BOT_TOKEN"},
			TokenFile: "discord.token",
		},
	}
}

// Load reads the configuration.
//
// A missing file is not an error: running with defaults and no configuration
// at all has to work, or the first experience of the tool is editing TOML. A
// file that exists but cannot be parsed is an error, because silently ignoring
// it would run with settings the operator believes are in force.
func Load(path string) (Config, string, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, "", fmt.Errorf("config: load defaults: %w", err)
	}

	resolved := path
	if resolved == "" {
		var err error
		if resolved, err = DefaultPath(); err != nil {
			return Config{}, "", err
		}
	}

	used := ""
	if err := k.Load(file.Provider(resolved), toml.Parser()); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Config{}, "", fmt.Errorf("config: read %s: %w", resolved, err)
		}
		// An explicitly requested file that is not there is a mistake worth
		// reporting; the default one simply may not exist yet.
		if path != "" {
			return Config{}, "", fmt.Errorf("config: %s does not exist", path)
		}
	} else {
		used = resolved
	}

	// JINGCLAW_AGENT_NAME sets agent.name, and so on.
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix: envPrefix,
		TransformFunc: func(key, value string) (string, any) {
			key = strings.ToLower(strings.TrimPrefix(key, envPrefix))
			return strings.Replace(key, "_", ".", 1), value
		},
	}), nil); err != nil {
		return Config{}, "", fmt.Errorf("config: read environment: %w", err)
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			// Durations are written as "2s" rather than nanoseconds, because a
			// config file is read by people.
			DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
			Result:           &cfg,
			WeaklyTypedInput: true,
		},
	}); err != nil {
		return Config{}, "", fmt.Errorf("config: decode: %w", err)
	}

	return cfg, used, nil
}

// Validate rejects settings that would misbehave.
//
// Making something configurable is exactly when it needs checking: a value
// nobody could previously write is now one somebody can, and finding out at
// the moment it matters is worse than finding out at startup.
func (c Config) Validate() error {
	if err := validateLoopback(c.Server.Addr); err != nil {
		return err
	}

	if _, ok := profileNames[c.Agent.PermissionProfile]; !ok {
		return fmt.Errorf("config: agent.permission_profile %q is not a known profile", c.Agent.PermissionProfile)
	}
	if _, ok := logLevels[strings.ToLower(c.Server.LogLevel)]; !ok {
		return fmt.Errorf("config: server.log_level %q is not one of debug, info, warn, error", c.Server.LogLevel)
	}

	positive := map[string]int64{
		"agent.max_iterations":      int64(c.Agent.MaxIterations),
		"model.retry.max_attempts":  int64(c.Model.Retry.MaxAttempts),
		"tools.read_limit":          c.Tools.ReadLimit,
		"tools.max_readable_file":   c.Tools.MaxReadableFile,
		"tools.glob_results":        int64(c.Tools.GlobResults),
		"tools.grep_results":        int64(c.Tools.GrepResults),
		"tools.max_command_output":  int64(c.Tools.MaxCommandOutput),
		"delivery.text_flush_bytes": int64(c.Delivery.TextFlushBytes),
	}
	for name, value := range positive {
		if value <= 0 {
			return fmt.Errorf("config: %s must be greater than zero", name)
		}
	}

	if c.Tools.CommandTimeout > c.Tools.MaxCommandTimeout {
		return fmt.Errorf("config: tools.command_timeout (%s) is above tools.max_command_timeout (%s)",
			c.Tools.CommandTimeout, c.Tools.MaxCommandTimeout)
	}
	if c.Model.Retry.BaseDelay > c.Model.Retry.MaxDelay {
		return fmt.Errorf("config: model.retry.base_delay (%s) is above model.retry.max_delay (%s)",
			c.Model.Retry.BaseDelay, c.Model.Retry.MaxDelay)
	}
	if c.Model.Retry.Jitter < 0 || c.Model.Retry.Jitter > 1 {
		return fmt.Errorf("config: model.retry.jitter must be between 0 and 1")
	}

	return nil
}

var (
	profileNames = map[string]bool{"local": true, "gateway": true}
	logLevels    = map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
)

// LogLevel resolves the configured level, having been validated.
func (c Config) LogLevel() slog.Level {
	return logLevels[strings.ToLower(c.Server.LogLevel)]
}

// validateLoopback refuses to serve anywhere but the local machine.
//
// This API can read files and run programs. Binding it to a reachable
// interface is not a preference to be expressed in a config line; it is a
// decision that needs a deliberate, visible mechanism, and there is not one
// yet.
func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: server.addr %q is not host:port: %w", addr, err)
	}

	switch host {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf(
		"config: server.addr %q is not a loopback address; this API can run programs and is not safe to expose",
		addr)
}

// DefaultPath is where the configuration lives when none is given.
func DefaultPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: locate config dir: %w", err)
	}

	platform := filepath.Join(base, appDir, fileName)
	if _, err := os.Stat(platform); err == nil {
		return platform, nil
	}

	// Developer tools on macOS commonly follow the XDG convention even though
	// os.UserConfigDir does not, and a headless machine is usually set up that
	// way, so an existing file there is honoured.
	if home, err := os.UserHomeDir(); err == nil {
		xdg := filepath.Join(home, ".config", appDir, fileName)
		if _, err := os.Stat(xdg); err == nil {
			return xdg, nil
		}
	}

	return platform, nil
}

// Example is a starting configuration, written with everything commented so
// that copying it changes nothing until a line is deliberately uncommented.
//
// It documents every setting rather than the interesting few. A setting an
// operator can only find by reading the source is, in practice, not
// configurable; TestExampleDocumentsEverySetting keeps this honest by taking
// the list of settings from the struct itself.
const Example = `# JingClaw configuration.
#
# Every setting here has a default; a value only needs to appear if it differs.
# Flags override this file, and JINGCLAW_-prefixed environment variables sit
# between the two (JINGCLAW_AGENT_NAME sets agent.name, and so on).
#
# Durations are written the way Go writes them: "500ms", "2s", "10m".

[agent]
# What the agent calls itself. Leave empty to have it not claim a name, which
# is the honest choice when the account it speaks through is called something
# else.
# name = "JingClaw"

# Extra identity: tone, stance, what this deployment is for.
# persona = """
# You are careful and concise. You work on this team's Go services.
# """

# Standing directions for every session.
# instructions = """
# Prefer table-driven tests. Do not add dependencies without saying why.
# """

# Files read from the workspace when they exist. AGENTS.md is a convention
# several tools already share.
# instruction_files = ["AGENTS.md", "JINGCLAW.md"]

# How many model turns a single run may take before it gives up.
# max_iterations = 12

# What local sessions may do without being asked: "local" allows reading and
# editing the workspace and asks before running programs; "gateway" refuses to
# run programs at all. Choose "gateway" on a machine you share.
# permission_profile = "local"

[model]
# "gemini" talks to Google; "fake" is the offline provider, which needs no
# credential and is what the tests and demos use.
# provider = "fake"

# model = "gemma-4-31b-it"

# Where the API key comes from. Both are consulted, environment first. The key
# is never written to a log and never passed to a program the agent runs.
# api_key_env = ["GEMINI_API_KEY", "GOOGLE_API_KEY"]

# Read from the config directory, and only if it is mode 600.
# api_key_file = "gemini.key"

# How fast the offline provider pretends to think, so streaming and
# interruption can be watched at human speed.
# fake_delay = "150ms"

[model.retry]
# The right numbers depend on the account: a free tier hitting a daily quota
# and a paid plan with generous limits want different behaviour.
# max_attempts = 4
# base_delay = "500ms"
# max_delay = "30s"

# Spreads retries so several clients recovering from one outage do not resend
# in lockstep. Between 0 and 1.
# jitter = 0.3

[tools]
# How much of a file one read returns.
# read_limit = 65536

# Files above this are not read at all; the answer for one of those is to
# search it instead.
# max_readable_file = 8388608

# The ceiling on replacing a file in full. A model rewriting something large is
# nearly always making the wrong shape of edit.
# max_overwrite_bytes = 131072

# Files above this are skipped when searching. They are generated or minified
# far more often than they are the answer.
# max_searchable_file = 2097152

# How many results glob_files and grep return before they stop and say so.
# glob_results = 200
# grep_results = 100

# How long a program may run when it does not ask for a limit of its own, and
# the most it may ask for.
# command_timeout = "2m"
# max_command_timeout = "10m"

# How much of a program's output is kept. The middle is dropped first: the
# start says what ran and the end says how it ended.
# max_command_output = 32768

[delivery]
# How provider output becomes events. These bound the event log by the clock
# rather than by whatever chunk size the provider happens to use, which is what
# stops one talkative model turning every few characters into a write.
# text_flush_bytes = 240
# text_flush_interval = "200ms"
# usage_flush_interval = "2s"

[workspace]
# The only directory tools can reach. Relative paths are resolved against the
# directory the daemon starts in.
# root = "."

[server]
# Loopback only. This API can run programs; exposing it deserves more thought
# than a config line, so a non-loopback address is refused at startup.
# addr = "127.0.0.1:0"

# Where the database lives. Empty uses the platform's own location.
# data_dir = ""

# Where the discovery file lives, which is how the CLI and the gateway find a
# running daemon. Empty uses the platform's own location.
# runtime_dir = ""

# One of debug, info, warn, error.
# log_level = "info"

[gateway]
# Read by gatewayd, not by the daemon. Only "discord" is implemented.
# platform = "discord"

# Names this bot account within JingClaw, so channel bindings and the delivery
# queue belong to it. Running a second bot means a second account_id.
# account_id = "main"

# Where the bot token comes from. As with the API key, the file has to be mode
# 600 and the value never reaches a log.
# token_env = ["DISCORD_BOT_TOKEN"]
# token_file = "discord.token"
`
