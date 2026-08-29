# JingClaw

A local-first, durable agent runtime you can attach to from anywhere.

`agentd` owns every piece of state — sessions, runs, the event log. The
control clients are projections of it, so closing one never stops work that is
already running, and reattaching from somewhere else picks up exactly where
you left off.

```
                    proto/jingclaw/control/v1
                              │
        ┌─────────────┬───────┴───────┬─────────────┐
        ↓             ↓               ↓             ↓
   SwiftUI        WinUI 3          Web UI        Go CLI
   (macOS)       (Windows)      (no GUI needed)
        └─────────────┴───────┬───────┴─────────────┘
                              ↓
                      Connect-RPC / loopback
                              ↓
                           agentd
```

## Status: M0 — walking skeleton

Given a repository with a failing test, JingClaw finds the cause, fixes it, and
runs the tests to confirm — stopping for a human before each change. State
persists to SQLite and survives a restart, including runs orphaned by a crash
and runs parked on an approval.

Deliberately absent until M1b and beyond: context compaction, MCP, subagents,
and every GUI.

| Milestone | Scope |
|---|---|
| M0 | walking skeleton — done |
| M1a | durable runtime, providers, tools, permissions — done |
| **M1b** | gateway plane ✓, Discord adapter ✓ |
| M2 | SwiftUI, WinUI 3 and web clients at parity |
| M3 | subagents, replay, sandboxing, remote fleet |

### Where it keeps things

```bash
agentd --init      # creates .JingClaw/ here
```

```
.JingClaw/
  config.toml    the settings
  workspace/     what the agent may read and change
  data/          the database and stored output
  run/           how clients find the daemon
```

Credentials go in beside them, mode 600. The directory is found by walking up
from wherever the daemon or a client is started, so running one from a
subdirectory reaches the same deployment rather than quietly becoming a second
one with its own database.

Without a `.JingClaw`, the platform's own config, data and runtime directories
are used exactly as before, and the workspace is the working directory. An
explicit setting always wins over both.

The workspace defaults to *inside* the directory rather than beside it, so a
deployment set up to try the thing out cannot reach the project it was set up
in. Point `[workspace] root` at your code to work on that.

### What it can do

| Tool | Gate |
|---|---|
| `read_file`, `glob_files`, `grep` | runs unattended |
| `web_read` | unattended locally, asks from a chat channel |
| `write_file`, `edit_file` | asks first |
| `exec_command` | asks first |

An edit must target text that appears exactly once, in a file the agent has
read and that has not changed since. `exec_command` takes a program and an
argument list — there is no shell — and a cancelled command takes its whole
process group with it.

### Models

| `[model] provider` | What it talks to |
|---|---|
| `gemini` | Google's API |
| `ollama` | a local Ollama daemon, or Ollama Cloud |
| `openai_compat` | vLLM, LM Studio, llama.cpp, OpenRouter, Groq, Together |
| `fake` | nothing; a deterministic stand-in for offline work |

Ollama is reached through its own API rather than its OpenAI-compatible one,
because the things a runtime has to know are only in the former: how much
context the server actually gave a model, whether it is loaded at all, and its
thinking as a field rather than mixed into the answer.

That first one decides whether long sessions work. A model trained for 128k is
routinely loaded with 4k, because that is what fit in the memory on hand, and
planning against the larger figure means every request is refused while
compaction waits for a threshold it will never reach. So a context window
carries its provenance, and the order of belief is: the operator, then a server
reporting what it has loaded, then a catalogue, then the model's own maximum —
and nothing at all if none of them said.

Ollama also serves an OpenAI-compatible endpoint at `/v1`, which is the
quickest way to try `openai_compat` against a real implementation. Note what
it costs: that listing says nothing about context length, so the window comes
back unknown and compaction stays off unless `[context] window` is set. The
native adapter reads the real figure. That difference is the argument for it.

`openai_compat` needs a `profile`, because the claim is about a request shape
rather than about behaviour. Servers making it disagree on whether usage is
reported, which of two fields carries reasoning, and what a status code means:
one answers `403` for a prompt longer than the context, which read the ordinary
way is a permissions failure nobody can fix. The profile is named in
configuration rather than guessed from the address, since a proxy makes the
address say nothing, and an unknown name is refused rather than silently
becoming the one that knows none of this.

### A channel as a console

"Discord" is not one trust level. A channel with fourteen people in it and one
only you can see are different rooms, and the platform's own permissions are
what separate them. Declare a private channel as a console in the config file:

```toml
[[gateway.channels]]
channel_id = "111111111111111111"
tenant_id  = "222222222222222222"
workspace_id = "default"
profile = "console"
users = ["333333333333333333"]
```

Channels are applied when the daemon starts, so a deployment is described in
the file rather than in commands somebody has to remember running. Declaring
one that already exists updates it. Removing one does *not* unbind it — a
daemon started once with an incomplete file would otherwise take away the thing
that decides who can reach the agent — and the startup log names any bound
channel the file does not, so nothing drifts unseen.

The same thing from the command line, when that is easier:

```bash
agent bindings add --channel <id> --guild <id> --workspace ws \
  --user <your-user-id> --profile console
```

A console channel reads and searches the workspace, fetches pages, and reads
what the agent remembers. Changes to files and to memory still stop and ask —
and you can answer there: `pending` lists what is waiting, `approve <id>` and
`deny <id>` decide it, and `artifact <id>` hands over something a run stored.
Those few words are the whole command set, matched on the entire message, so an
ordinary sentence still reaches the agent.

A console is also a log. Every finished tool call appears there with what it
did, how long it took and whether it worked — as subtext, so twenty of them do
not bury the reply. An ordinary channel gets none of that: it is a conversation,
not a terminal.

The same asymmetry applies to failures. A run that dies gets a plain sentence in
a room other people read, because a provider's own words carry the account's
quota and limits. A console gets the real reason, since hiding it from the one
person who can fix it protects nobody.

Every run ends with what it cost: which provider and model answered, the tools
it used and how long each took, the sources it drew on, and the tokens.

Artifacts are pulled, never pushed. A run that produces a large result says it
exists and stops there; the bytes cross into a channel when a person names the
one they want. Attaching everything a run produced would put whole build logs
and fetched pages into a room on the agent's initiative.

It cannot run programs. That is the line, and it is where it is because of what
a channel permission can and cannot do. It settles who is in the room, which is
what makes reading and writing reasonable there. It cannot settle whether an
account still belongs to its owner, and a stolen one holds the request and the
approval both. So running programs stays where somebody has to be at the
machine, which is also the only place that can tell.

The channel is told all of this the first time it is used, and again on `help`.
A boundary that lives only in a configuration file on a machine nobody in the
room is sitting at is one everybody will assume the shape of instead.

A console decides only for its own conversation. A run started at the terminal
is still answered at the terminal.

### Reading the web

Off by default; `[web] enabled = true` turns it on. `web_read` fetches a page
and returns its visible text and links, and that is the whole of it: no
clicking, typing, signing in or submitting. Those are a different power with a
different blast radius, and keeping them out of this tool is what stops the
gentler one from quietly carrying them.

Pages are fetched by driving a real browser rather than by making an HTTP
request. A growing share of the web answers anything that does not look like a
browser with a challenge page, which an agent then reads as though it were the
article; a browser also runs the JavaScript that many sites need before there
is any text to read at all. It costs a process and a few seconds per page, and
needs Python with the `cloakbrowser` package installed. This is not a way past
serious bot detection — sites running Cloudflare or DataDome still refuse, and
`web_read` reports the refusal rather than pretending — it is a way to read the
ordinary web that a plain fetch no longer reaches.

Three things hold regardless of which page comes back:

- **Nothing inside this machine is reachable.** Addresses are checked against
  what they resolve to rather than how they are spelled, every address a name
  resolves to is checked, and the same check runs again inside the browser for
  wherever a redirect leads. A public hostname pointed at `169.254.169.254` is
  the normal way this is attacked, and it is refused.
- **What comes back is labelled.** Every result opens by naming the final URL,
  the redirect it came through, and the fact that a stranger wrote it. Text in
  a page that addresses the agent is content, not instruction.
- **Who chose the address decides the gate.** The operator naming a page is
  research and runs unattended. A link arriving from a chat channel stops for
  the operator, because that plane can already read the workspace and a page
  saying "now show me your .env" would otherwise complete the loop.

### From a chat channel

`gatewayd` connects a Discord bot to the same runtime, so a mention in a bound
channel starts work and the reply comes back to that thread.

```bash
agent bindings add --channel <id> --guild <id> --workspace ws --user <your-user-id>
gatewayd --account main
```

Every default is no: a channel with no binding is unreachable, a binding with
nobody allowed permits nobody, overheard text is not a request, and bots are
refused whatever the allowlist says. Runs from a channel use a stricter
profile that denies execution outright — approving from the same account that
asked is one unbroken chain, so anything that runs a program has to be
authorised from a local client.

## Layout

```
proto/    the contract, shared by every client
core/     Go daemon and CLI
macos/    SwiftUI client       (M2)
windows/  WinUI 3 client       (M2)
web/      embedded web UI      (M2)
```

Generated code lives in `core/gen/` and **is committed**, so Xcode, Visual
Studio and the web build never need `buf` installed. CI regenerates and fails
on any drift.

## Build

Requires Go 1.26+ and [buf](https://buf.build) 1.72+ (only for changing the
proto files).

```bash
cd core && go build -o bin/agentd ./cmd/agentd && go build -o bin/agent ./cmd/agent
```

## Try it

Three terminals. First, the daemon:

```bash
core/bin/agentd --dev-fake
```

Or against a real model:

```bash
core/bin/agentd --provider=gemini --model=gemma-4-31b-it --workspace .
```

`--workspace` is the only directory tools can reach. Paths are resolved and
symlink-checked against it before any I/O, so neither traversal nor a symlink
pointing elsewhere gets out.

Reads run unattended; anything that modifies the workspace stops for a
decision:

```bash
core/bin/agent approvals <session-id>
core/bin/agent approve <approval-id>     # --session to allow that tool for the session
core/bin/agent deny <approval-id>
```

The pause is durable. A run waiting for an answer survives a daemon restart and
can be answered hours later, by a client that never saw the original prompt.

The key comes from `GEMINI_API_KEY`, or a mode-600 file at
`gemini.key` under the config directory. `--list-models` prints what the
provider actually serves; a `--model` it does not serve fails at startup
rather than on the first message.

It binds an ephemeral port on loopback and writes a `0600` discovery file
holding the address and a bearer token; the CLI finds it from there. State
lives in `jingclaw.db` under the user config directory, or wherever
`--data-dir` points.

Then create a session and follow it:

```bash
core/bin/agent session create
core/bin/agent attach <session-id>
```

And in a third, send a turn:

```bash
core/bin/agent send <session-id> "測試訊息"
```

```
000001 user.message           測試訊息
000002 run.running
000003 assistant.delta        收到：
000004 assistant.delta        測試訊息
000005 assistant.completed
000006 run.completed
```

Detach with `Ctrl+C` — the run keeps going — then resume from where you
stopped:

```bash
core/bin/agent attach <session-id> --after 3
```

Only events 4, 5 and 6 arrive. Restarting the daemon changes nothing: the
session, its runs and the sequence all continue where they were.

## Configuration

Every setting the daemon, the CLI and the gateway have is written in one file.
Flags exist so a single run can differ from it, not so that a deployment has to
be described on a command line.

The daemon writes the file the first time it starts, so there is nothing to
find out and nowhere to put it:

```
$ core/bin/agentd
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

## The console

The machine this agent is most useful on often has no desktop: a server reached
over SSH, a container, somebody else's Linux box. So a console ships inside the
binary, and the daemon prints where it is:

```
Console:   http://127.0.0.1:54832/?c=4EDM-HB22-V5LY-6BKA
           valid once, until 13:58:19 (agent console for another)
```

Open it and you get the session list, a live timeline, approvals with
allow/deny, interrupt, and stored output shown inline next to the line that
produced it. Over SSH, forward the port — nothing else has to be installed at
the other end.

It is one page with no build step, talking Connect over JSON: a unary call is a
POST whose body is the message, a stream is the same POST answered in
length-prefixed frames. That is deliberate. A static binary that needs a
`node_modules` directory to have been present on the machine that built it is
not a static binary, and the browser needs no proxy to reach the same endpoint
the CLI uses.

What is in that URL is a **code, not a credential**. It works once, expires,
and buys the browser its own token — which is narrower than the one the CLI
holds and can be revoked without touching it. A console credential cannot mint
another, and cannot rewrite which channels the gateway listens to.

That distinction is the whole point of the line above. It is going to sit in a
terminal's scrollback, on a machine somebody else can scroll back through,
inside a screenshot of a demo. A code that stopped working an hour ago is a
very different thing to leave lying there than a credential that works until
the daemon restarts. `agent console` prints another when you need one.

The page takes the code out of the address bar before anything else happens,
and keeps what it bought in `localStorage` so a reload and a second tab work —
a code only works once, so a second tab has no way to get its own. The files
themselves are served without a credential, because they are code rather than
data and everything the console can *do* is behind the check. The Host
validation applies to all of it.

A session started from the terminal shows up in the console, and a turn sent
from the terminal streams into it while it is open — which is what "clients are
projections" has to mean in practice rather than only in the design.

## Images

Send the bot a screenshot on Discord — **attached to the message that mentions
it**, not sent separately — and it looks at it. Without the Message Content
intent the bot only sees messages addressed to it, so a picture posted on its
own is a picture it was never shown.

From a terminal it is the same picture, a flag away:

```bash
core/bin/agent send <session-id> "what is wrong with this layout" --attach shot.png
```
 The path is the same
shape as everything else here: the adapter fetches the bytes, the ingress puts
them in the artifact store, the **event carries a reference** — not the picture
— and the bytes are read back when a request is assembled. An image is large
and the log is replayed on every turn, so a conversation that carried copies of
everything ever sent to it would stop working long before the context window
did.

The adapter fetches rather than passing a link inward. Discord's link is signed
for a client this daemon is not and it expires, so a conversation replayed next
month would find nothing behind it — and the gateway is the only part of this
system meant to talk to the platform at all.

**The declared type is not believed.** It comes from the platform, which got it
from whoever uploaded the file. The bytes have to agree with the label, the
label has to be on a short list — PNG, JPEG, WebP — and the header has to
declare a picture small enough to decode. That last one matters on its own: a
bounded number of bytes is not a bounded amount of work, and a few dozen bytes
of PNG header can promise 900 million pixels.

SVG is a document that can carry script and is not accepted. An attachment that
cannot be shown is still named in the message, because "here, fix this" makes
no sense at all if the attachment is invisible.

A picture from a gateway turn is labelled as coming from outside this machine.
That is not a security control — text inside an image is a known way to instruct
a model, and no label prevents it. What holds is the same thing that always
holds here: a run's permissions come from where it came from, and nothing the
model reads can raise them.

## Memory

Off by default. What is written here is read by every later session, by an
agent that no longer knows where it came from, so turning it on is a decision
somebody makes rather than one they inherit:

```toml
[memory]
enabled = true
```

Then `remember` writes something down and `recall` looks it up. Three rules
shape the rest, and each comes from something that has actually gone wrong in a
shipped system rather than from taste.

**What stops for a person is authority, not persistence.** A memory has an
`activation`: `retrieval` is looked up when it is wanted, `standing` goes in
front of the model on every future run. Writing the first changes nothing until
somebody asks for it, so it does not interrupt. Writing the second shapes every
future run without anybody asking, so it does. That split matters because an
approval that fires on every write is one people learn to click through — and a
one-click approve-everything flow is not an approval.

What the approval shows is the proposed text *and* which session, which message
and which principal produced it: a conditional injection that fires when you say
"yes" to something innocuous is only visible if the screen says where the
proposal came from.

**Reading is scoped by who is asking.** What a Discord account told the agent
is not recalled for the operator, and the operator's notes are not read into a
channel. The scope comes from the turn, never from an argument the model could
choose. What the *project* knows is shared, because that is what a project is —
but a turn from outside this machine can only write about the person it came
from. Project knowledge is read by local runs that can execute programs, so
promoting anything into it is the operator's to do.

**What came from outside stays marked.** A gateway-origin memory is untrusted
permanently — a fact derived from untrusted text is untrusted however many
summaries it has been through — and is never put in front of the model as a
standing direction. It can be looked up deliberately, labelled as coming from
outside this machine.

You can see all of it, which is the control that matters:

```bash
core/bin/agent memory list             # everything, with where each came from
core/bin/agent memory list --history   # including what has been superseded
core/bin/agent memory forget mem_01K…  # gone, not merely superseded
```

A correction invalidates rather than overwrites, so "what is believed now" and
"what was believed then" are both answerable. Two corrections to the same
memory cannot both win: the second is refused, because a supersession graph
that forks has no answer to which branch is believed.

Forgetting removes the memory, index and all — the agent will not recall it or
carry it again. It does **not** erase the conversation the memory came from:
that is still in the event log, because an append-only log cannot forget, and
that is the price of it being able to say what happened. The provenance on each
memory is what tells you where else to look.

Retrieval is SQLite FTS5, and it fails silently — it does not crash, the agent
just looks as though it forgot. `TestRecallOnParaphrase` measures exactly that
against a corpus of realistic paraphrases and prints what it missed. It stands
at **6 of 11**, and the misses are all the same shape: the same thing said in
other words. That number is the evidence for adding embeddings when the time
comes, and there is no point adding them before there is a number.

The reasoning, the evidence behind it, and what is deliberately not built are
in `docs/research/05-memory.md`.

## Large output

A build log that fails at line 40,000 is the most useful thing in a session and
the least printable. Truncating it throws away the part somebody wanted;
keeping it whole ends the session. So the model gets an excerpt and an
identifier:

```
/bin/cat big.txt
exit status 0 after 12ms

line-0-padding-padding-padding
...
[... 132890 bytes omitted ...]
...
line-3999-padding-padding-padding
[the whole output is 134890 bytes; read it with read_artifact on sha256-2d932a66...]
```

The model reads the rest with `read_artifact`, which reports the window and how
much remains, so a caller paging through does not have to hold its own belief
about how much there is. A person gets the same bytes:

```bash
core/bin/agent artifact get sha256-2d932a66... > build.log
```

Content is addressed by its digest, so running a failing suite four times in an
afternoon stores one log rather than four. The reference lives in the
`tool.completed` event rather than in a table of its own — the event already
records which session, which run and which tool produced it, and a second place
holding the same fact is a second place for it to be wrong. A failure carries
the reference as readily as a success: a command that timed out is exactly the
one whose output somebody wants.

Identifiers reach the store from a model, so they are input rather than facts.
Only an exact lowercase sha256 digest resolves to a path.

## Tool servers

Tools that speak the Model Context Protocol run as child processes of the
daemon and appear to the model as ordinary tools:

```toml
[[mcp.servers]]
name = "sqlite"
command = "uvx"
args = ["mcp-server-sqlite", "--db-path", "notes.db"]
level = "workspace_read"
pass_env = ["SOME_TOKEN"]
```

```
Tools:     12 (6 from 1 of 1 mcp servers)
```

Three things about that boundary are deliberate, because an MCP server is
somebody else's program running on your machine.

**A server does not get to say how dangerous it is.** The policy engine reads
`Level` and `Capabilities`; if a server could set them, one that declared its
tool read-only would walk straight past the approval a truthful one would have
to stop for. The level comes from `level` in your configuration and defaults to
`execute`, the honest floor for a call that makes another program act.
Capabilities are assumed rather than asked: network, execute and destructive,
none of idempotent or parallel-safe.

**Names are prefixed, never passed through.** Tools arrive as
`mcp_<server>_<tool>`, so installing a server cannot make `read_file` mean
something other than the one that respects the workspace boundary. A name too
long for a model to call is refused rather than truncated, because truncating
invents collisions between tools that differ only in the part that was cut off.

**Nothing is inherited that was not named.** The daemon's environment holds
your provider credentials. A server gets `PATH` and the like, whatever `env`
sets literally, and whatever `pass_env` names — nothing else.

A server that will not start is logged and skipped rather than taken as a
reason to refuse to run, and the banner says how many of how many answered: a
tool that is quietly absent looks exactly like one the model chose not to use.

## Long sessions

The conversation sent to the model is rebuilt from the event log every turn.
That is what lets a session survive a restart with its history intact, and it
is also unbounded: left alone, a session that goes on long enough exceeds the
model's context window and every turn after that fails.

So when the next request would be too large, the older part of the session is
summarised and the summary is written to the log as an event:

```
000214 conversation.compacted folded 38 messages, ~92000 tokens to ~24000
```

Nothing is deleted. The folded events are still in the log and a client may
still show them; they are simply no longer part of what the model sees. Because
the summary is an event, a daemon that restarts picks the session up from it
rather than replaying the history it replaced, and every attached client learns
that it happened the same way it learns everything else.

Two things make this safe rather than merely smaller. Compaction runs only at
the top of the tool loop with nothing outstanding — the one point where every
call the model made has a recorded result — and the cut never leaves the
conversation starting with a tool result whose call has been folded away. Both
are properties the tests assert directly.

The window comes from the provider. It can be set in `[context]` for a local
model served by something that does not report one; with neither, compaction
stays off, because summarising against a guessed window would either throw work
away early or fail to save the session that needed saving.

## Verifying

```bash
cd core && go test -race ./...
./scripts/verify-all.sh
```

The scripts are separate from the tests on purpose. Every serious defect this
project has had was in an assembly seam rather than in logic a unit test was
looking at: the daemon that never wired the projector, so runs completed and
Discord got nothing; the storage codec that did not know an event, so
compaction worked in memory and vanished on SQLite; the bot that connected,
logged cleanly, and ignored every message. None were visible to a unit test.
All were visible within seconds of running the thing.

So each script starts a real daemon, drives it the way a person would, and
checks what came out. `verify-artifacts.sh` needs a provider credential —
only a model calls tools — and skips itself when there is none.

## What is one conversation

A Discord **thread** is a session. Outside a thread, the **channel** is: two
people mentioning the bot in `#agent` are continuing one conversation, and
opening a thread is how somebody says they want a separate one.

This is worth stating because getting it wrong is invisible from the code and
obvious from the channel. Keying on the arriving message's id — which is what
this did until it was noticed — gives every mention a session of its own, and
what that looks like is an agent with no memory: ask it something, say "go
ahead", and it has never heard of you.

## What it is doing, on Discord

A run that reads four files and waits on a test suite used to say nothing at
all between "working on it" and the answer, and silence because it is busy
looks exactly like silence because something broke. It now says what it is
doing:

```
江委員  _Working on it — `read_file notes.txt`_
```

That line is **rewritten in place** rather than added to. A run touching ten
files leaves one line behind it, not ten, and when the run ends the same line
becomes `_Done in 12s._` instead of still claiming to be busy.

It belongs to its run. Keyed by channel — which it was, briefly — a new run
edited the line the previous one left behind, and since that line had ended up
at the bottom of the previous answer, asking a fresh question rewrote the tail
of the last one.

The lines are throttled to `gateway.working_interval` (two seconds), which is
about as fast as anybody reads and comfortably inside what Discord accepts as
edits to one message. Which message is live is held in the adapter's memory
rather than in the outbox, because it is a presentation detail: losing it
across a restart costs one extra line in a channel, not a wrong one.

## Answers as they are written

A model writing for twenty seconds used to be twenty seconds of nothing
followed by a wall of text. The answer now grows in one message: the projector
sends what has been said so far on a cadence, each version naming the answer it
belongs to, and the adapter rewrites the message rather than posting the same
paragraph again.

The first delta only starts the clock, so an answer finished inside one
`gateway.stream_interval` (1.5 seconds) never streams — it arrives whole, which
is what it should do rather than appearing and being rewritten a moment later.

While it is being written an answer occupies exactly one message, cut at
Discord's limit with an ellipsis. Deciding between several messages and a file
belongs to the final version, when the whole thing is known.

## Long answers on Discord

A reply that needs eight messages is a channel somebody has to scroll past for
the rest of the day. Past `gateway.max_messages` (three by default) the answer
goes as a `.txt` attachment instead, with its opening in the message so a
reader can tell whether it is worth opening:

```
Here is what I found. The suite fails in three places, all in the
vowel counting…

the whole answer is attached (18.4 KB)
```

Only the agent's own answers. An approval is the thing somebody has to act on
and a status line is short by construction; hiding either behind a download
would be worse than a long channel.

No link is ever generated for it. A `http://127.0.0.1:PORT/…` address means
nothing to anybody reading Discord, and the artifact store is not something
this daemon publishes.

## Design rules

These are load-bearing. Breaking one is a design change, not a refactor.

1. **The event log is the history.** Message lists in a UI are projections of
   it, never the source.
2. **Sessions and runs are separate.** A session is a conversation; a run is
   one execution within it.
3. **Clients are projections.** They may not decide tool permissions, judge
   whether a run has finished, or keep their own model catalog.
4. **The runtime never sees the wire format.** Translation lives only in
   `internal/control`.
5. **Not every caller is trusted.** `Run.origin` and `TrustLevel` exist from
   M0 so the gateway plane can arrive without a schema migration.
6. **Loopback is not authentication.** Any web page can reach `127.0.0.1`, and
   this API will grow a shell.
7. **A dead process must not strand a run.** Startup resolves runs left live by
   a crash and records the outcome as an event, so a reconnecting client learns
   it the same way it learns everything else.
8. **Provider deltas are coalesced before they are persisted.** A model that
   streams a few characters at a time must not turn every keystroke into a
   database write — but interrupting a reply still flushes what was already
   generated, because losing it would be worse than never batching.
9. **A model's cooperation is not a security control.** The system prompt says
   what the agent may reach, but the workspace boundary is enforced in code and
   tested by forcing the calls a well-behaved model would decline to make.
10. **A failed tool is an observation, not an error.** Structured codes and a
   suggested next step go back to the model; "exit status 1" leaves it retrying
   the same call until the budget runs out.
11. **Permission and correctness are separate.** Approval answers "may you touch
   this"; the write rules answer "do you know what you are touching". A human
   saying yes does not let the agent overwrite a file it never read.
12. **A paused run is not an orphan.** Waiting on a human is a legitimate state,
   so startup leaves it alone instead of failing it.
13. **Credentials never reach a log.** They load from an environment variable or
   a mode-600 file into a type whose `String`, `Format` and `MarshalJSON` all
   redact.

`internal/architecture` asserts rules 3–4 mechanically: the CLI cannot import
the runtime, and the runtime cannot import generated protobuf.

## Development

```bash
cd core
go test ./...
go test -race ./...
```

After editing anything in `proto/`:

```bash
buf format -w && buf lint && buf generate
```
