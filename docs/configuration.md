# Configuration

[`config.example.toml`](config.example.toml) is a starter, not a reference.
Every setting appears in it at its default, and carries a comment only where
deleting that comment would make somebody editing the line more likely to get
it wrong. The reasons live here instead.

The daemon reports every unusable setting at startup by name, value, why it
will not do, and how to fix it. That is where a wrong value gets explained;
this is where a setting's reasons are.

## Where the settings are, and what writes them

Three documents hold everything the daemon, the CLI and the gateway are set
by, and they are rows in the daemon's database, not files: `config`, the TOML
this page is about; `persona`, who the agent is; and `agents`, how a project
wants to be worked on, in the AGENTS.md convention other tools read too. The
database is `data/jingclaw.db` in the deployment directory, which is
`~/.jingclaw` unless `JINGCLAW_HOME` names another.

The dashboard is the only editor. `jingclaw dashboard` prints a link and opens
it: a form for each section of `config`, the whole text of each document, and
the versions each has had. [`using-it.md`](using-it.md#the-dashboard) has the
tour. There is no file to find and nothing to point the daemon at one:
`--config` is gone. Flags exist so a single run can differ from the settings,
not so that a deployment has to be described on a command line.

The first start saves `config` with every setting at its default and nearly
every setting commented. The few live lines state the default itself, so its
arrival changes nothing until a line is deliberately changed or uncommented.
[`config.example.toml`](config.example.toml) is that text,
checked in so it can be read without running anything, and `jingclaw daemon
--print-config` prints it. `persona` and `agents` start empty. The start says
what it did:

```
$ jingclaw daemon
JingClaw daemon
Listening: http://127.0.0.1:54832
Settings:  database (created, all defaults)
```

`Settings:` reads `created, all defaults`, `created from JINGCLAW_CONFIG` or
`imported from config.toml` on the start that made the settings, and otherwise
how many documents the database holds.

Precedence runs defaults → saved text → `JINGCLAW_`-prefixed environment →
flags, so `JINGCLAW_PROVIDER_BACKEND` overrides `provider.backend`, and
`--provider` overrides both. Only a flag actually typed counts; an unset one
does not quietly replace a configured value with a default. Only a setting
whose key has one dot, such as `provider.backend` or `memory.enabled`, can be
given by a variable, because the first underscore is the one that stands for
the dot: `provider.gemini.model` has no spelling. The dashboard marks a setting
that a variable or a flag decides, and does not offer to change it, because
changing the saved text would do nothing.

### Read once, at start

Settings are read when the daemon starts and not again: `config`, `persona`
and `agents` alike. A change saved while it runs is saved and not running yet,
and the page says so for each document whose saved text differs from the text
the running process started with. It takes effect after `jingclaw stop`, then
`jingclaw`. A restart also ends the browser's access to the dashboard, which
the daemon holds and which ends with it; `jingclaw dashboard` gives a new link.

### Saving, and versions

A save from the dashboard is checked the way the daemon checks the settings at
start, and a text that fails is not saved: the page shows each problem with the
setting and what to write instead. So is a text that sets `server.data_dir` or
`server.runtime_dir`, which are not settings in the database (below). The check
is of the settings alone. A missing key, a model the provider does not serve,
the sandbox on a machine that cannot hold it and a missing package are found
when the daemon starts, which then comes up in repair mode (below). A form edit changes only the lines it concerns: comments,
order and settings this version does not know are left as they are. A document
is at most 1 MiB. A save based on a version that has since been replaced is
refused rather than written over.

Every save that changes a document is a version, and each document keeps its
last 20. The page lists them with when each was saved and where it came from
(the first start, or a save in the dashboard), shows how one differs from the
saved text, and restores one as a new version after the same check, so history
is never rewritten.

A version is the whole text. The `headers` and `env` of a tool server are plain
text in `config` and the page shows them whole, so a key typed there stays in
every version saved while it was there, until 20 later saves push those out.
Taking it out of the saved text does not take it out of the old versions.
`pass_env` names a variable of the daemon's own environment and keeps the value
out of the settings. The database file is created readable by its owner alone,
for the same reason.

### Seeds, for a first start

Three variables carry a whole document rather than one setting, for deployments
whose only inputs are a volume and an environment: `JINGCLAW_CONFIG`,
`JINGCLAW_PERSONA` and `JINGCLAW_AGENTS`. A value prefixed `base64:` is decoded
first, so a multi-line document survives a platform's single-line form.

They apply on a first start only, one document at a time: a document with no
saved version takes its first text from its variable, and one with a saved
version never looks at it. The variable is set on the service and arrives again
on every restart, so saving it again would discard what was edited since. A
document somebody emptied has a saved version, an empty one, and is not refilled
either.

All or nothing. A value that does not decode, a `JINGCLAW_CONFIG` that is not
TOML, or one that fails the check a save gets: the daemon says which and why and
stops with nothing saved, so a corrected variable on the next start is still a
first start. [`container.md`](container.md) has the whole arrangement.

### Settings from an older version

Before the settings moved into the database they were `config.toml`,
`AGENTS.md` and `PERSONA.md` in the deployment directory. The first start that
finds a document with no saved version imports its file as it is, unchecked, so
that a file which would not have started is still imported and corrected where
the daemon can be reached (see repair mode below). Then it renames the file
`<name>.imported`. A file wins over a variable for the same document. Nothing is
deleted, and `config.toml.bak.*` files are left alone. A file edited since it
was imported, one whose `.imported` name is taken and one on a read-only mount
stay where they are with a warning in the log; the settings are in the database
either way. Once a document has a saved version, its old file is not read again.

A `config.toml` that sets `server.data_dir` or `server.runtime_dir` to somewhere
other than where this process puts them is not imported: the daemon refuses to
start and names the variable to set. Starting anyway would open an empty
database in the default place, and one deployment would quietly become two.

### What is kept outside the settings

`server.data_dir` and `server.runtime_dir` say where the database and the
discovery file (how a client finds the daemon) are, so they cannot live in the
database: the daemon would need the answer to find the question. They are given
when it starts, as `--data-dir` or `JINGCLAW_SERVER_DATA_DIR` and as
`JINGCLAW_SERVER_RUNTIME_DIR`, which the CLI and the gateway read as well to
find the daemon. The defaults are `data/` and `run/` in the deployment
directory. A line for either in the saved text is ignored. `jingclaw daemon
--print-paths` says where everything is without creating anything.

The workspace is not a setting either. The agent works in `workspace/` in the
deployment directory, never in the directory it was started from: every path is
resolved inside it, symlinks included, and anything outside is refused.

### When the settings cannot be used

If the saved text does not read, fails the check, or the agent cannot be built
from it and the environment (no key for the provider, a key file that others can
read, a model the provider does not serve, the sandbox on a machine that cannot
hold it), the daemon does not exit. It comes up in repair mode: the dashboard
and the services needed to put the problem right (the settings, the credentials
and the pairing of a browser), on loopback whatever `server.addr` says, with no
agent, no gateway and no tools, so nothing in it can act. It says why where it
starts, in `jingclaw dashboard` before it prints the link and on the first
screen of the page, and `jingclaw status` shows the mode. Fix it there, then
`jingclaw stop` and `jingclaw`: there is no restart from the page. A database
that will not open, or a `JINGCLAW_CONFIG` that cannot be used, still ends the
start.

### Keys and skills are still files

Two things are not documents: credential files, and skills. They are secrets and
fetched directories, and stay files in the deployment directory; the dashboard
manages them. A credential is a variable that a `*_env` setting names or a file
that a `*_file` setting names, the variable first. For each, the page says where
it would come from and what is in the way (a file that others can read is not
used), writes a key into its file readable by its owner alone, removes it, or
tightens a file's mode. It is write-only: no reply carries a key, its length, a
prefix or a hash. A write replaces the file with no backup, and takes effect
when the process that reads it next starts, which the page says. Skills are
staged, read in full and then activated or removed from the page; `jingclaw
skills` still works.

### The gateway

The gateway reads no file. It asks the running daemon for the part of the
settings it needs (which platform, the log level, that platform's token settings
and limits, and not its channels, users or approvers), so the daemon has to be
running first. A gateway that cannot start for a reason in the settings, such as
a missing token, stops with the reason; the supervisor prints `chat is off:
<reason>` and keeps the daemon and the dashboard up. `jingclaw mcp list`,
`login` and `logout` ask the daemon in the same way for the tool servers in
`config`, and need it running.

### In a container

The dashboard cannot be reached from inside a container yet: the daemon listens
on loopback in the container, and nothing publishes that. A container is set up
through the seeds, on its first start only. After that its settings are the
database in the volume, and changing a variable changes nothing.

### What the agent says it is

Who the agent is and how it works are the `persona` and `agents` documents, and
not settings: a setting that said the same thing would be a second place, and a
second place is one somebody edits while the first is what runs. They are put in
front of the model, `agents` first.

The prompt is assembled in layers, each with a stated source, and
`--print-prompt` shows the whole thing with where every part came from. One
layer is deliberately not configurable: the contract that tells the model how
tools behave and what a refusal means. Letting an operator edit that would let
them describe a system that does not exist.

### Checked at startup

Settings are checked at startup rather than at the moment they matter, and all
of them at once — restarting a daemon to discover the next mistake is work the
program can do in one pass:

```
Configuration problem in the saved settings

  server.addr = "0.0.0.0:9977"
      is not a loopback address
      This API can run programs and is not safe to expose. Use 127.0.0.1, ::1 or localhost.

  provider.backend = "openai"
      is not a provider that exists
      Use "gemini", "anthropic", "ollama", "openai_compat", or "fake" for the offline provider.
```

The first of those refusals is not a preference. This API reads files and runs
programs; binding it somewhere reachable needs a more deliberate mechanism than
a config line, and there is not one.

---

## `[agent]`

**`permission_profile`**

What runs may do without asking. "local" is for somebody at this machine.
"gateway" is the stricter profile channels get, and naming it here applies it to
this machine too: even a local run then refuses to execute programs, which is a
reasonable choice on a shared machine. Channels get a profile of their own
whatever this says, and it is not settable here.

**`max_iterations`**

How many model turns one run may take before it gives up.

There is no name, persona, instructions or instruction files setting: who the
agent is and how it works are the `persona` and `agents` documents. persona
shapes how it writes; agents is added to every prompt. Neither is a permission:
what the agent may do is decided by the policy engine, which reads none of this.

## `[provider]`

**`backend`**

Which model server to talk to.

"gemini"         Google's API. Needs a key.
"anthropic"      Anthropic's Messages API. Needs a key.
"ollama"         A daemon on this machine, or Ollama Cloud. See below.
"openai_compat"  vLLM, LM Studio, llama.cpp, OpenRouter, Groq, Together.
"fake"           No network, no credential. What proves a problem is not
the model.

"fake" is the default, so that a fresh install starts and does nothing
surprising; set this before expecting real work. Each backend has a section of
its own for its model and its credential, so switching between them is this one
line, and the model each was set up with is still there when you switch back.

**`model`**

In each backend's section, `provider.<backend>.model`: named as the chosen
provider names it, "gemma-4-31b-it", "qwen3:8b", whatever /v1/models lists.
Empty is allowed only when the provider serves exactly one model; otherwise the
daemon names them and starts no agent (repair mode), and `--list-models` prints
what the provider serves.

**`fake_delay`**

How fast the offline provider pretends to think.

**`fake_reasoning`**

Working-out the offline provider emits before its answer, so the path a
thinking model takes can be driven without one. Empty means none.

**`[[provider.fake_script]]`**

What the offline provider does turn by turn instead of echoing, so the paths
only a model takes — a tool call, an approval — can be checked without one.
Each entry is one turn; omitting tool ends the conversation.

## `[provider.retry]`

**`max_attempts`**

A server's own Retry-After is honoured exactly, never shortened: asking again
early earns the same refusal. These bound what this code invents when the
server said nothing.

**`budget`**

The most time one request may spend waiting across all its attempts. A server
asking for longer than this ends the request with a reason instead.

## `[provider.gemini]`

Google's API. Its model and its credential are here; each backend that takes a
credential has the same two, and so do `[memory.embedding]` and, as `key_env`
and `key_file`, `[web.search]` and, as `token_env` and `token_file`, a gateway.

**`api_key_env`**

Where the credential comes from, environment first: `GEMINI_API_KEY`, then
`GOOGLE_API_KEY`. Never logged, never passed to a program the agent runs. It
lists the names of variables (letters, digits and underscores, not starting with
a digit), never the key: an entry that is not such a name does not start the
agent, and is not printed back, because it is more often a key pasted into the
wrong place than a typing mistake.

**`api_key_file`**

Where the credential comes from when the environment has none: the name of a
file in the deployment directory, `gemini.key` unless set, mode 600. Every
setting that names a credential file (`api_key_file`, `key_file` for web search,
`token_file` for a gateway) holds a plain name and never a path: up to 64
letters, digits, `.`, `_` or `-`, starting with a letter or digit and not ending
in `.`. It cannot be a name the deployment keeps for itself (`config.toml`,
`AGENTS.md`, `PERSONA.md`, `workspace`, `data`, `run`, `log`, `bin`, `skills`,
`mcp-auth`, `sandbox`), a backup name (`*.bak.*`) or the name a settings file was
given when it was imported (`*.imported`). Settings that name anything else do
not start the agent, and the daemon says which setting, without repeating what
it holds, and what is allowed. A name with a directory part, such as
`keys/gemini.key` or `../gemini.key`, used to be joined onto the deployment
directory and read; it is now one of those. Two credentials may still name one
file, as a provider and its embeddings on the same endpoint may share a key;
the dashboard will not save to or delete a file that two settings name, because
that would change both.

## `[provider.anthropic]`

Anthropic's Messages API. Its own backend rather than a dialect of the
OpenAI-compatible one: the wire format differs in kind, not in detail.

Unlike Gemini there is no default variable and no default file, and, as with
Gemini, no default model. The example shows `claude-sonnet-4-5` and `ANTHROPIC_API_KEY` as values to
start from; they are used only once they are written into the settings.

**`base_url`**

For a gateway or a proxy in front of the service. Empty is Anthropic's own
address.

## `[provider.ollama]`

For Ollama Cloud, set base_url to "https://ollama.com" and supply a credential
(`api_key_env` or `api_key_file`). A model server on this machine needs none,
including for cloud models it proxies, and having none is not an error.

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

## `[provider.openai_compat]`

The credential is optional here too: a server on this machine usually needs
none.

**`base_url`**

The address the chat path hangs off, usually ending in /v1. No default, and
required with this backend.

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

`jingclaw memory list` is how you see everything it believes and where each
belief came from; `jingclaw memory forget` removes one. The dashboard has the
same two on its memory page.

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

**`curate`**

Note what a person said about themselves and the project once they have
been answered.

On. After each answered turn the model is handed the person's own messages
from that turn — nothing the assistant said, nothing a tool returned — and
asked for at most three claims, each with the verbatim words it rests on. A
claim whose words are not in the message it cites is dropped. What survives
is a retrieval memory: looked up when wanted, never carried, never an
instruction, and marked as approved by nobody. One small model call per
answered turn; off, and memory is written only when the model calls
`remember`. A channel is told how many notes a run left, under the line
that accounts for the run.

**`auto_recall`**

How many notes are put in front of the turn being answered.

Chosen by what the turn says, from the scopes its sender may read — the
project's and their own, never another account's — and placed as a labelled
block before their words, on that turn only. `0` puts none and leaves the
`recall` tool as the only way in.

**`auto_recall_bytes`**

The ceiling on that block.

## `[memory.embedding]`

Finding memories by meaning as well as by their words, through any
OpenAI-compatible `/embeddings` endpoint. Off until `model` is set. Every
memory, and every turn a person types, is sent to the endpoint.

**`model`**

The embedding model. Empty leaves search to words. Changing it makes every
stored vector meaningless next to the new ones; memories are embedded again
with the new model when the daemon starts.

**`base_url`**

The root `/embeddings` hangs off, usually ending in `/v1`: Ollama is
`http://127.0.0.1:11434/v1`, OpenAI `https://api.openai.com/v1`, Gemini
`https://generativelanguage.googleapis.com/v1beta/openai`. Required with a
model.

**`api_key_env`**, **`api_key_file`**

Where the key comes from, when the endpoint needs one, the same as a
provider's.

**`min_similarity`**

How alike a memory has to be to a turn, from 0 to 1, to be put in front of it
by meaning alone without anybody asking. `0.56` was measured on
qwen3-embedding:0.6b: the closest note to an unrelated turn never passed
0.553. Another model draws the line elsewhere.

**`recall_min_similarity`**

The same line when the model searches with `recall`, and when notes are
checked for what is already known. Lower, `0.45`, because what comes back is
read and judged rather than put in front of a turn: on the same model the
right note for a question in other words scored from 0.49.

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

**`key_env`**, **`key_file`**

Where the subscription token comes from, the variable first. The file is a plain
name in the deployment directory, `brave-search.token` unless set, and must be
mode 600; the value never reaches a log.

**`endpoint`**

The API root, for a deployment behind a proxy. Empty uses the service's own.

**`max_results`**

How many results one search returns by default.

## `[tools]`

**`read_limit`**

Bounds on what one tool call may read, write or return. A tool that can fill
the context window in one call can end a session in one call.

## `[artifacts]`

Where output too large to show is kept, content-addressed, in `artifacts/`
beside the database. Where is not a setting.

**`max_bytes`**

How large one piece of stored output may be. Something larger is a file that
belongs in the workspace, not output that was captured.

**`max_image_bytes`**

How large one image put in front of the model may be. Something larger is
described in words instead: a provider refuses a request that is too big, and a
refused request is worse than a picture nobody saw.

## `[sandbox]`

**`enabled`**

Confines every command `exec_command` runs: it may write to the workspace and
to the deployment's own caches, and nowhere else. Approving a command says
somebody meant to run it; it cannot say what a build's dependencies will do.

Off by default, because it changes what commands can do. On, it is not
advisory: a machine that cannot confine starts no agent, and the daemon comes up
in repair mode and says why, rather than running commands that only look
protected.

macOS only so far, through `sandbox-exec`. On Linux, turning it on leaves the
daemon without an agent, and it says why: the deployment's own directory is
always hidden from a command, and Landlock grants access rather than removing
it, so it cannot hide one directory while everything else stays readable. The
Linux backend is in `internal/sandbox` and accepts only a policy that hides
nothing; it becomes usable when the policy can be written as a list of what is
readable.

**`network`**

Lets confined commands open connections. Off by default, because a build or a
test is most of what an agent runs and neither needs one. One setting rather
than an exception per command: a list of programs allowed onto the network
turns, one entry at a time, into a network anybody may reach.

**`hidden`**

Directories a confined command may not read, written with a leading `~` if
you like. The deployment's own directory is always hidden and need not be
listed. Confining writes says nothing about reads: a command that cannot write
outside the workspace can still open `~/.ssh`, and having no network only means
it cannot send what it found today.

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

**`headers`**

Sent with every request to a url server, which is where an authorization
header goes. The value is kept in the saved settings as it was typed, and in the
versions saved while it was there; see "Saving, and versions" above.

**`env`**

The child gets nothing from this daemon's environment except what is named
here, and `pass_env` names variables to forward. The daemon holds the provider
credentials. A value written in `env` is kept in the saved settings as it was
typed, like a header; `pass_env` keeps the value out of them.

**`level`**

What its tools are gated at: internal, workspace_read, workspace_write,
remember, execute, high_impact. Anything a server can do to a machine deserves
execute unless somebody has read it.

Left unset it is execute — and execute is refused outright by the gateway
and console profiles, not asked about. A server left at the default can be
called from a local console and from nowhere else, which from a chat channel
reads as the tool not existing rather than as a permission. A server that
only transforms text it is given — a linter, a formatter — has been read, and
workspace_read or internal is the honest level; set it, and the tool is
reachable from chat.

A server's tools are found when the daemon starts, so adding one is a restart
away. The model sees them as `mcp_<name>_<tool>`.

**`oauth`**

Says a url server is behind an authorization server, so somebody signs in once
with `jingclaw mcp login <name>`. The daemon never opens a browser itself:
nobody is watching it, so a session that has run out is reported rather than
blocking a run on a page no one will see. `jingclaw mcp list` says which servers
need one, `login` stores a session and `logout` forgets one. All three ask the
running daemon for the servers saved in `config`, so it has to be running; what
they use is what is saved, which a restart makes what is running.

**`defer`**

Keeps this server's tools out of the prompt until the model asks for one.
Every declared tool costs every request its name, description and schema,
and one server can carry forty; with several, the model reads a hundred
schemas a turn and chooses among them worse. Deferred, the prompt names the
server in one line — what it is called, how many tools, what they are gated
at — and the model reaches a tool through `tool_search`, which returns names
and descriptions, and `tool_load`, which returns the schema and makes the
tool callable for the rest of the session.

Off by default: it is about cost, and a deployment with one small server has
none to save. What is loaded is read from the session's log, not held in
memory, so a daemon restarted mid-session declares the same tools.
Deferring changes nothing about permission: a loaded tool is gated at the
server's level as if it had been listed from the start.

## `[process]`

**`buffer_bytes`**

How much output is kept per long-running process, oldest first to go. A dev
server left running overnight would otherwise be a program whose log is the
whole of memory.

## `[delivery]`

**`text_flush_bytes`**

How often a streaming answer is flushed onward. Every delta becoming a
message is a rate limit rather than a feature.

## `[server]`

**`addr`**

Loopback only. This API can run programs, so an address reachable from
elsewhere is refused however it is written. Port 0 takes a free one and
publishes it in the discovery file.

**`log_level`**

debug, info, warn or error. A key or a token never reaches a log at any level.

**`data_dir`**, **`runtime_dir`**

Not settings in the database: they say where the database and the discovery file
are. They are given when the daemon starts, and a line for either in the saved
text is ignored. See above.

## `[gateway]`

**`platform`**

One of "discord" or "telegram"; the section below matching the one chosen is
the one that is used. The gateway is told it by the daemon.

**`working_interval`**

The least time between "what it is doing now" lines. Here rather than under a
platform, as is `stream_interval`: what these pace is the projector, which does
not know which platform will carry them.

**`stream_interval`**

The least time between versions of an answer that is still being written. An
answer finished inside one interval never streams: it arrives whole, which is
what it should do rather than appearing and being rewritten a moment later.

Everything else about a platform lives in that platform's own section,
including which rooms it serves. A channel id, a guild id, a user id and a
role id all belong to one service, and a single shared list would let settings
written for Discord be read by a Telegram gateway.

## `[gateway.discord]`

**`token_env`**, **`token_file`**

Where the bot token comes from, the variable first (`DISCORD_BOT_TOKEN`). The
file is a plain name in the deployment directory, `discord.token` unless set,
and must be mode 600; the value never reaches a log. [`discord.md`](discord.md)
has the steps.

**`account_id`**

Names this bot within JingClaw, so bindings and the delivery queue belong to
it. A second bot means a second account_id.

**`max_messages`**

How many messages one answer may be split into before it goes as a file
instead. Ten by default.

**`max_attachment_bytes`**

The upload ceiling, well under what Discord accepts.

**`tables_as_images`**

Draws a table as a picture instead of writing it out in a code block. Off,
because it changes the shape of an answer rather than its appearance: a picture
cannot sit inside a message next to text, so an answer with a table in it
arrives as several messages, and a picture cannot be selected or searched. What
it buys is a table that is aligned, because the layout is decided here and not
by whatever font the reader's client uses.

The channels this agent answers in, applied when the daemon starts.
Declaring one that exists updates it.

Removing one does not unbind it: a daemon started once against incomplete
settings would take away the thing that decides who can reach the agent. The
startup log names every bound channel the settings do not, when the settings
declare any channel at all. Bindings are also made in the dashboard and with
`jingclaw bindings add`; those are in the database, take effect at once, and
are not in the settings, so a channel the settings still declare comes back at
the next start however it was removed.

Which list a channel is in decides its powers. Neither can run programs.
Channel permissions settle who is in the room, which is what makes the rest
reasonable; they cannot settle whether an account still belongs to its owner,
and a stolen one holds the request and the approval both.

**`[[gateway.discord.channels]]`**

Rooms other people can type in. Reading is unattended; changes, memory and
web pages stop and ask.

**`channel_ids`**

Several channels may share an entry when they share their rules.

**`users`**, **`roles`**

Who may ask the agent for something, by platform user id or role id.

Leaving both empty opens the channel to everyone the platform lets into it.
The channel already has a membership, set up by whoever wrote this entry, and
a second list here would have to be kept in step with it; the way that fails
is somebody being added to the room and silently ignored. Whoever gets in
still runs under the gateway profile, and bots are refused whatever these say.
The startup log marks such a channel with `who_may_speak=anyone the platform
lets in`, so a widening by accident shows before anyone turns up. To restrict
a channel, name the people or roles.

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

A console is also the terminal console, for somebody not at the machine. It
mirrors every run's feed — the message that started it, the run, each tool
going out and coming back with what it printed, the answer — for a run in a
public room as much as one in the console itself, drawn by the same code that
draws the terminal so the two never disagree. The public room sees none of
it. Running a program from a console is still refused: the feed shows the
call going out with its arguments, which is what a console is for, and never
a call that ran.

## `[gateway.telegram]`

**`token_env`**, **`token_file`**

Where the bot token comes from, the variable first (`TELEGRAM_BOT_TOKEN`). The
file is a plain name in the deployment directory, `telegram.token` unless set,
and must be mode 600; the value never reaches a log.

**`max_upload_bytes`**

The upload ceiling, well under what Telegram accepts.

**`api_base`**

The API root. Empty is Telegram itself; set it to reach a local proxy.

**`account_id`**

The same three settings Discord has, because they name Telegram's own ids.
