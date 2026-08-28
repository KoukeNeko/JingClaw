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

	Command string   `koanf:"command"`
	Args    []string `koanf:"args"`

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

	// Discord is how that platform behaves. Its own section because these are
	// facts about Discord — its upload ceiling, its rate limits, where its
	// token lives — and a second platform would bring its own rather than
	// competing for these names.
	Discord Discord `koanf:"discord"`

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

	// WorkingInterval is the least time between "what it is doing now" lines.
	// A run that reads six files in a second would otherwise send six of them,
	// which is more than a person can read and more than a platform will take.
	WorkingInterval time.Duration `koanf:"working_interval"`

	// StreamInterval is the least time between versions of an answer that is
	// still being written. An answer finished inside one interval never
	// streams: it arrives whole, which is what it should do.
	StreamInterval time.Duration `koanf:"stream_interval"`
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
			Root: ".",
		},
		Server: Server{
			Addr:       "127.0.0.1:0",
			LogLevel:   "info",
			WebConsole: true,
			PairingTTL: 10 * time.Minute,
		},
		Gateway: Gateway{
			Platform:  "discord",
			AccountID: "main",
			Discord: Discord{
				TokenEnv:           []string{"DISCORD_BOT_TOKEN"},
				TokenFile:          "discord.token",
				MaxMessages:        3,
				MaxAttachmentBytes: 4 << 20,
				WorkingInterval:    2 * time.Second,
				StreamInterval:     1500 * time.Millisecond,
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

	if !platformNames[c.Gateway.Platform] {
		problems = append(problems, Problem{
			Key: "gateway.platform", Value: quote(c.Gateway.Platform),
			Why: "is not a platform that exists",
			Fix: `Only "discord" is implemented.`,
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
		{"gateway.working_interval", int64(c.Gateway.Discord.WorkingInterval), "Zero would say what it is doing on every tool call."},
		{"gateway.stream_interval", int64(c.Gateway.Discord.StreamInterval), "Zero would send a version of the answer per chunk."},
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
	platformNames = map[string]bool{"discord": true}

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
# Which model server to talk to.
#
#   "gemini"         Google's API. Needs a key.
#   "ollama"         An Ollama daemon on this machine, or Ollama Cloud.
#                    Settings in [model.ollama].
#   "openai_compat"  Anything speaking the OpenAI chat protocol: vLLM,
#                    LM Studio, llama.cpp, OpenRouter, Groq, Together.
#                    Settings in [model.openai_compat].
#   "fake"           No network and no credential. What the tests and demos
#                    use, and what proves a problem is not the model.
#
# provider = "fake"

# Which model to ask for, in whatever the chosen provider calls it:
# "gemma-4-31b-it" for gemini, "qwen3:8b" or "gemma4:31b-cloud" for ollama,
# whatever /v1/models lists for openai_compat.
#
# Empty is allowed only when the provider serves exactly one model. Where
# there are several the daemon refuses to start and names them, rather than
# picking one and leaving somebody to work out later which it chose.
# model = "gemma-4-31b-it"

# Where the credential comes from. Both are consulted, environment first. It
# is never written to a log and never passed to a program the agent runs.
#
# A model server on this machine usually needs none, and having none is an
# ordinary state rather than a startup failure. Ollama Cloud reached directly
# does need one; reached through a signed-in local daemon it does not.
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

# The most time one request may spend waiting across all its attempts.
#
# A server that is rate limiting may ask to be left alone for longer than the
# person watching is willing to wait. Its figure is honoured exactly rather
# than shortened — asking again early only earns the same refusal — so this is
# what decides when to stop instead and say when it would have been free.
#
# 0 bounds retries by the attempt count alone.
# budget = "1m30s"

[model.ollama]
# A local Ollama daemon, or the hosted service.
#
# One section for both: they are the same API at different addresses. For the
# hosted service set base_url to "https://ollama.com" and supply a credential
# the same way as for any other provider. A local daemon needs none.
#
# This uses Ollama's own API rather than its OpenAI-compatible one, because
# that is where the things a runtime needs live: how much context the server
# actually gave a model, whether it is loaded, and its thinking as a field
# rather than mixed into the answer.
# base_url = "http://localhost:11434"

# How long a model stays in memory after a request. Empty leaves the server's
# own default, which is the right choice on a machine doing other work.
# Written as a duration: "30m", "1h", or "0" to unload immediately.
# keep_alive = ""

# Ask for the model to be loaded with this much context.
#
# Worth setting. Ollama otherwise sizes the context against free memory, and
# on a busy machine a model trained for 128k is commonly loaded with 4k — and
# the whole session is then planned against that smaller figure.
# num_ctx = 0

# Ask the model to report its reasoning separately from its answer.
#
#   ""                            follow the model: ask when it says it can
#   "off"                         never ask
#   "on"                          always ask, at the server's own depth
#   "low" "medium" "high" "max"   always ask, at that depth
#
# Empty is the useful setting. Asking a model that cannot think is an error
# rather than something ignored, so anything fixed breaks the moment this
# points at a different model.
#
# Reasoning is kept out of the answer whichever way this is set. It is a
# separate kind of event and nothing forwards it to a chat channel.
# think = ""

[model.openai_compat]
# Any endpoint that speaks the OpenAI chat protocol: vLLM, LM Studio,
# llama.cpp's server, OpenRouter, Groq, Together.
#
# The address the chat path hangs off, usually ending in /v1. There is no
# default: an endpoint nobody named is not one to guess at.
# base_url = ""

# What this particular server does differently from the protocol it claims.
#
# "OpenAI-compatible" describes a request shape, not behaviour: servers
# disagree about whether usage is reported, which field carries reasoning, and
# what a status code means — one answers 403 for a prompt that is too long,
# which otherwise reads as a permissions failure nobody can fix.
#
# Named here rather than guessed from the address, because a proxy in front of
# a server makes the address say nothing about what is behind it.
#
# Known: generic, vllm, lmstudio, llamacpp, openrouter, groq, together.
# profile = "generic"

# What to call this endpoint in logs, so two of them can be told apart when
# one is failing. Empty uses the profile name.
# name = ""

[context]
# What the model is given of a long session. Replaying everything is what lets
# a session survive a restart, but a session that goes on long enough stops
# working; the older part is summarised into the log so that it does not.

# The model's context window in tokens. Zero asks the provider, which knows
# better than this file does. Set it for a local model served by something that
# does not report one.
# window = 0

# The share of the window at which history is summarised. Below one because the
# size of a request is estimated, not counted.
# compact_at = 0.7

# The share of the window the recent turns may keep, verbatim, afterwards.
# Must be below compact_at, or compacting would not make anything smaller.
# keep_fraction = 0.3

# The ceiling on the summary itself.
# summary_tokens = 1024

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

[artifacts]
# Where output too large to show the model is kept, so that truncating it is
# not the same as destroying it. The model is given an excerpt and an id, and
# reads the rest with read_artifact.

# Empty puts it beside the database, where the rest of the durable state is.
# dir = ""

# The ceiling on one artifact. Larger than this is a file that belongs in the
# workspace rather than output that was captured.
# max_bytes = 67108864

# The ceiling on one image put in front of the model. Larger is described in
# words instead: a provider refuses a request that is too big, and a refused
# request is worse than a picture nobody saw.
# max_image_bytes = 8388608

[memory]
# What the agent carries between sessions.
#
# On. What makes that safe is not this setting but the one thing that has not
# changed: nothing is ever written unattended. Writing a memory stops for a
# person in every profile, so turning this on grants the agent the ability to
# recall and to ask, never to decide by itself what it will believe later.
#
# What arrived from outside this machine stays marked as such and is never put
# in front of the model as a standing instruction.
#
# Set it to false to run with no memory at all.
# enabled = true

# The ceiling on the standing directions injected every turn. Everything here
# is context the work does not get.
# max_instruction_bytes = 2000

[web]
# Whether the agent may read web pages.
#
# Off by default. Turning it on lets the agent pull in text written by people
# who are not the operator, which the model reads alongside its own findings.
# What comes back is labelled as somebody else's and is never treated as an
# instruction, but the exposure is real and somebody should choose it.
#
# Reading only. There is no clicking, typing, signing in or submitting: that is
# a different power and it is not in this tool.
#
# Local sessions fetch unattended. Sessions arriving from a chat platform stop
# for the operator, because there the address is chosen by somebody else.
# enabled = false

# How a page is fetched.
#
# "browser" drives a real browser. It is slower than an HTTP request and it
# reaches the growing number of sites that answer anything else with a
# challenge page — which an agent otherwise reads as though it were the
# article. It needs Python and the cloakbrowser package on this machine.
#
# "none" disables fetching while leaving this section configured.
# backend = "browser"

# The interpreter the browser backend runs. Empty finds python3 on PATH.
# python = ""

# How long one page gets. A page still rendering at the deadline returns
# whatever it has, which beats returning nothing.
# timeout = "45s"

# How much of a page's text reaches the model. The whole text is stored either
# way and can be read back with read_artifact.
# max_characters = 40000

# How many of a page's links to list, so a next page can be named rather than
# guessed. A navigation page has thousands and none are worth the context.
# max_links = 50

# The channels this agent answers in.
#
# Declared here so a deployment is described in the file rather than in
# commands somebody has to remember running. Applied when the daemon starts;
# declaring a channel that already exists updates it.
#
# Removing one from this file does not unbind it. A daemon started once with an
# incomplete file would otherwise take channels away, and a binding decides who
# can reach the agent. The startup log names any bound channel this file does
# not, so nothing drifts unseen; "agent bindings remove" is how one goes.
#
# There are two lists, and which one a channel is in is what decides its
# powers. Neither can run programs: channel permissions settle who is in the
# room, which is what makes the rest reasonable, but they cannot settle whether
# an account still belongs to its owner — and a stolen one holds the request
# and the approval both. So that stays where somebody has to be at the machine.

# Rooms other people can type in. Reading runs unattended; changes, memory and
# web pages stop and ask; programs are refused.
#
# [[gateway.channels]]
# Several channels may share an entry when they share their rules. Repeating
# everything else for each is how two of them drift apart.
# channel_ids = ["111111111111111111"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
#
# Who may trigger work. Both empty permits nobody, which is the default a
# channel should have rather than answering anyone who finds it.
# users = ["333333333333333333"]
# roles = []

# Private channels you control, used as a remote console.
#
# The same as above, and additionally: fetching a page runs unattended, and an
# approval can be answered in the channel by replying "approve <id>" or
# "deny <id>". The channel is told what it is the first time it is used.
#
# [[gateway.consoles]]
# channel_ids = ["111111111111111112"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
# users = ["333333333333333333"]
# roles = []

[mcp]
# Tool servers speaking the Model Context Protocol, run as child processes.
#
# A server is somebody else's program. What it says about itself decides what
# the model is told a tool does; it does not decide what the tool is allowed to
# do. That is the level below, and it lives here rather than in the server.

# How long a server has to start and answer the handshake.
# start_timeout = "30s"

# How long one tool call may take.
# call_timeout = "2m"

# The ceiling on one result, so a single call cannot fill the context window.
# max_output = 32768

# One table per server. Tools arrive named mcp_<server>_<tool>, so installing
# one can never shadow a built-in: read_file keeps meaning the one that
# respects the workspace boundary.
#
# [[mcp.servers]]
# name = "sqlite"
# command = "uvx"
# args = ["mcp-server-sqlite", "--db-path", "/tmp/notes.db"]
#
# What its tools count as to the policy engine: internal, workspace_read,
# workspace_write, execute or high_impact. Defaults to execute, the honest
# floor for a call that makes another program on this machine act. Lower it
# only for a server you have read.
# level = "execute"
#
# Nothing is inherited from the daemon's environment that is not named here.
# It holds the provider credentials, and a tool server is exactly the kind of
# program that should not be handed them by default.
# pass_env = ["GITHUB_TOKEN"]
#
# [mcp.servers.env]
# LOG_LEVEL = "warn"

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

# Serve the built-in console from the same loopback address. The daemon prints
# the URL, which carries a one-time-visible token; the page keeps it for the
# tab and takes it back out of the address bar. Over SSH, forward the port.
# web_console = true

# How long the code in that URL stays good. It works once whatever happens
# here; this is the ceiling on how long an unused one is worth stealing out of
# a screenshot. "agent console" mints another.
# pairing_ttl = "10m"

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

# How many messages one answer may become before it goes as a file instead. An
# answer split into eight messages is a channel somebody has to scroll past for
# the rest of the day; as a file it is one line they can open.
# max_messages = 3

# The ceiling on an upload, well under what the platform accepts. The point is
# to be readable, not to be as large as possible.
# max_attachment_bytes = 4194304

# The least time between "what it is doing now" lines. They rewrite one message
# rather than stacking, so this is about how fast a person can read rather than
# about how much a channel can hold.
# working_interval = "2s"

# The least time between versions of an answer that is still being written.
# An answer finished inside one interval never streams: it arrives whole,
# which is what it should do.
# stream_interval = "1.5s"
`
