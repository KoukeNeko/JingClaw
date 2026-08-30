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

	// envPrefix keeps JingClaw's variables from colliding with anything else
	// in a shell.
	envPrefix = "JINGCLAW_"
)

// Config is the whole of the daemon's settings.
type Config struct {
	Agent     Agent     `koanf:"agent"`
	Provider  Provider  `koanf:"provider"`
	Context   Context   `koanf:"context"`
	Tools     Tools     `koanf:"tools"`
	Artifacts Artifacts `koanf:"artifacts"`
	Sandbox   Sandbox   `koanf:"sandbox"`
	Memory    Memory    `koanf:"memory"`
	Web       Web       `koanf:"web"`
	MCP       MCP       `koanf:"mcp"`
	Process   Process   `koanf:"process"`
	Delivery  Delivery  `koanf:"delivery"`
	Server    Server    `koanf:"server"`
	Gateway   Gateway   `koanf:"gateway"`
}

// Agent is who the agent is and what it has been told.
//
// None of this is a security control. It shapes how the agent presents itself
// and what it tries; what it is permitted to do is settled by the policy
// engine, which does not read any of these fields.
// Who the agent is and how it works are files in the workspace, not settings.
//
// AGENTS.md and PERSONA.md sit beside the settings, are created by --init,
// and are read on every run in that order. Beside rather than in the
// workspace: they describe the agent, and the workspace is what the agent may
// change. There is deliberately no name, persona, instructions or
// instruction_files setting: each was a second place to say the same thing,
// and a second place is one somebody edits while the first is what runs.
//
// Fixed names rather than configurable ones for the same reason. AGENTS.md is
// a convention other tools already read, so a project that has one gets it
// free; PERSONA.md is this deployment's own. A setting naming them could only
// point at a file that does not exist.
const (
	InstructionsFile = "AGENTS.md"
	PersonaFile      = "PERSONA.md"
)

// InstructionFiles are read from the deployment directory when present, in
// the order returned.
func InstructionFiles() []string { return []string{InstructionsFile, PersonaFile} }

type Agent struct {
	// MaxIterations bounds the tool loop per run.
	MaxIterations int `koanf:"max_iterations"`

	// PermissionProfile decides what local sessions may do without asking.
	// Setting it to "gateway" makes even a local operator's runs refuse to
	// execute programs, which is a reasonable choice on a shared machine.
	PermissionProfile string `koanf:"permission_profile"`
}

type Provider struct {
	Backend string `koanf:"backend"`

	// FakeModel is what the offline stand-in calls itself. Empty uses its own
	// name, which is what every check wants.
	FakeModel string `koanf:"fake_model"`

	// FakeDelay paces the offline provider, so streaming and interruption can
	// be watched at human speed.
	FakeDelay time.Duration `koanf:"fake_delay"`

	// FakeReasoning is working-out the offline provider emits before its
	// answer. Empty means none, which is what most checks want.
	//
	// It exists so the path a thinking model takes can be driven without one:
	// recorded under its own kind, refused by the projector, shown only on the
	// control plane.
	FakeReasoning string `koanf:"fake_reasoning"`

	// FakeScript is what the offline provider does turn by turn instead of
	// echoing, so the paths only a model takes — a tool call, an approval —
	// can be checked without one.
	FakeScript []FakeTurn `koanf:"fake_script"`

	Retry Retry `koanf:"retry"`

	Gemini       Gemini       `koanf:"gemini"`
	Ollama       Ollama       `koanf:"ollama"`
	OpenAICompat OpenAICompat `koanf:"openai_compat"`
	Anthropic    Anthropic    `koanf:"anthropic"`
}

// Model is what the selected backend will answer with.
//
// Read through here rather than from a field, because each backend names its
// own: switching between them is one line, and the model each was set up with
// is still there when you switch back.
func (p Provider) Model() string {
	switch p.Backend {
	case "gemini":
		return p.Gemini.Model
	case "ollama":
		return p.Ollama.Model
	case "openai_compat":
		return p.OpenAICompat.Model
	case "anthropic":
		return p.Anthropic.Model
	case "fake":
		return p.FakeModel
	}
	return ""
}

// SetModel overrides the selected backend's model, for the flag that does
// that. It has to land where the daemon will read it rather than beside it.
func (p *Provider) SetModel(model string) {
	switch p.Backend {
	case "gemini":
		p.Gemini.Model = model
	case "ollama":
		p.Ollama.Model = model
	case "openai_compat":
		p.OpenAICompat.Model = model
	case "anthropic":
		p.Anthropic.Model = model
	case "fake":
		p.FakeModel = model
	}
}

// Each backend says where its own key comes from, rather than sharing one
// pair of settings.
//
// Shared, only one backend can be configured at a time: switching from a
// hosted model to a local one would mean editing the key settings as well as
// the name of the backend, and switching back would mean editing them again
// from memory. Declared per backend, everything stays set up and only
// `backend` changes.
//
// Two places because deployments differ: a service manager supplies a
// credential through the environment, a workstation through a file. The
// environment is read first, and neither value ever reaches a log.
//
// Written out on each backend rather than embedded from one shared struct.
// Defaults are seeded by walking this config, and that walk does not honour
// an embedded squash: the keys come out as provider.gemini.Credential.api_key_env
// and every default inside is silently lost.

// Gemini is what that backend needs, which is only a credential: it has one
// endpoint and no local variant.
type Gemini struct {
	// Model is what this backend answers with. Per backend, like the
	// credential above and for the same reason: a model name means nothing
	// outside the service that serves it, and one shared slot would have to
	// be retyped on every switch.
	Model string `koanf:"model"`

	APIKeyEnv  []string `koanf:"api_key_env"`
	APIKeyFile string   `koanf:"api_key_file"`
}

// Ollama configures a local Ollama daemon or the hosted service.
//
// One section for both: they are the same API at different addresses, and the
// hosted one wants a credential.
type Ollama struct {
	// Model is what this backend answers with. Per backend, like the
	// credential above and for the same reason: a model name means nothing
	// outside the service that serves it, and one shared slot would have to
	// be retyped on every switch.
	Model string `koanf:"model"`

	// Where this backend's key comes from, when it needs one: the hosted
	// service does and a local daemon has nothing to check it against.
	APIKeyEnv  []string `koanf:"api_key_env"`
	APIKeyFile string   `koanf:"api_key_file"`

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
// Anthropic is the Messages API.
//
// Its own backend rather than a dialect of the OpenAI-compatible one: the
// wire format differs in kind rather than in detail — content is typed blocks
// in both directions, a tool result is a block in a user message, and the
// stream is named events with indices.
type Anthropic struct {
	Model string `koanf:"model"`

	APIKeyEnv  []string `koanf:"api_key_env"`
	APIKeyFile string   `koanf:"api_key_file"`

	// BaseURL overrides the service, for a gateway or a proxy in front of it.
	BaseURL string `koanf:"base_url"`
}

type OpenAICompat struct {
	// Model is what this backend answers with. Per backend, like the
	// credential above and for the same reason: a model name means nothing
	// outside the service that serves it, and one shared slot would have to
	// be retyped on every switch.
	Model string `koanf:"model"`

	// Where this backend's key comes from, when it needs one: the hosted
	// service does and a local daemon has nothing to check it against.
	APIKeyEnv  []string `koanf:"api_key_env"`
	APIKeyFile string   `koanf:"api_key_file"`

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
// Sandbox confines what an approved command can reach.
//
// A human approval and this answer different questions. Approving says
// somebody meant to run a build; it cannot say what the build's dependencies
// will do, and for anything with a package manager in it that is most of what
// runs. So the two are worth having together.
type Sandbox struct {
	// Enabled confines every command run by exec_command.
	//
	// Off by default. It changes what a command can do, and a deployment that
	// gets it without asking finds out when something it relied on stops
	// working.
	//
	// On, it is not advisory: a machine that cannot confine refuses to run
	// the command. A sandbox that runs it anyway is one somebody believes in
	// and does not have.
	Enabled bool `koanf:"enabled"`

	// Network allows confined commands to open connections.
	//
	// Off by default, because most of what an agent runs is a build or a test
	// and neither needs one. A single setting rather than an exception per
	// command: a list of programs that may reach the network becomes, one
	// entry at a time, a network anybody may reach.
	Network bool `koanf:"network"`

	// Hidden are directories a confined command may not read.
	//
	// Confining writes says nothing about reads. A command that cannot write
	// outside the workspace can still open ~/.ssh, and having no network only
	// means it cannot send what it found today — it can print it, or leave it
	// somewhere a later command with a network will find.
	Hidden []string `koanf:"hidden"`
}

type Artifacts struct {
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

	// ExpandQueries lets a recall that matched nothing be tried once more
	// with other words, asked of the model.
	//
	// It costs a small model call, and only on a search that failed. What it
	// buys is the failure the index cannot report: the words did not
	// overlap, so nothing came back, and nothing said anything was missed.
	ExpandQueries bool `koanf:"expand_queries"`
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

	// Search is finding an address rather than reading one somebody named.
	//
	// Its own section, and its own switch, because it is a different thing to
	// be trusted with: reading fetches what somebody chose, and searching
	// lets the agent choose. A deployment may reasonably want one and not the
	// other.
	Search Search `koanf:"search"`
}

// Search is how the agent finds addresses worth reading.
type Search struct {
	// Backend is the search service. "brave" or "none".
	//
	// One implemented rather than four written from four sets of
	// documentation: every service has its own key, quota and result shape,
	// and an adapter that has never run is a liability. The next one is added
	// when somebody has a key to try it with.
	Backend string `koanf:"backend"`

	// KeyEnv and KeyFile are where the subscription token comes from. The
	// file must be mode 600, and the value never reaches a log.
	KeyEnv  []string `koanf:"key_env"`
	KeyFile string   `koanf:"key_file"`

	// Endpoint overrides the API root, for a deployment behind a proxy.
	// Empty uses the service's own.
	Endpoint string `koanf:"endpoint"`

	// MaxResults is how many results one search returns by default.
	MaxResults int `koanf:"max_results"`
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

// Process bounds what long-running programs may keep.
type Process struct {
	// BufferBytes is how much output is kept per process, oldest first to go.
	//
	// A dev server left running overnight would otherwise be a program whose
	// log is the whole of memory. The end to lose is the beginning: what a
	// caller wants is what it said a moment ago, and the start of a server's
	// log is a banner.
	BufferBytes int `koanf:"buffer_bytes"`
}

// FakeTurn is one scripted answer from the offline provider.
type FakeTurn struct {
	Text string `koanf:"text"`

	// Tool and Args are a call it makes. Empty Tool means it calls nothing.
	Tool string `koanf:"tool"`
	Args string `koanf:"args"`
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
}

// The platforms a gateway can serve. Named so the resolver below and the
// validator agree by symbol rather than by two spellings of one string.
const (
	PlatformDiscord  = "discord"
	PlatformTelegram = "telegram"
)

// Bindings is the part of a platform section that has the same shape whichever
// platform it is: which bot account, and which rooms it serves.
//
// A return type rather than an embedded field. Defaults are seeded by walking
// the config struct, and that walk does not honour an embedded squash: the
// keys come out as "gateway.discord.Bindings.account_id" and every default
// inside is silently lost. So each platform declares the three fields itself
// and this is what reads them back as one thing.
type Bindings struct {
	// AccountID names the bot account within JingClaw, so bindings and the
	// delivery queue can be scoped to it.
	AccountID string `koanf:"account_id"`

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

// Selected returns the account and rooms of the platform this daemon serves.
//
// Each platform declares its own rather than sharing one list, because a
// channel id, a guild id, a user id and a role id all belong to one service.
// One shared list would let a file written for Discord be read by a Telegram
// daemon, which is the failure the separate sections exist to prevent.
//
// A platform this build does not serve returns nothing. Validation reports
// that as a problem; this does not guess which section was meant.
func (g Gateway) Selected() Bindings {
	switch g.Platform {
	case PlatformDiscord:
		return Bindings{g.Discord.AccountID, g.Discord.Channels, g.Discord.Consoles}
	case PlatformTelegram:
		return Bindings{g.Telegram.AccountID, g.Telegram.Channels, g.Telegram.Consoles}
	}
	return Bindings{}
}

// SetAccountID overrides the selected platform's bot account.
//
// For the command-line flag that does that, which has to land in the section
// the daemon will actually read rather than beside it.
func (g *Gateway) SetAccountID(account string) {
	switch g.Platform {
	case PlatformDiscord:
		g.Discord.AccountID = account
	case PlatformTelegram:
		g.Telegram.AccountID = account
	}
}

// Discord is what the Discord adapter needs.
type Discord struct {
	AccountID string    `koanf:"account_id"`
	Channels  []Channel `koanf:"channels"`
	Consoles  []Channel `koanf:"consoles"`

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

	// TablesAsImages draws a table as a picture instead of writing it out in
	// a code block.
	//
	// Off by default, because it changes the shape of an answer rather than
	// its appearance: a picture cannot sit inside a message next to text, so
	// an answer with a table in the middle arrives as three messages instead
	// of one. It also cannot be selected, searched, or read aloud.
	//
	// What it buys is a table that is actually aligned. A code block is laid
	// out by whatever font the client happens to use, and no amount of
	// counting columns here makes that font agree — which is why a table of
	// Chinese and Latin text arrives bent however carefully it was measured.
	TablesAsImages bool `koanf:"tables_as_images"`
}

// Telegram is what the Telegram adapter needs.
//
// Shorter than Discord's because Telegram decides less: one message limit,
// no per-answer message budget worth setting, and no privileged intent to
// negotiate for.
type Telegram struct {
	AccountID string    `koanf:"account_id"`
	Channels  []Channel `koanf:"channels"`
	Consoles  []Channel `koanf:"consoles"`

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

	// Approvers and ApproverRoles are who may answer an approval from here,
	// by pressing a button on the message rather than by typing.
	//
	// A separate list from Users on purpose. Being allowed to ask the agent
	// for something and being allowed to permit it are different powers, and
	// a deployment where everybody in the room holds both is a choice an
	// operator should have to write down rather than get by default.
	//
	// Both empty means nobody, and the message then says to go and decide it
	// somewhere else. Typing is never enough here however these are set: a
	// message in a shared room says only which account posted it, and the
	// point of the button is that the platform tells us who pressed it.
	Approvers     []string `koanf:"approvers"`
	ApproverRoles []string `koanf:"approver_roles"`
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
			MaxIterations:     12,
			PermissionProfile: "local",
		},
		Provider: Provider{
			Backend: "fake",
			Gemini: Gemini{
				APIKeyEnv:  []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
				APIKeyFile: "gemini.key",
			},
			FakeDelay: 150 * time.Millisecond,
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
			ExpandQueries:       true,
		},
		Web: Web{
			Enabled:       false,
			Backend:       "browser",
			Timeout:       45 * time.Second,
			MaxCharacters: 40000,
			MaxLinks:      50,
			Search: Search{
				Backend:    "none",
				KeyEnv:     []string{"BRAVE_SEARCH_TOKEN"},
				KeyFile:    "brave-search.token",
				MaxResults: 8,
			},
		},
		MCP: MCP{
			StartTimeout: 30 * time.Second,
			CallTimeout:  2 * time.Minute,
			MaxOutput:    32 * 1024,
		},
		Process: Process{
			BufferBytes: 256 << 10,
		},
		Delivery: Delivery{
			TextFlushBytes:     240,
			TextFlushInterval:  200 * time.Millisecond,
			UsageFlushInterval: 2 * time.Second,
		},
		Server: Server{
			Addr:     "127.0.0.1:0",
			LogLevel: "info",
		},
		Gateway: Gateway{
			Platform:        PlatformDiscord,
			WorkingInterval: 2 * time.Second,
			StreamInterval:  1500 * time.Millisecond,
			Discord: Discord{
				AccountID:          "main",
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

	b.WriteString("Run \"jingclaw daemon --print-config\" to see every setting with its default.\n\n")
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
	if !providerNames[c.Provider.Backend] {
		problems = append(problems, Problem{
			Key: "provider.backend", Value: quote(c.Provider.Backend),
			Why: "is not a provider that exists",
			Fix: `Use "gemini", "anthropic", "ollama", "openai_compat", or "fake" for the offline provider.`,
		})
	}
	// Each provider is checked only when it is the one selected. Refusing a
	// half-filled section for a provider nobody chose would make the file
	// impossible to keep several options in.
	switch c.Provider.Backend {
	case "ollama":
		if !thinkLevels[strings.ToLower(strings.TrimSpace(c.Provider.Ollama.Think))] {
			problems = append(problems, Problem{
				Key: "provider.ollama.think", Value: quote(c.Provider.Ollama.Think),
				Why: "is not a thinking setting",
				Fix: `Leave it empty to follow the model, or use "off", "on", "low", "medium", "high" or "max".`,
			})
		}
		if c.Provider.Ollama.NumCtx < 0 {
			problems = append(problems, Problem{
				Key: "provider.ollama.num_ctx", Value: fmt.Sprint(c.Provider.Ollama.NumCtx),
				Why: "is negative",
				Fix: "Leave it at 0 to let the server size the context itself.",
			})
		}
		if c.Provider.Ollama.KeepAlive != "" {
			if _, err := time.ParseDuration(c.Provider.Ollama.KeepAlive); err != nil {
				problems = append(problems, Problem{
					Key: "provider.ollama.keep_alive", Value: quote(c.Provider.Ollama.KeepAlive),
					Why: "is not a duration",
					Fix: `Write it as "30m" or "1h". Leave it empty for the server's own default.`,
				})
			}
		}

	case "openai_compat":
		if strings.TrimSpace(c.Provider.OpenAICompat.BaseURL) == "" {
			problems = append(problems, Problem{
				Key: "provider.openai_compat.base_url", Value: `""`,
				Why: "is empty, and there is no default endpoint to fall back to",
				Fix: `Give the address the chat path hangs off, e.g. "http://localhost:8000/v1".`,
			})
		}
		if _, ok := openaicompat.ProfileByName(c.Provider.OpenAICompat.Profile); !ok {
			problems = append(problems, Problem{
				Key: "provider.openai_compat.profile", Value: quote(c.Provider.OpenAICompat.Profile),
				Why: "is not a profile that exists",
				Fix: "Known profiles: " + strings.Join(openaicompat.ProfileNames(), ", ") + ".",
			})
		}
	}

	// Only the selected platform's rooms are checked. A file may describe
	// both, and the one this daemon is not serving is not its business.
	bound := c.Gateway.Selected()

	seen := map[string]string{}
	for _, list := range []struct {
		name     string
		channels []Channel
	}{
		{"gateway." + c.Gateway.Platform + ".channels", bound.Channels},
		{"gateway." + c.Gateway.Platform + ".consoles", bound.Consoles},
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

		// One of the two, never both, which is the same rule the transport
		// applies. Requiring a command outright — which this used to — made
		// the url setting undocumentable in practice: it existed, it worked,
		// and no configuration that used it would start.
		switch {
		case server.Command == "" && server.URL == "":
			problems = append(problems, Problem{
				Key: where + ".command", Value: `""`,
				Why: "is empty, and so is url",
				Fix: "Give the program to run, with its arguments in args — " +
					"or a url, for a server that is already running.",
			})
		case server.Command != "" && server.URL != "":
			problems = append(problems, Problem{
				Key: where + ".url", Value: quote(server.URL),
				Why: "is set, and so is command",
				Fix: "A server is a child process or an endpoint, never both. Remove one.",
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
		{"provider.retry.max_attempts", int64(c.Provider.Retry.MaxAttempts), "One attempt means no retry; zero means no request."},
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
	if c.Provider.Retry.BaseDelay > c.Provider.Retry.MaxDelay {
		problems = append(problems, Problem{
			Key: "provider.retry.base_delay", Value: quote(c.Provider.Retry.BaseDelay.String()),
			Why: fmt.Sprintf("is above model.retry.max_delay (%s)", c.Provider.Retry.MaxDelay),
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

		switch c.Web.Search.Backend {
		case "brave", "none", "":
		default:
			problems = append(problems, Problem{
				Key: "web.search.backend", Value: quote(c.Web.Search.Backend),
				Why: "is not a search backend this knows",
				Fix: `Use "brave", or "none" to leave searching off.`,
			})
		}
		if c.Web.Search.MaxResults < 0 {
			problems = append(problems, Problem{
				Key: "web.search.max_results", Value: fmt.Sprint(c.Web.Search.MaxResults),
				Why: "is negative",
				Fix: "Leave it at 0 to use the default of 8.",
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

	if c.Provider.Retry.Budget < 0 {
		problems = append(problems, Problem{
			Key: "provider.retry.budget", Value: quote(c.Provider.Retry.Budget.String()),
			Why: "is negative",
			Fix: "Use 0 to bound retries by attempts alone.",
		})
	}
	if c.Provider.Retry.Jitter < 0 || c.Provider.Retry.Jitter > 1 {
		problems = append(problems, Problem{
			Key: "provider.retry.jitter", Value: fmt.Sprint(c.Provider.Retry.Jitter),
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
		"anthropic": true,
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

// DefaultPath is where the configuration lives when none is given.
//
// One answer, from one place. The platform-native directory, the XDG
// convention and an existence check used to decide between them, which meant
// three rules produced one path and nobody could say which had applied.
func DefaultPath() (string, error) {
	dir, found := home.Resolve()
	if !found {
		return "", fmt.Errorf("config: %s is set to none, so there is no default", home.EnvVar)
	}
	return dir.ConfigFile(), nil
}

// Example is a starting configuration.
//
// A starter, not a reference. Every setting appears — a setting an operator
// can only find by reading the source is, in practice, not configurable, and
// TestExampleDocumentsEverySetting keeps that honest — but a setting only
// carries a comment when one of these is true:
//
//   - the value shown is deliberately not the default
//   - it has a safety or exposure consequence
//   - it constrains, or is constrained by, another setting
//   - its syntax cannot be guessed from its name
//   - the line exists to show a workflow rather than to be edited
//
// Everything else that was here is in docs/configuration.md. The test for a
// comment is not "is this worth knowing" — nearly all of it is — but "would
// deleting it make somebody editing this line more likely to get it wrong".
//
// Everything is commented out, so copying this file changes nothing until a
// line is deliberately uncommented. The daemon reports every unusable setting
// at startup by name, value and fix, which is where a wrong value gets
// explained rather than here.
const Example = `# JingClaw configuration. Every setting is shown at its default.
# Reference: docs/configuration.md

# ── Agent and provider ─────────────────────────────────────────

[agent]
# Who this agent is and how it works are AGENTS.md and PERSONA.md, beside
# this file and created with it. They are not settings: a second place to say
# the same thing is one somebody edits while the first is what runs.

# Tool calls one turn may make before the run is stopped.
# max_iterations = 12

# "local" stops for a person on changes and commands. "gateway" is stricter
# and is what a channel gets; naming it here applies it to this machine too.
# permission_profile = "local"

[provider]
# "gemini", "ollama" or "openai_compat". Left as it is, an offline stand-in
# answers instead of a model, so a fresh install starts and does nothing
# surprising; set this before expecting real work.
backend = "fake"

# Each backend names its own model and credential below, so switching between
# them is this one line and nothing else.

[provider.retry]
# max_attempts = 4
# base_delay = "500ms"
# max_delay = "30s"
# jitter = 0.3
# Total time across all attempts. Reached first, the attempts stop.
# budget = "1m30s"

[provider.gemini]
# model = ""
# The environment is read first, then the file, which must be mode 600.
# Neither value ever reaches a log.
# api_key_env = ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
# api_key_file = "gemini.key"

[provider.ollama]
# model = ""
# Only the hosted service needs one; a local daemon has nothing to check it
# against.
# api_key_env = []
# api_key_file = ""
# base_url = "http://localhost:11434"
# How long a model stays loaded after a request. Empty uses Ollama's own.
# keep_alive = ""
# Context the server loads the model with. Zero uses whatever it decides,
# which is routinely far less than the model was trained for.
# num_ctx = 0
# "" leaves the model's own setting alone; "true" or "false" overrides it.
# think = ""

[provider.anthropic]
# The Messages API. Its own backend rather than a dialect of the one below:
# the wire format differs in kind, not in detail.
# model = "claude-sonnet-4-5"
# api_key_env = ["ANTHROPIC_API_KEY"]
# api_key_file = ""
# For a gateway or a proxy in front of the service.
# base_url = ""

[provider.openai_compat]
# model = ""
# api_key_env = []
# api_key_file = ""
# Required for this backend: there is no default endpoint.
# base_url = ""
# "generic", or a name where a server needs its own handling.
# profile = "generic"
# name = ""

[context]
# Tokens to plan against. Zero asks the provider, which is usually right.
# window = 0
# Fraction of the window at which the conversation is folded into a summary.
# compact_at = 0.7
# keep_fraction = 0.3
# summary_tokens = 1024
# Events kept behind a fold. Below zero keeps everything and grows forever.
# keep_after_fold = 200

# ── Durable state ───────────────────────────────────────────
#
# Where things live is not a setting. The agent reads and writes one
# directory, the deployment's own workspace, and nothing outside it is
# reachable; stored output goes beside the database. Two ways to say where
# something is would be two places for them to disagree, and the answer to
# "which one ran" would depend on which file somebody edited last.

[artifacts]
# How large one piece of stored output may be, and how large one image put in
# front of the model may be.
# max_bytes = 67108864
# max_image_bytes = 8388608

[sandbox]
# Confine what a command run by exec_command can reach: it may write to the
# workspace and to this deployment's own caches, and nothing else on the
# machine. Approving a command says somebody meant to run it; it cannot say
# what a build's dependencies will do, and this is the difference.
#
# Off by default because it changes what commands can do. On, it is not
# advisory: a machine that cannot confine refuses to run the command rather
# than running it unprotected.
#
# macOS only so far.
# enabled = false
# Let confined commands open connections. Off suits a build or a test, which
# is most of what an agent runs.
# network = false
# Directories a confined command may not read. Confining writes says nothing
# about reads, and having no network only means it cannot send today what it
# read today.
# hidden = ["~/.ssh", "~/.aws"]

[memory]
# What the agent carries between sessions. A retrieval memory is written
# unattended; a standing one goes in front of the model every turn and stops
# for a person in every profile. What arrived from outside this machine is
# marked, and never becomes a standing instruction.
enabled = true
# max_instruction_bytes = 2000
# Ask the model for other words when a lookup matches nothing. Costs one
# small call, only on a search that already failed.
# expand_queries = true

# ── Capabilities ────────────────────────────────────────────

[tools]
# read_limit = 65536
# max_readable_file = 8388608
# max_overwrite_bytes = 131072
# max_searchable_file = 2097152
# glob_results = 200
# grep_results = 100
# command_timeout = "2m"
# What a call may ask for, above which it is refused rather than granted.
# max_command_timeout = "10m"
# Output past this is stored as an artifact and named in the result.
# max_command_output = 32768

[process]
# Output kept per long-running program, oldest discarded first.
# buffer_bytes = 262144

[web]
# Off by default: reading pages gives an agent somebody else's words.
enabled = false
# "browser" drives a real browser and needs python3 with cloakbrowser.
# backend = "browser"
# python = ""
# timeout = "45s"
# max_characters = 40000
# max_links = 50

[web.search]
# "brave", or "none" to leave the section configured and switched off.
# backend = "none"
# key_env = ["BRAVE_SEARCH_TOKEN"]
# key_file = "brave-search.token"
# endpoint = ""
# max_results = 8

[mcp]
# start_timeout = "30s"
# call_timeout = "2m"
# max_output = 32768

# A server is a child process (command) or an endpoint (url), never both.
# [[mcp.servers]]
# name = "files"
# command = "mcp-server-filesystem"
# args = ["--root", "."]
# url = ""
# headers = { authorization = "Bearer ..." }
# env = { LOG_LEVEL = "info" }
# Names variables passed through from this daemon's own environment.
# pass_env = ["HOME"]
# What this server's tools count as to the policy engine.
# level = "execute"

# ── Interfaces ──────────────────────────────────────────────

[server]
# 127.0.0.1 only. A LAN address exposes the daemon to the network.
# addr = "127.0.0.1:0"
# log_level = "info"
# Empty uses the platform's own locations.
# data_dir = ""
# runtime_dir = ""

[gateway]
# Read by the gateway. The section below matching this is the one that is read.
platform = "discord"
# working_interval = "2s"
# stream_interval = "1.5s"

[gateway.discord]
# The file must be mode 600. Neither value reaches a log.
token_env = ["DISCORD_BOT_TOKEN"]
token_file = "discord.token"
# A second bot needs a second account_id.
account_id = "main"
# Past this many, an answer is sent as a file instead.
# max_messages = 3
# Draw a table as a picture rather than writing it out in a code block.
# Aligned, because the layout is decided here rather than by whatever font the
# reader's client uses — but an answer with a table in it then arrives as
# several messages, and a picture cannot be selected or searched.
# tables_as_images = false
# max_attachment_bytes = 4194304

# Rooms other people can type in. Applied at startup; removing one does not
# unbind it, and the startup log names any binding this file does not.
# [[gateway.discord.channels]]
# channel_ids = ["111111111111111111"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
# Who may ask. Both empty is nobody, which is the right default for a room.
# users = ["333333333333333333"]
# roles = []
# Who may approve, by pressing a button on the message. A separate list:
# asking for something and permitting it are different powers. Both empty is
# nobody, and the message then says to decide it elsewhere.
# approvers = ["333333333333333333"]
# approver_roles = []

# Private rooms you control. As above, and additionally: pages are fetched
# unattended, and an approval is answered by replying "approve <id>".
# [[gateway.discord.consoles]]
# channel_ids = ["111111111111111112"]
# tenant_id = "222222222222222222"
# workspace_id = "default"
# users = ["333333333333333333"]
# roles = []
# approvers = []
# approver_roles = []

[gateway.telegram]
token_env = ["TELEGRAM_BOT_TOKEN"]
token_file = "telegram.token"
# account_id = "main"
# max_upload_bytes = 4194304
# Empty is Telegram itself; set it to reach a local proxy.
# api_base = ""

# [[gateway.telegram.channels]] and [[gateway.telegram.consoles]] take the
# same fields as Discord's above, naming Telegram's own ids.

[delivery]
# How text is batched on its way to a channel.
# text_flush_bytes = 240
# text_flush_interval = "200ms"
# usage_flush_interval = "2s"
`
