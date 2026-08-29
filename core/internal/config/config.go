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
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/KoukeNeko/JingClaw/core/internal/home"

	"github.com/KoukeNeko/JingClaw/core/internal/provider/openaicompat"
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
	Context   Context   `koanf:"context"`
	Tools     Tools     `koanf:"tools"`
	Artifacts Artifacts `koanf:"artifacts"`
	Memory    Memory    `koanf:"memory"`
	Web       Web       `koanf:"web"`
	MCP       MCP       `koanf:"mcp"`
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

	Ollama       Ollama       `koanf:"ollama"`
	OpenAICompat OpenAICompat `koanf:"openai_compat"`
}

// Ollama configures a local Ollama daemon or the hosted service.
//
// One section for both: they are the same API at different addresses, and the
// hosted one wants a credential.
type Ollama struct {
	// BaseURL defaults to a daemon on this machine. Point it at
	// https://ollama.com for the hosted service.
	BaseURL string `koanf:"base_url"`

	// KeepAlive is how long a model stays in memory after a request. Empty
	// leaves the server's own default, which is the right choice on a machine
	// doing other work.
	KeepAlive string `koanf:"keep_alive"`

	// NumCtx asks for a model to be loaded with this much context. Ollama
	// otherwise sizes it against free memory, which on a busy machine can be
	// a small fraction of what the model supports — and the whole session is
	// then planned against that.
	NumCtx int `koanf:"num_ctx"`

	// Think asks a model to report its reasoning separately from its answer.
	//
	// Empty follows the model: asked for when it reports being able to, and
	// not otherwise, because asking a model that cannot think is an error
	// rather than something ignored. "off" never asks, "on" always does, and
	// "low", "medium", "high" or "max" ask at a depth.
	Think string `koanf:"think"`
}

// OpenAICompat configures an endpoint that speaks the OpenAI chat protocol.
type OpenAICompat struct {
	// BaseURL is the root the chat path hangs off, usually ending in /v1.
	BaseURL string `koanf:"base_url"`

	// Profile names what this server does differently from the protocol it
	// claims. Named rather than guessed from the address, because a proxy
	// makes the address say nothing about what is behind it.
	Profile string `koanf:"profile"`

	// Name identifies this endpoint in logs, so two of them can be told apart
	// when one is failing.
	Name string `koanf:"name"`
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

	// Budget bounds the total time spent waiting across all attempts for one
	// request. A server may ask for longer than the person watching a channel
	// is willing to sit through, and the honest answer then is to stop and say
	// when it would be free rather than to retry early and fail again.
	Budget time.Duration `koanf:"budget"`
}

// problems reports what is wrong with one entry.
//
// seen carries channel ids already claimed, so the same room appearing in both
// lists is caught. Silently letting one win would give a channel powers the
// file appears to deny it, and which of the two won would depend on the order
// they are checked in.
func (c Channel) problems(where string, seen map[string]string) []Problem {
	var problems []Problem

	if len(c.ChannelIDs) == 0 {
		problems = append(problems, Problem{
			Key: where + ".channel_ids", Value: "[]",
			Why: "is empty, so this names no channel",
			Fix: `Give the platform's own ids, e.g. channel_ids = ["111111111111111111"].`,
		})
	}

	for _, id := range c.ChannelIDs {
		if strings.TrimSpace(id) == "" {
			problems = append(problems, Problem{
				Key: where + ".channel_ids", Value: `""`,
				Why: "contains an empty id",
				Fix: "Remove it, or give the channel's id.",
			})
			continue
		}
		if first, duplicate := seen[id]; duplicate {
			problems = append(problems, Problem{
				Key: where + ".channel_ids", Value: quote(id),
				Why: "is already declared in " + first,
				Fix: "A channel belongs to one list. Decide which, and remove the other.",
			})
			continue
		}
		seen[id] = where
	}

	if len(c.Users) == 0 && len(c.Roles) == 0 {
		problems = append(problems, Problem{
			Key: where + ".users", Value: "[]",
			Why: "is empty, and so is roles, which permits nobody",
			Fix: "List the accounts or roles that may trigger work here.",
		})
	}

	return problems
}

// Context bounds how much of a session is sent to the model.
//
// Replaying the whole history is what lets a session survive a restart, but it
// is unbounded: a session that goes on long enough stops working, and it stops
// at the moment somebody is in the middle of something. These settle when the
// older part is summarised and how much is kept as it was.
type Context struct {
	// Window overrides the model's context window. Zero asks the provider,
	// which is right whenever the provider knows; it is here for a local model
	// served by something that does not say.
	Window int64 `koanf:"window"`

	// CompactAt is the fraction of the window at which history is summarised.
	// It is below one because the size of a request is estimated rather than
	// counted, and an estimate has to be allowed to be wrong.
	CompactAt float64 `koanf:"compact_at"`

	// KeepFraction is how much of the window the verbatim tail may occupy once
	// the older part has been folded away.
	KeepFraction float64 `koanf:"keep_fraction"`

	// SummaryTokens caps the summary. One that may grow without limit is not a
	// smaller conversation.
	SummaryTokens int `koanf:"summary_tokens"`

	// KeepAfterFold is how many events before a summary are kept anyway.
	//
	// Once turns are folded, the conversation sent to the model reads the
	// summary and not the turns behind it, so they can go. A margin keeps the
	// tail of what was folded readable for somebody asking what actually
	// happened. Negative keeps everything, which is what a deployment that
	// wants a complete audit log wants.
	KeepAfterFold int `koanf:"keep_after_fold"`
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

// Artifacts is where output too large to show the model is kept.
//
// Without a store, truncation is destruction: the model is told there was more
// and given no way to reach it. With one, the excerpt it sees is a starting
// point and the whole of it is a tool call away.
type Artifacts struct {
	// Dir holds the content. Empty puts it beside the database, which is where
	// the rest of this daemon's durable state already lives.
	Dir string `koanf:"dir"`

	// MaxBytes bounds one artifact. Something larger is a file that belongs in
	// the workspace, not output that was captured.
	MaxBytes int64 `koanf:"max_bytes"`

	// MaxImageBytes bounds one image put in front of the model. Something
	// larger is described in words instead: a provider refuses a request that
	// is too big, and a refused request is worse than a picture nobody saw.
	MaxImageBytes int64 `koanf:"max_image_bytes"`
}

// Memory is what the agent carries between sessions.
//
// It is off by default. What is written here is read by every later session,
// by an agent that no longer knows where it came from, so turning it on is a
// decision somebody should make rather than one they inherit.
type Memory struct {
	Enabled bool `koanf:"enabled"`

	// MaxInstructionBytes bounds the standing directions put in front of the
	// model on every turn. Everything here is context the work does not get.
	MaxInstructionBytes int `koanf:"max_instruction_bytes"`
}

// Web is whether and how the agent may read pages.
//
// Off by default. Turning it on gives an agent that reads its own workspace
// the ability to also read text somebody else wrote, which is a different
// thing to have running unattended, and an operator should have to decide it.
type Web struct {
	Enabled bool `koanf:"enabled"`

	// Backend is how a page is fetched.
	//
	// "browser" drives a real browser, which is slower and reaches sites that
	// answer anything else with a challenge page. It needs Python and the
	// cloakbrowser package on this machine. "none" disables fetching while
	// leaving the rest of the section configured.
	Backend string `koanf:"backend"`

	// Python is the interpreter the browser backend runs. Empty finds python3
	// on PATH.
	Python string `koanf:"python"`

	// Timeout bounds one fetch. A page that has not rendered by then returns
	// what it has rather than nothing.
	Timeout time.Duration `koanf:"timeout"`

	// MaxCharacters bounds what one page puts in front of the model. The whole
	// text is kept as an artifact regardless.
	MaxCharacters int `koanf:"max_characters"`

	// MaxLinks bounds the link list. A navigation page can have thousands, and
	// none of them are worth the context.
	MaxLinks int `koanf:"max_links"`
}

// MCP is the tool servers this agent runs alongside itself.
//
// A server is somebody else's program. What it says about itself decides what
// the model is told a tool does; it does not decide what the tool is allowed
// to do, which is why the level lives here and not in the server's own
// description of itself.
type MCP struct {
	StartTimeout time.Duration `koanf:"start_timeout"`
	CallTimeout  time.Duration `koanf:"call_timeout"`

	// MaxOutput bounds one result, for the same reason the built-in tools have
	// a bound: a tool that can fill the context window in one call can end a
	// session in one call.
	MaxOutput int `koanf:"max_output"`

	Servers []MCPServer `koanf:"servers"`
}

// MCPServer is one server to run.
type MCPServer struct {
	// Name prefixes this server's tools, so installing one can never shadow a
	// built-in: read_file has to keep meaning the one that respects the
	// workspace boundary.
	Name string `koanf:"name"`

	// Command runs the server as a child of this daemon. URL reaches one that
	// is already running, over HTTP. Exactly one of them.
	Command string   `koanf:"command"`
	Args    []string `koanf:"args"`

	// URL is a Streamable HTTP endpoint. A server reached this way is not
	// started, stopped or killed by this daemon, and every tool call is a
	// network hop to an address named here.
	URL string `koanf:"url"`

	// Headers are sent with every request to a URL server, which is where an
	// authorization header goes.
	Headers map[string]string `koanf:"headers"`

	// Env are literal values for the child, and PassEnv names variables
	// forwarded from the daemon's own environment. Nothing else is inherited:
	// the daemon's environment holds the provider credentials, and handing
	// those to every server somebody installs would make installing one an act
	// of trust nobody was asked for.
	Env     map[string]string `koanf:"env"`
	PassEnv []string          `koanf:"pass_env"`

	// Level is what this server's tools count as to the policy engine. It
	// defaults to execute, which is the honest floor for a call that makes
	// another program on this machine act.
	Level string `koanf:"level"`
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

	// WorkingInterval is the least time between "what it is doing now" lines.
	// A run that reads six files in a second would otherwise send six of them,
	// which is more than a person can read and more than a platform will take.
	//
	// Here rather than under a platform: what paces these is the projector,
	// which does not know which platform will carry them.
	WorkingInterval time.Duration `koanf:"working_interval"`

	// StreamInterval is the least time between versions of an answer that is
	// still being written. An answer finished inside one interval never
	// streams: it arrives whole, which is what it should do.
	StreamInterval time.Duration `koanf:"stream_interval"`

	// Discord and Telegram are how each platform behaves. Their own sections
	// because these are facts about one service — its upload ceiling, where
	// its token lives — and naming them separately is what stops a setting
	// meant for one silently applying to the other.
	Discord  Discord  `koanf:"discord"`
	Telegram Telegram `koanf:"telegram"`

	// Channels are rooms other people can type in. Reading runs unattended;
	// changes, memory and web pages stop and ask; programs are refused.
	//
	// Applied when the daemon starts, so a deployment is described in the file
	// rather than in commands somebody has to remember running. Declaring a
	// channel that already exists updates it.
	//
	// Removing one from the file does not unbind it. A daemon started once
	// with an incomplete file would otherwise take channels away, and a
	// binding decides who may reach the agent; losing that by accident is
	// worse than leaving one behind. The startup log names any binding the
	// file does not, so drift is visible.
	Channels []Channel `koanf:"channels"`

	// Consoles are private channels an operator controls, which may answer
	// their own approvals.
	//
	// A separate list rather than a setting inside one, so that what a channel
	// may do is decided by which list it is in. A field naming the profile can
	// be misspelled, and can be given the profile meant for somebody at the
	// machine; a list cannot.
	//
	// Neither list can run programs. That needs somebody present.
	Consoles []Channel `koanf:"consoles"`
}

// Discord is what the Discord adapter needs.
type Discord struct {
	TokenEnv  []string `koanf:"token_env"`
	TokenFile string   `koanf:"token_file"`

	// MaxMessages is how many messages one answer may become before it is sent
	// as a file instead. An answer split into eight is a channel somebody has
	// to scroll past for the rest of the day.
	MaxMessages int `koanf:"max_messages"`

	// MaxAttachmentBytes bounds what is uploaded, well under what the platform
	// would accept: the point is to be readable, not to be as large as
	// possible.
	MaxAttachmentBytes int `koanf:"max_attachment_bytes"`
}

// Telegram is what the Telegram adapter needs.
//
// Shorter than Discord's because Telegram decides less: one message limit,
// no per-answer message budget worth setting, and no privileged intent to
// negotiate for.
type Telegram struct {
	TokenEnv  []string `koanf:"token_env"`
	TokenFile string   `koanf:"token_file"`

	// MaxUploadBytes bounds what is uploaded, well under what the platform
	// would accept.
	MaxUploadBytes int `koanf:"max_upload_bytes"`

	// APIBase is the API root. Configurable so a test, or a deployment behind
	// a local proxy, can point it somewhere it controls. Empty uses Telegram.
	APIBase string `koanf:"api_base"`
}

// Channel is a set of conversations sharing one set of rules.
type Channel struct {
	// ChannelIDs are the channels themselves. Several may share an entry:
	// rooms with the same workspace and the same people usually differ only
	// by id, and repeating everything else for each is how the two drift
	// apart.
	ChannelIDs []string `koanf:"channel_ids"`

	// TenantID is the guild or workspace they live in, where the platform has
	// such a thing.
	TenantID string `koanf:"tenant_id"`

	// WorkspaceID names the workspace runs from here use.
	WorkspaceID string `koanf:"workspace_id"`

	// Users and Roles are who may trigger work. Both empty means nobody: a
	// channel that answers anyone who finds it is not a default worth having.
	Users []string `koanf:"users"`
	Roles []string `koanf:"roles"`
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

	// WebConsole serves the built-in console from the same address. It is the
	// answer for a machine with no desktop on it, which is where this agent is
	// most useful and where a native client cannot go.
	WebConsole bool `koanf:"web_console"`

	// PairingTTL is how long the code a browser exchanges for its credential
	// stays good. It is short because the code is the part that ends up in a
	// terminal's scrollback and in screenshots.
	PairingTTL time.Duration `koanf:"pairing_ttl"`
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
				Budget:      90 * time.Second,
			},
			Ollama: Ollama{
				BaseURL: "http://localhost:11434",
			},
			OpenAICompat: OpenAICompat{
				Profile: "generic",
			},
		},
		Context: Context{
			CompactAt:     0.7,
			KeepFraction:  0.3,
			SummaryTokens: 1024,
			KeepAfterFold: 200,
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
		Artifacts: Artifacts{
			MaxBytes:      64 << 20,
			MaxImageBytes: 8 << 20,
		},
		Memory: Memory{
			Enabled:             true,
			MaxInstructionBytes: 2000,
		},
		Web: Web{
			Enabled:       false,
			Backend:       "browser",
			Timeout:       45 * time.Second,
			MaxCharacters: 40000,
			MaxLinks:      50,
		},
		MCP: MCP{
			StartTimeout: 30 * time.Second,
			CallTimeout:  2 * time.Minute,
			MaxOutput:    32 * 1024,
		},
		Delivery: Delivery{
			TextFlushBytes:     240,
			TextFlushInterval:  200 * time.Millisecond,
			UsageFlushInterval: 2 * time.Second,
		},
		Workspace: Workspace{
			Root: defaultWorkspace(),
		},
		Server: Server{
			Addr:       "127.0.0.1:0",
			LogLevel:   "info",
			WebConsole: true,
			PairingTTL: 10 * time.Minute,
		},
		Gateway: Gateway{
			Platform:        "discord",
			AccountID:       "main",
			WorkingInterval: 2 * time.Second,
			StreamInterval:  1500 * time.Millisecond,
			Discord: Discord{
				TokenEnv:           []string{"DISCORD_BOT_TOKEN"},
				TokenFile:          "discord.token",
				MaxMessages:        3,
				MaxAttachmentBytes: 4 << 20,
			},
			Telegram: Telegram{
				TokenEnv:       []string{"TELEGRAM_BOT_TOKEN"},
				TokenFile:      "telegram.token",
				MaxUploadBytes: 4 << 20,
			},
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

// Problem is one setting that will not do.
//
// It carries the key and the value as written so the report can point at the
// line an operator has to edit, rather than describing the mistake and leaving
// them to find it.
type Problem struct {
	Key   string
	Value string
	Why   string
	Fix   string
}

// InvalidError is everything wrong with a configuration.
type InvalidError struct {
	Problems []Problem
}

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, fmt.Sprintf("%s %s %s", p.Key, p.Value, p.Why))
	}
	return "config: " + strings.Join(parts, "; ")
}

// Report renders the problems for a terminal.
//
// The one-line Error is for logs. Somebody who has just mistyped a setting is
// about to open the file, so the report names the file, quotes each value back
// as they wrote it, and says what to write instead.
func (e *InvalidError) Report(file string) string {
	var b strings.Builder

	if file == "" {
		// Nothing was read, so the values came from the environment or from
		// flags; sending the reader to a file would waste their time.
		b.WriteString("\nConfiguration problem (no configuration file was read)\n\n")
	} else {
		fmt.Fprintf(&b, "\nConfiguration problem in %s\n\n", file)
	}

	for _, p := range e.Problems {
		fmt.Fprintf(&b, "  %s = %s\n", p.Key, p.Value)
		fmt.Fprintf(&b, "      %s\n", p.Why)
		if p.Fix != "" {
			fmt.Fprintf(&b, "      %s\n", p.Fix)
		}
		b.WriteString("\n")
	}

	b.WriteString("Run \"agentd --print-config\" to see every setting with its default.\n\n")
	return b.String()
}

// Report writes a readable account of a configuration problem to w, and says
// whether err was one.
//
// It lives here so that every command reports the same way; a daemon and a
// gateway explaining the same mistake differently is how an operator learns to
// distrust both.
func Report(w io.Writer, err error, file string) bool {
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		return false
	}

	fmt.Fprint(w, invalid.Report(file))
	return true
}

// Validate reports every setting that will not do.
//
// All of them at once, rather than stopping at the first: an operator who has
// to restart the daemon to discover the next mistake is doing work the program
// could have done in one pass.
//
// Making something configurable is exactly when it needs checking. A value
// nobody could previously write is now one somebody can, and finding out at
// the moment it matters is worse than finding out at startup.
func (c Config) Validate() error {
	var problems []Problem

	problems = append(problems, c.addressProblems()...)
	problems = append(problems, c.choiceProblems()...)
	problems = append(problems, c.rangeProblems()...)
	problems = append(problems, c.serverProblems()...)

	if len(problems) == 0 {
		return nil
	}
	return &InvalidError{Problems: problems}
}

func (c Config) addressProblems() []Problem {
	host, _, err := net.SplitHostPort(c.Server.Addr)
	if err != nil {
		return []Problem{{
			Key: "server.addr", Value: quote(c.Server.Addr),
			Why: "is not written as host:port",
			Fix: `Write it as "127.0.0.1:0"; port 0 picks a free one.`,
		}}
	}

	if isLoopback(host) {
		return nil
	}

	// This API can read files and run programs. Binding it to a reachable
	// interface is not a preference to be expressed in a config line; it is a
	// decision that needs a deliberate, visible mechanism, and there is not
	// one yet.
	return []Problem{{
		Key: "server.addr", Value: quote(c.Server.Addr),
		Why: "is not a loopback address",
		Fix: "This API can run programs and is not safe to expose. Use 127.0.0.1, ::1 or localhost.",
	}}
}

func (c Config) choiceProblems() []Problem {
	var problems []Problem

	if !profileNames[c.Agent.PermissionProfile] {
		problems = append(problems, Problem{
			Key: "agent.permission_profile", Value: quote(c.Agent.PermissionProfile),
			Why: "is not a profile that exists",
			Fix: `Use "local", or "gateway" to refuse to run programs at all.`,
		})
	}
	if _, ok := logLevels[strings.ToLower(c.Server.LogLevel)]; !ok {
		problems = append(problems, Problem{
			Key: "server.log_level", Value: quote(c.Server.LogLevel),
			Why: "is not a level that exists",
			Fix: "Use debug, info, warn or error.",
		})
	}
	if !providerNames[c.Model.Provider] {
		problems = append(problems, Problem{
			Key: "model.provider", Value: quote(c.Model.Provider),
			Why: "is not a provider that exists",
			Fix: `Use "gemini", "ollama", "openai_compat", or "fake" for the offline provider.`,
		})
	}
	// Each provider is checked only when it is the one selected. Refusing a
	// half-filled section for a provider nobody chose would make the file
	// impossible to keep several options in.
	switch c.Model.Provider {
	case "ollama":
		if !thinkLevels[strings.ToLower(strings.TrimSpace(c.Model.Ollama.Think))] {
			problems = append(problems, Problem{
				Key: "model.ollama.think", Value: quote(c.Model.Ollama.Think),
				Why: "is not a thinking setting",
				Fix: `Leave it empty to follow the model, or use "off", "on", "low", "medium", "high" or "max".`,
			})
		}
		if c.Model.Ollama.NumCtx < 0 {
			problems = append(problems, Problem{
				Key: "model.ollama.num_ctx", Value: fmt.Sprint(c.Model.Ollama.NumCtx),
				Why: "is negative",
				Fix: "Leave it at 0 to let the server size the context itself.",
			})
		}
		if c.Model.Ollama.KeepAlive != "" {
			if _, err := time.ParseDuration(c.Model.Ollama.KeepAlive); err != nil {
				problems = append(problems, Problem{
					Key: "model.ollama.keep_alive", Value: quote(c.Model.Ollama.KeepAlive),
					Why: "is not a duration",
					Fix: `Write it as "30m" or "1h". Leave it empty for the server's own default.`,
				})
			}
		}

	case "openai_compat":
		if strings.TrimSpace(c.Model.OpenAICompat.BaseURL) == "" {
			problems = append(problems, Problem{
				Key: "model.openai_compat.base_url", Value: `""`,
				Why: "is empty, and there is no default endpoint to fall back to",
				Fix: `Give the address the chat path hangs off, e.g. "http://localhost:8000/v1".`,
			})
		}
		if _, ok := openaicompat.ProfileByName(c.Model.OpenAICompat.Profile); !ok {
			problems = append(problems, Problem{
				Key: "model.openai_compat.profile", Value: quote(c.Model.OpenAICompat.Profile),
				Why: "is not a profile that exists",
				Fix: "Known profiles: " + strings.Join(openaicompat.ProfileNames(), ", ") + ".",
			})
		}
	}

	seen := map[string]string{}
	for _, list := range []struct {
		name     string
		channels []Channel
	}{
		{"gateway.channels", c.Gateway.Channels},
		{"gateway.consoles", c.Gateway.Consoles},
	} {
		for index, channel := range list.channels {
			where := fmt.Sprintf("%s[%d]", list.name, index)
			problems = append(problems, channel.problems(where, seen)...)
		}
	}

	for index, server := range c.MCP.Servers {
		where := fmt.Sprintf("mcp.servers[%d]", index)

		hasCommand := strings.TrimSpace(server.Command) != ""
		hasURL := strings.TrimSpace(server.URL) != ""

		switch {
		case hasCommand && hasURL:
			problems = append(problems, Problem{
				Key: where, Value: quote(server.Name),
				Why: "names both a command and a url",
				Fix: "A server is either started here or reached over HTTP. Remove one.",
			})
		case !hasCommand && !hasURL:
			problems = append(problems, Problem{
				Key: where, Value: quote(server.Name),
				Why: "names neither a command nor a url",
				Fix: "Give a command to run, or a url to connect to.",
			})
		}

		if hasURL {
			if parsed, err := url.Parse(server.URL); err != nil ||
				(parsed.Scheme != "http" && parsed.Scheme != "https") {
				problems = append(problems, Problem{
					Key: where + ".url", Value: quote(server.URL),
					Why: "is not an http or https address",
					Fix: "Give the endpoint the server listens on.",
				})
			}
		}
	}

	if !platformNames[c.Gateway.Platform] {
		problems = append(problems, Problem{
			Key: "gateway.platform", Value: quote(c.Gateway.Platform),
			Why: "is not a platform that exists",
			Fix: `Choose "discord" or "telegram".`,
		})
	}

	return problems
}

// serverProblems checks the tool servers.
//
// Each one is a child process this daemon will start, so a mistake here is a
// program that does not run or, worse, one whose tools quietly arrive with the
// wrong risk attached to them.
func (c Config) serverProblems() []Problem {
	var problems []Problem

	seen := make(map[string]bool, len(c.MCP.Servers))

	for index, server := range c.MCP.Servers {
		where := fmt.Sprintf("mcp.servers[%d]", index)

		switch {
		case server.Name == "":
			problems = append(problems, Problem{
				Key: where + ".name", Value: `""`,
				Why: "is empty",
				Fix: "Every server needs a name; it is what prefixes its tools.",
			})
		case seen[server.Name]:
			problems = append(problems, Problem{
				Key: where + ".name", Value: quote(server.Name),
				Why: "is used by more than one server",
				Fix: "Names prefix tool names, so two servers sharing one would collide.",
			})
		default:
			seen[server.Name] = true
		}

		if server.Command == "" {
			problems = append(problems, Problem{
				Key: where + ".command", Value: `""`,
				Why: "is empty",
				Fix: "Give the program to run, with its arguments in args.",
			})
		}

		if server.Level != "" && !toolLevels[server.Level] {
			problems = append(problems, Problem{
				Key: where + ".level", Value: quote(server.Level),
				Why: "is not a level that exists",
				Fix: "Use internal, workspace_read, workspace_write, remember, execute or high_impact.",
			})
		}
	}

	return problems
}

func (c Config) rangeProblems() []Problem {
	var problems []Problem

	// An explicit slice rather than a map, so two operators fixing the same
	// file are told about their mistakes in the same order.
	positive := []struct {
		key   string
		value int64
		fix   string
	}{
		{"agent.max_iterations", int64(c.Agent.MaxIterations), "A run needs at least one model turn to do anything."},
		{"model.retry.max_attempts", int64(c.Model.Retry.MaxAttempts), "One attempt means no retry; zero means no request."},
		{"tools.read_limit", c.Tools.ReadLimit, "This is how much of a file one read returns."},
		{"tools.max_readable_file", c.Tools.MaxReadableFile, "Zero would make every file too large to read."},
		{"tools.max_overwrite_bytes", c.Tools.MaxOverwriteBytes, "Zero would refuse every whole-file write."},
		{"tools.max_searchable_file", c.Tools.MaxSearchableFile, "Zero would make every file too large to search."},
		{"tools.glob_results", int64(c.Tools.GlobResults), "Zero would return no matches however many there are."},
		{"tools.grep_results", int64(c.Tools.GrepResults), "Zero would return no matches however many there are."},
		{"tools.max_command_output", int64(c.Tools.MaxCommandOutput), "Zero would discard everything a program printed."},
		{"delivery.text_flush_bytes", int64(c.Delivery.TextFlushBytes), "Zero would write an event per character."},
		{"context.summary_tokens", int64(c.Context.SummaryTokens), "A summary of nothing is not a summary."},
		{"mcp.max_output", int64(c.MCP.MaxOutput), "Zero would discard whatever a tool server answered."},
		{"artifacts.max_bytes", c.Artifacts.MaxBytes, "Zero would mean nothing can be kept."},
		{"artifacts.max_image_bytes", c.Artifacts.MaxImageBytes, "Zero would mean no image is ever shown to the model."},
		{"gateway.max_messages", int64(c.Gateway.Discord.MaxMessages), "Zero would send every answer as a file."},
		{"gateway.max_attachment_bytes", int64(c.Gateway.Discord.MaxAttachmentBytes), "Zero would make every attachment empty."},
		{"gateway.working_interval", int64(c.Gateway.WorkingInterval), "Zero would say what it is doing on every tool call."},
		{"gateway.stream_interval", int64(c.Gateway.StreamInterval), "Zero would send a version of the answer per chunk."},
	}
	for _, setting := range positive {
		if setting.value <= 0 {
			problems = append(problems, Problem{
				Key: setting.key, Value: fmt.Sprint(setting.value),
				Why: "must be greater than zero",
				Fix: setting.fix,
			})
		}
	}

	if c.Tools.CommandTimeout > c.Tools.MaxCommandTimeout {
		problems = append(problems, Problem{
			Key: "tools.command_timeout", Value: quote(c.Tools.CommandTimeout.String()),
			Why: fmt.Sprintf("is above tools.max_command_timeout (%s)", c.Tools.MaxCommandTimeout),
			Fix: "The default a program gets cannot exceed the most it may ask for.",
		})
	}
	if c.Model.Retry.BaseDelay > c.Model.Retry.MaxDelay {
		problems = append(problems, Problem{
			Key: "model.retry.base_delay", Value: quote(c.Model.Retry.BaseDelay.String()),
			Why: fmt.Sprintf("is above model.retry.max_delay (%s)", c.Model.Retry.MaxDelay),
			Fix: "The first wait cannot be longer than the longest wait.",
		})
	}
	if c.Memory.Enabled && c.Memory.MaxInstructionBytes <= 0 {
		problems = append(problems, Problem{
			Key: "memory.max_instruction_bytes", Value: fmt.Sprint(c.Memory.MaxInstructionBytes),
			Why: "must be greater than zero when memory is on",
			Fix: "It bounds the standing directions put in front of the model every turn.",
		})
	}

	if c.Web.Enabled {
		switch c.Web.Backend {
		case "browser", "none":
		default:
			problems = append(problems, Problem{
				Key: "web.backend", Value: quote(c.Web.Backend),
				Why: "is not a backend this knows",
				Fix: `Use "browser" to drive a real browser, or "none" to disable fetching.`,
			})
		}
		if c.Web.Timeout <= 0 {
			problems = append(problems, Problem{
				Key: "web.timeout", Value: c.Web.Timeout.String(),
				Why: "must be greater than zero",
				Fix: "A page that has not rendered by then returns what it has.",
			})
		}
		if c.Web.MaxCharacters <= 0 {
			problems = append(problems, Problem{
				Key: "web.max_characters", Value: fmt.Sprint(c.Web.MaxCharacters),
				Why: "must be greater than zero",
				Fix: "It bounds how much of a page reaches the model; the whole text is kept as an artifact regardless.",
			})
		}
		if c.Web.MaxLinks < 0 {
			problems = append(problems, Problem{
				Key: "web.max_links", Value: fmt.Sprint(c.Web.MaxLinks),
				Why: "is negative",
				Fix: "Use 0 to list no links at all.",
			})
		}
	}

	if c.Context.Window < 0 {
		problems = append(problems, Problem{
			Key: "context.window", Value: fmt.Sprint(c.Context.Window),
			Why: "is negative",
			Fix: "Leave it at 0 to use whatever window the provider reports.",
		})
	}
	for _, fraction := range []struct {
		key   string
		value float64
	}{
		{"context.compact_at", c.Context.CompactAt},
		{"context.keep_fraction", c.Context.KeepFraction},
	} {
		if fraction.value <= 0 || fraction.value >= 1 {
			problems = append(problems, Problem{
				Key: fraction.key, Value: fmt.Sprint(fraction.value),
				Why: "is not a fraction between 0 and 1",
				Fix: "It is a share of the context window, so 1 or more leaves no room to be wrong.",
			})
		}
	}
	if c.Context.KeepFraction >= c.Context.CompactAt {
		problems = append(problems, Problem{
			Key: "context.keep_fraction", Value: fmt.Sprint(c.Context.KeepFraction),
			Why: fmt.Sprintf("is not below context.compact_at (%v)", c.Context.CompactAt),
			Fix: "Compacting has to leave the conversation smaller than the size that triggered it.",
		})
	}

	if c.Model.Retry.Budget < 0 {
		problems = append(problems, Problem{
			Key: "model.retry.budget", Value: quote(c.Model.Retry.Budget.String()),
			Why: "is negative",
			Fix: "Use 0 to bound retries by attempts alone.",
		})
	}
	if c.Model.Retry.Jitter < 0 || c.Model.Retry.Jitter > 1 {
		problems = append(problems, Problem{
			Key: "model.retry.jitter", Value: fmt.Sprint(c.Model.Retry.Jitter),
			Why: "is not between 0 and 1",
			Fix: "It is the fraction of a delay that is randomised: 0.3 spreads retries by up to 30%.",
		})
	}

	return problems
}

var (
	profileNames  = map[string]bool{"local": true, "gateway": true}
	providerNames = map[string]bool{
		"fake": true, "gemini": true, "ollama": true, "openai_compat": true,
	}
	platformNames = map[string]bool{"discord": true, "telegram": true}

	thinkLevels = map[string]bool{
		"": true, "off": true, "on": true,
		"low": true, "medium": true, "high": true, "max": true,
	}
	toolLevels = map[string]bool{
		"internal":        true,
		"workspace_read":  true,
		"workspace_write": true,
		"remember":        true,
		"execute":         true,
		"high_impact":     true,
	}

	logLevels = map[string]slog.Level{
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

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// quote shows a value the way the operator wrote it, so an empty setting reads
// as "" rather than as nothing at all.
func quote(value string) string {
	return strconv.Quote(value)
}

// EnsureFile writes the example to the default location when nothing is there.
//
// The example is entirely commented, so creating it changes no behaviour. It
// exists so that configuring the agent means editing a file already sitting in
// front of you with every setting in it, rather than first finding out what to
// write and where to put it.
func EnsureFile() (path string, created bool, err error) {
	path, err = DefaultPath()
	if err != nil {
		return "", false, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, fmt.Errorf("config: create config directory: %w", err)
	}

	// O_EXCL rather than a Stat and then a write: two daemons starting
	// together must not both find the file missing and race to create it, and
	// an existing file must never be overwritten by a process that is only
	// trying to be helpful.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return path, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("config: create %s: %w", path, err)
	}

	if _, err := file.WriteString(Example); err != nil {
		_ = file.Close()
		return "", false, fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return "", false, fmt.Errorf("config: write %s: %w", path, err)
	}

	return path, true, nil
}

// defaultWorkspace is what the agent may touch when nothing says otherwise.
//
// Inside a .JingClaw directory when there is one, so that a deployment set up
// to try the thing out cannot reach the project it was set up in. Without one
// it is the working directory, as before.
func defaultWorkspace() string {
	if dir, found := home.FromWorkingDirectory(); found {
		return dir.Workspace()
	}
	return "."
}

// DefaultPath is where the configuration lives when none is given.
//
// A .JingClaw directory decides it when there is one. Otherwise the platform's
// own location, as before.
func DefaultPath() (string, error) {
	if dir, found := home.FromWorkingDirectory(); found {
		return dir.ConfigFile(), nil
	}

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
# Settings are shown at their defaults. Uncommenting one and leaving the value
# alone changes nothing; the point is that the value is visible. The commented
# ones are the rarer knobs.
#
# Precedence: flags beat the environment, which beats this file. Every setting
# has a JINGCLAW_ environment variable: JINGCLAW_MODEL_PROVIDER, and so on.

[agent]
name = "JingClaw"

# What runs may do without asking. "local" is for somebody at this machine.
# Channels get their own, which is not settable here.
permission_profile = "local"

# Read from the workspace root and put in front of the model. Missing files
# are skipped.
instruction_files = ["AGENTS.md", "JINGCLAW.md"]

# How many model turns one run may take before it gives up.
max_iterations = 12

# Extra standing text. persona shapes how it writes; instructions are added to
# every prompt. Neither is a permission: what the agent may do is decided by
# the policy engine, which reads none of this.
# persona = ""
# instructions = ""

[model]
# Which model server to talk to.
#
#   "gemini"         Google's API. Needs a key.
#   "ollama"         A daemon on this machine, or Ollama Cloud. See below.
#   "openai_compat"  vLLM, LM Studio, llama.cpp, OpenRouter, Groq, Together.
#   "fake"           No network, no credential. What proves a problem is not
#                    the model.
provider = "fake"

# Named as the chosen provider names it: "gemma-4-31b-it", "qwen3:8b",
# whatever /v1/models lists. Empty is allowed only when the provider serves
# exactly one model; otherwise the daemon names them and stops.
# model = ""

# Where the credential comes from, environment first. Never logged, never
# passed to a program the agent runs. A model server on this machine usually
# needs none, and having none is not an error.
api_key_env = ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
api_key_file = "gemini.key"

# How fast the offline provider pretends to think.
# fake_delay = "150ms"

[model.retry]
# A server's own Retry-After is honoured exactly, never shortened: asking again
# early earns the same refusal. These bound what this code invents when the
# server said nothing.
# max_attempts = 4
# base_delay = "500ms"
# max_delay = "30s"
# jitter = 0.3

# The most time one request may spend waiting across all its attempts. A server
# asking for longer than this ends the request with a reason instead.
# budget = "1m30s"

[model.ollama]
base_url = "http://localhost:11434"

# For Ollama Cloud, set base_url to "https://ollama.com" and supply a
# credential above. A local daemon needs none, including for cloud models it
# proxies.

# How long a model stays resident after a request. Empty leaves the server's
# default. "30m", "1h", or "0" to unload at once.
# keep_alive = ""

# Load the model with this much context. Worth setting: Ollama otherwise sizes
# it against free memory, and a model trained for 128k is commonly given 4k —
# which is then what the whole session is planned against.
# num_ctx = 0

# Ask for the model's reasoning separately. Empty follows the model, asking
# only when it reports being able to, because asking one that cannot is an
# error rather than something ignored. "off", "on", "low", "medium", "high",
# "max". Reasoning never reaches a chat channel either way.
think = ""

[model.openai_compat]
# The address the chat path hangs off, usually ending in /v1. No default.
# base_url = ""

# What this server does differently from the protocol it claims. Named rather
# than guessed, because a proxy makes the address say nothing.
# generic, vllm, lmstudio, llamacpp, openrouter, groq, together.
profile = "generic"

# What to call this endpoint in logs. Empty uses the profile name.
# name = ""

[context]
# The model's window, in tokens. Zero asks the provider, which is right
# whenever it knows; set it for a server that does not say.
window = 0

# When to fold older turns into a summary, and how much to keep as it was.
# compact_at = 0.7
# keep_fraction = 0.3
# summary_tokens = 1024

# How many events before a summary are kept anyway.
#
# Once turns are folded, the conversation sent to the model reads the summary
# and not the turns behind it, so they are discarded — that is what stops the
# log growing forever. This margin keeps the tail of what was folded readable
# for somebody asking what actually happened.
#
# A negative number keeps everything, for a deployment that wants a complete
# audit log and will manage the size itself.
# keep_after_fold = 200

[memory]
# What the agent carries between sessions.
#
# On. What makes that safe is that nothing is ever written unattended: writing
# a memory stops for a person in every profile. What arrived from outside this
# machine stays marked as such and never becomes a standing instruction.
enabled = true

# The ceiling on standing directions injected every turn.
# max_instruction_bytes = 2000

[web]
# Whether the agent may read web pages. Reading only: no clicking, typing,
# signing in or submitting.
#
# Local sessions fetch unattended. Channels stop for the operator, because
# there the address is chosen by somebody else.
enabled = false

# "browser" drives a real browser, which reaches the growing number of sites
# that answer anything else with a challenge page. Needs Python and the
# cloakbrowser package. "none" disables fetching.
backend = "browser"

# python = ""
# timeout = "45s"
# max_characters = 40000
# max_links = 50

[tools]
# Bounds on what one tool call may read, write or return. A tool that can fill
# the context window in one call can end a session in one call.
# read_limit = 65536
# max_readable_file = 8388608
# max_overwrite_bytes = 131072
# max_searchable_file = 2097152
# glob_results = 200
# grep_results = 100
# command_timeout = "2m"
# max_command_timeout = "10m"
# max_command_output = 32768

[artifacts]
# Where output too large to show is kept, content-addressed. Empty uses the
# data directory.
# dir = ""
# max_bytes = 67108864
# max_image_bytes = 8388608

[mcp]
# Tool servers speaking the Model Context Protocol, run as child processes.
#
# What a server says about itself decides what the model is told a tool does.
# It does not decide what the tool may do: that is the level below, and it
# lives here rather than in the server.
# start_timeout = "30s"
# call_timeout = "2m"
# max_output = 32768

# [[mcp.servers]]
# Prefixes this server's tools, so installing one cannot shadow a built-in.
# name = "fs"

# Either a command run as a child of this daemon, or a Streamable HTTP url
# reaching one already running. Never both.
#
# A url server is not started, stopped or killed by this daemon, and every
# tool call is a network hop to that address.
# command = "npx"
# args = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
# url = "http://localhost:9000/mcp"
# headers = { Authorization = "Bearer ..." }

# The child gets nothing from this daemon's environment except what is named
# here. The daemon holds the provider credentials.
# env = { EXAMPLE = "value" }
# pass_env = ["PATH", "HOME"]

# What its tools are gated at: internal, workspace_read, network_read,
# workspace_write, remember, execute, high_impact. Anything a server can do to
# a machine deserves execute unless somebody has read it.
# level = "execute"

[delivery]
# How often a streaming answer is flushed onward. Every delta becoming a
# message is a rate limit rather than a feature.
# text_flush_bytes = 240
# text_flush_interval = "200ms"
# usage_flush_interval = "2s"

[workspace]
# What the agent may touch. Every path is resolved inside it, symlinks
# included, and anything outside is refused.
#
# Left unset it is the workspace folder inside .JingClaw, or the working
# directory when there is no .JingClaw. Point it at your project to work on
# that instead.
# root = ""

[server]
# Loopback only. This API can run programs, so an address reachable from
# elsewhere is refused however it is written. Port 0 takes a free one and
# publishes it in the discovery file.
addr = "127.0.0.1:0"

log_level = "info"

# The embedded console, served from this same address.
web_console = true

# How long a printed pairing code stays valid.
# pairing_ttl = "10m"

# Empty uses the platform's own locations.
# data_dir = ""
# runtime_dir = ""

[gateway]
# Read by gatewayd. One of "discord" or "telegram"; the section below matching
# the one chosen is the one that is read.
platform = "discord"

# Names this bot within JingClaw, so bindings and the delivery queue belong to
# it. A second bot means a second account_id.
account_id = "main"

# The least time between "what it is doing now" lines, and between versions of
# an answer still being written. Here rather than under a platform: what these
# pace is the projector, which does not know which platform will carry them.
# working_interval = "2s"
# stream_interval = "1.5s"

# The channels this agent answers in, applied when the daemon starts.
# Declaring one that exists updates it.
#
# Removing one does not unbind it: a daemon started once against an incomplete
# file would take away the thing that decides who can reach the agent. The
# startup log names every bound channel this file does not.
#
# Which list a channel is in decides its powers. Neither can run programs.
# Channel permissions settle who is in the room, which is what makes the rest
# reasonable; they cannot settle whether an account still belongs to its owner,
# and a stolen one holds the request and the approval both.

# Rooms other people can type in. Reading is unattended; changes, memory and
# web pages stop and ask.
# [[gateway.channels]]
# Several channels may share an entry when they share their rules.
# channel_ids = ["111111111111111111"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
# Both empty permits nobody, which is the right default for a room.
# users = ["333333333333333333"]
# roles = []

# Private channels you control. As above, and additionally: pages are fetched
# unattended, and an approval can be answered by replying "approve <id>" or
# "deny <id>". The channel is told what it is the first time it is used.
# [[gateway.consoles]]
# channel_ids = ["111111111111111112"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
# users = ["333333333333333333"]
# roles = []

[gateway.discord]
# Where the bot token comes from. The file must be mode 600, and the value
# never reaches a log.
token_env = ["DISCORD_BOT_TOKEN"]
token_file = "discord.token"

# How many messages one answer may become before it goes as a file instead.
# max_messages = 3

# The upload ceiling, well under what Discord accepts.
# max_attachment_bytes = 4194304

[gateway.telegram]
# Where the bot token comes from. The file must be mode 600, and the value
# never reaches a log.
token_env = ["TELEGRAM_BOT_TOKEN"]
token_file = "telegram.token"

# The upload ceiling, well under what Telegram accepts.
# max_upload_bytes = 4194304

# The API root. Empty is Telegram itself; set it to reach a local proxy.
# api_base = ""
`
