# Configuration

`config.example.toml` is a starter, not a reference. Every setting appears in
it at its default, and carries a comment only where deleting that comment
would make somebody editing the line more likely to get it wrong. The reasons
live here instead.

`agentd` reports every unusable setting at startup by name, value, why it will
not do, and how to fix it. That is where a wrong value gets explained; this is
where a setting's reasons are.

## Where the file is, and what writes it

Every setting the daemon, the CLI and the gateway have is written in one file.
Flags exist so a single run can differ from it, not so that a deployment has to
be described on a command line.

The daemon writes the file the first time it starts, so there is nothing to
find out and nowhere to put it:

```
$ jingclaw daemon
JingClaw daemon
Listening: http://127.0.0.1:54832
Config:    /Users/you/.config/JingClaw/config.toml (created, all defaults)
```

Every setting is in it and every line is commented, so its arrival changes
nothing until a line is deliberately uncommented. An existing file is never
touched. [`core/config.example.toml`](core/config.example.toml) is the same
content, checked in so it can be read without running anything, and
`--print-config` writes it to stdout.

The location is the platform config directory — `~/Library/Application Support/JingClaw/` on macOS,
`%AppData%\JingClaw\` on Windows, `~/.config/JingClaw/` on Linux and on macOS
when a file is already there. `--config` names another one.

Precedence runs defaults → file → `JINGCLAW_`-prefixed environment →
flags, so `JINGCLAW_AGENT_NAME` overrides `agent.name`, and `--model` overrides
both. Only a flag actually typed counts; an unset one does not quietly replace
a configured value with a default.

Three variables carry a whole file rather than one setting, for deployments
whose only inputs are a volume and an environment: `JINGCLAW_CONFIG`,
`JINGCLAW_PERSONA` and `JINGCLAW_AGENTS` write `config.toml`, `PERSONA.md`
and `AGENTS.md` on first start, never over a file that is already there. A
value prefixed `base64:` is decoded first, so a multi-line document survives
a platform's single-line form. [`container.md`](container.md) has the whole
arrangement.

What the agent says it is comes from here too:

```toml
[agent]
name = "江委員"
persona = """
You are careful and concise. You work on this team's Go services.
"""
```

The prompt is assembled in layers, each with a stated source, and
`--print-prompt` shows the whole thing with where every part came from. One
layer is deliberately not configurable: the contract that tells the model how
tools behave and what a refusal means. Letting an operator edit that would let
them describe a system that does not exist.

Settings are checked at startup rather than at the moment they matter, and all
of them at once — restarting a daemon to discover the next mistake is work the
program can do in one pass:

```
Configuration problem in /Users/you/.config/JingClaw/config.toml

  server.addr = "0.0.0.0:9977"
      is not a loopback address
      This API can run programs and is not safe to expose. Use 127.0.0.1, ::1 or localhost.

  model.provider = "openai"
      is not a provider that exists
      Use "gemini", or "fake" for the offline provider.
```

That last refusal is not a preference. This API reads files and runs programs;
binding it somewhere reachable needs a more deliberate mechanism than a config
line, and there is not one.

---

## `(no section)`

JingClaw configuration.

Settings are shown at their defaults. Uncommenting one and leaving the value
alone changes nothing; the point is that the value is visible. The commented
ones are the rarer knobs.

Precedence: flags beat the environment, which beats this file. Every setting
has a JINGCLAW_ environment variable: JINGCLAW_PROVIDER_BACKEND, and so on.

## `[agent]`

**`permission_profile`**

What runs may do without asking. "local" is for somebody at this machine.
Channels get their own, which is not settable here.

**`instruction_files`**

Read from the workspace root and put in front of the model. Missing files
are skipped.

**`max_iterations`**

How many model turns one run may take before it gives up.

**`persona`**

Extra standing text. persona shapes how it writes; instructions are added to
every prompt. Neither is a permission: what the agent may do is decided by
the policy engine, which reads none of this.

## `[model]`

**`provider`**

Which model server to talk to.

"gemini"         Google's API. Needs a key.
"ollama"         A daemon on this machine, or Ollama Cloud. See below.
"openai_compat"  vLLM, LM Studio, llama.cpp, OpenRouter, Groq, Together.
"fake"           No network, no credential. What proves a problem is not
the model.

**`model`**

Named as the chosen provider names it: "gemma-4-31b-it", "qwen3:8b",
whatever /v1/models lists. Empty is allowed only when the provider serves
exactly one model; otherwise the daemon names them and stops.

**`api_key_env`**

Where the credential comes from, environment first. Never logged, never
passed to a program the agent runs. A model server on this machine usually
needs none, and having none is not an error.

**`fake_delay`**

How fast the offline provider pretends to think.

**`fake_reasoning`**

Working-out the offline provider emits before its answer, so the path a
thinking model takes can be driven without one. Empty means none.

**`[[model.fake_script]]`**

What the offline provider does turn by turn instead of echoing, so the paths
only a model takes — a tool call, an approval — can be checked without one.
Each entry is one turn; omitting tool ends the conversation.

## `[model.retry]`

**`max_attempts`**

A server's own Retry-After is honoured exactly, never shortened: asking again
early earns the same refusal. These bound what this code invents when the
server said nothing.

**`budget`**

The most time one request may spend waiting across all its attempts. A server
asking for longer than this ends the request with a reason instead.

## `[model.ollama]`

For Ollama Cloud, set base_url to "https://ollama.com" and supply a
credential above. A local daemon needs none, including for cloud models it
proxies.

**`keep_alive`**

How long a model stays resident after a request. Empty leaves the server's
default. "30m", "1h", or "0" to unload at once.

**`num_ctx`**

Load the model with this much context. Worth setting: Ollama otherwise sizes
it against free memory, and a model trained for 128k is commonly given 4k —
which is then what the whole session is planned against.

**`think`**

Ask for the model's reasoning separately. Empty follows the model, asking
only when it reports being able to, because asking one that cannot is an
error rather than something ignored. "off", "on", "low", "medium", "high",
"max". Reasoning never reaches a chat channel either way.

## `[model.openai_compat]`

**`base_url`**

The address the chat path hangs off, usually ending in /v1. No default.

**`profile`**

What this server does differently from the protocol it claims. Named rather
than guessed, because a proxy makes the address say nothing.
generic, vllm, lmstudio, llamacpp, openrouter, groq, together.

**`name`**

What to call this endpoint in logs. Empty uses the profile name.

## `[context]`

**`window`**

The model's window, in tokens. Zero asks the provider, which is right
whenever it knows; set it for a server that does not say.

**`compact_at`**

When to fold older turns into a summary, and how much to keep as it was.

**`keep_after_fold`**

How many events before a summary are kept anyway.

Once turns are folded, the conversation sent to the model reads the summary
and not the turns behind it, so they are discarded — that is what stops the
log growing forever. This margin keeps the tail of what was folded readable
for somebody asking what actually happened.

A negative number keeps everything, for a deployment that wants a complete
audit log and will manage the size itself.

## `[memory]`

**`enabled`**

What the agent carries between sessions.

On. What is gated is authority, not writing: a retrieval memory is written
without asking, because it changes nothing until something looks it up, and
an approval that fires on every note is one people learn to click through. A
standing memory goes in front of the model on every future turn, and that
one stops for a person in every profile.

What arrived from outside this machine stays marked as such, is labelled
when it is recalled, and never becomes a standing instruction however it was
approved.

"agent memory list" is how you see everything it believes and where each
belief came from; "agent memory forget" removes one.

**`max_instruction_bytes`**

The ceiling on standing directions injected every turn.

**`expand_queries`**

Try other words when a lookup matches nothing.

Memory is searched by word, and the way that fails is quiet: "do not build a
second component that already exists" and "should I add a new modal?" are the
same subject with no word in common, so the search returns nothing and says
nothing about what it did not look for. When a search comes back empty, the
model is asked for other words the same note might have been written in and
the search is run once more. Results found that way are labelled, because
they answer a question near the one that was asked rather than that one.

The cost is one small model call, only ever on a search that already failed.
Turn it off to keep memory lookups from reaching the provider at all.

Nothing expires by being unused. A memory nobody has recalled for months is
not evidence of anything: the production namespace of a service nobody has
deployed since spring is correct, important and cold. What ends a memory is
a correction, an end date it was given, or somebody removing it.

## `[web]`

**`enabled`**

Whether the agent may read web pages. Reading only: no clicking, typing,
signing in or submitting.

Local sessions fetch unattended. Channels stop for the operator, because
there the address is chosen by somebody else.

**`backend`**

"browser" drives a real browser, which reaches the growing number of sites
that answer anything else with a challenge page. Needs Python and the
cloakbrowser package. "none" disables fetching.

Both halves are checked when the daemon starts, not when the model first asks
for a page: the interpreter has to be there, and it has to be able to import
the package. An interpreter without the package used to pass, and what that
produced was a daemon that came up, validated everything, and failed on every
fetch — which from a chat room is indistinguishable from a model that will not
use the tool.

## `[web.search]`

**`backend`**

Finding an address rather than reading one somebody named. Its own switch,
because it is a different thing to be trusted with: reading fetches what
somebody chose, and searching lets the agent choose.

"brave" or "none". One backend is implemented rather than four written from
four sets of documentation; the next is added when somebody has a key to try
it with.

**`key_env`**

Where the subscription token comes from. The file must be mode 600, and the
value never reaches a log.

**`endpoint`**

The API root, for a deployment behind a proxy. Empty uses the service's own.

**`max_results`**

How many results one search returns by default.

## `[tools]`

**`read_limit`**

Bounds on what one tool call may read, write or return. A tool that can fill
the context window in one call can end a session in one call.

## `[artifacts]`

**`dir`**

Where output too large to show is kept, content-addressed. Empty uses the
data directory.

## `[mcp]`

**`start_timeout`**

Tool servers speaking the Model Context Protocol, run as child processes.

What a server says about itself decides what the model is told a tool does.
It does not decide what the tool may do: that is the level below, and it
lives here rather than in the server.

**`name`**

Prefixes this server's tools, so installing one cannot shadow a built-in.

**`command`**

Either a command run as a child of this daemon, or a Streamable HTTP url
reaching one already running. Never both.

A url server is not started, stopped or killed by this daemon, and every
tool call is a network hop to that address.

**`env`**

The child gets nothing from this daemon's environment except what is named
here. The daemon holds the provider credentials.

**`level`**

What its tools are gated at: internal, workspace_read, network_read,
workspace_write, remember, execute, high_impact. Anything a server can do to
a machine deserves execute unless somebody has read it.

Left unset it is execute — and execute is refused outright by the gateway
and console profiles, not asked about. A server left at the default can be
called from a local console and from nowhere else, which from a chat channel
reads as the tool not existing rather than as a permission. A server that
only transforms text it is given — a linter, a formatter — has been read, and
workspace_read or internal is the honest level; set it, and the tool is
reachable from chat.

A server's tools are found when the daemon starts, so adding one is a restart
away. The model sees them as `mcp_<name>_<tool>`.

## `[process]`

**`buffer_bytes`**

How much output is kept per long-running process, oldest first to go. A dev
server left running overnight would otherwise be a program whose log is the
whole of memory.

## `[delivery]`

**`text_flush_bytes`**

How often a streaming answer is flushed onward. Every delta becoming a
message is a rate limit rather than a feature.

## `[workspace]`

**`root`**

What the agent may touch. Every path is resolved inside it, symlinks
included, and anything outside is refused.

Left unset it is the workspace folder inside .JingClaw, or the working
directory when there is no .JingClaw. Point it at your project to work on
that instead.

## `[server]`

**`addr`**

Loopback only. This API can run programs, so an address reachable from
elsewhere is refused however it is written. Port 0 takes a free one and
publishes it in the discovery file.

**`web_console`**

The embedded console, served from this same address.

**`pairing_ttl`**

How long a printed pairing code stays valid.

**`console_ttl`**

How long a paired browser stays paired without being used. Counted from last
use, so a console somebody works in all day does not stop in the afternoon.
Zero means as long as the daemon runs.

**`data_dir`**

Empty uses the platform's own locations.

## `[gateway]`

**`platform`**

Read by gatewayd. One of "discord" or "telegram"; the section below matching
the one chosen is the one that is read.

**`working_interval`**

The least time between "what it is doing now" lines, and between versions of
an answer still being written. Here rather than under a platform: what these
pace is the projector, which does not know which platform will carry them.

Everything else about a platform lives in that platform's own section,
including which rooms it serves. A channel id, a guild id, a user id and a
role id all belong to one service, and a single shared list would let a file
written for Discord be read by a Telegram daemon.

## `[gateway.discord]`

**`token_env`**

Where the bot token comes from. The file must be mode 600, and the value
never reaches a log.

**`account_id`**

Names this bot within JingClaw, so bindings and the delivery queue belong to
it. A second bot means a second account_id.

**`max_messages`**

How many messages one answer may become before it goes as a file instead.

**`max_attachment_bytes`**

The upload ceiling, well under what Discord accepts.

The channels this agent answers in, applied when the daemon starts.
Declaring one that exists updates it.

Removing one does not unbind it: a daemon started once against an incomplete
file would take away the thing that decides who can reach the agent. The
startup log names every bound channel this file does not.

Which list a channel is in decides its powers. Neither can run programs.
Channel permissions settle who is in the room, which is what makes the rest
reasonable; they cannot settle whether an account still belongs to its owner,
and a stolen one holds the request and the approval both.

**`[[gateway.discord.channels]]`**

Rooms other people can type in. Reading is unattended; changes, memory and
web pages stop and ask.

**`channel_ids`**

Several channels may share an entry when they share their rules.

**`users`**

Both empty permits nobody, which is the right default for a room.

**`approvers`**

Who may answer an approval here, by pressing a button on the message.
A separate list because asking for something and permitting it are
different powers. Both empty means nobody, and the message then says to go
and decide it elsewhere. Typing is never enough in a shared room however
these are set: a message says which account posted it, and a button press
is delivered with the presser's own identity attached.

**`[[gateway.discord.consoles]]`**

Private channels you control. As above, and additionally: pages are fetched
unattended, and an approval can be answered by replying "approve <id>" or
"deny <id>". The channel is told what it is the first time it is used.

## `[gateway.telegram]`

**`token_env`**

Where the bot token comes from. The file must be mode 600, and the value
never reaches a log.

**`max_upload_bytes`**

The upload ceiling, well under what Telegram accepts.

**`api_base`**

The API root. Empty is Telegram itself; set it to reach a local proxy.

**`account_id`**

The same three settings Discord has, because they name Telegram's own ids.
