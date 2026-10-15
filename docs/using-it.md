# Using JingClaw

What there is to do once it is running: what a tool may reach, what stops for a person, and how a conversation is watched and answered.

## Try it

```bash
jingclaw
```

That is the whole thing: it creates `~/.jingclaw` if there is none, runs the
daemon and the chat gateway in that one process (and the console, on a
terminal), and stays until you interrupt it. Run it again from another terminal
and it attaches a console to the one that is running rather than starting a
second. Interrupting it (or `jingclaw stop`) stops the gateway, then the daemon.

```bash
jingclaw status      # running, and where
jingclaw stop        # stop the one that is running
```

To run a part by itself, in a process of its own — which is what most of the
checks do, and what to use if you want the gateway kept apart from the daemon:

```bash
jingclaw daemon --provider=gemini --model=gemma-4-31b-it
jingclaw gateway
```

The gateway reads no settings of its own: it asks the running daemon for the
part it needs, so the daemon goes first. In the single process it is the same:
the gateway waits until the daemon has published itself, then reaches it over
loopback with its own credential, as if it were a separate process. What a
separate process gives that the single one does not is crash isolation: in the
single process a panic in the chat library's own goroutines ends everything. A
panic in the gateway's code is caught and logged, and the gateway is started
again after a wait that grows from a second to a minute. A gateway that cannot be
configured, such as a missing token, is not retried: chat stays off and the
terminal says `chat is off: <reason>`.

`~/.jingclaw/workspace` is the only directory tools can reach. Paths are
resolved and symlink-checked against it before any I/O, so neither traversal
nor a symlink pointing elsewhere gets out.

Reads run unattended; anything that modifies the workspace stops for a
decision:

```bash
jingclaw approvals <session-id>
jingclaw approve <approval-id>     # --session to allow that tool for the session
jingclaw deny <approval-id>
```

The pause is durable. A run waiting for an answer survives a daemon restart and
can be answered hours later, by a client that never saw the original prompt.

The key comes from `GEMINI_API_KEY`, or a mode-600 file named `gemini.key` in
`~/.jingclaw`, which the dashboard can write. `--list-models` prints what the
provider actually serves; a `--model` it does not serve is reported at startup
rather than on the first message.

It binds an ephemeral port on loopback and writes a `0600` discovery file
holding the address and a bearer token; the CLI finds it from there. State,
the settings included, lives in `jingclaw.db` under `data/` in `~/.jingclaw`, or
wherever `--data-dir` points.

Then create a session and follow it:

```bash
jingclaw session create
jingclaw attach <session-id>
```

And in a third, send a turn:

```bash
jingclaw send <session-id> "測試訊息"
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
jingclaw attach <session-id> --after 3
```

Only events 4, 5 and 6 arrive. Restarting the daemon changes nothing: the
session, its runs and the sequence all continue where they were.

## Watching it work

Conversation happens in the chat channel. The terminal is where you watch what
it is doing and answer it when it stops to ask.

```bash
jingclaw attach <session-id>          # follow one session's events
jingclaw approvals <session-id>       # what is waiting for a decision
jingclaw questions                    # what it has stopped to ask
```

The daemon owns the run, so detaching does not stop it. Reattach with
`--after <seq>` and you resume exactly where you left off, because the sequence
is dense within a session and a client that asks for something older than what
is kept is told to resync rather than handed a conversation missing its middle.

## The dashboard

```bash
jingclaw dashboard
```

Opens a page in your browser, served by the daemon on the same loopback
address as everything else. It shows what is waiting on you and lets you
approve, refuse or answer it; lists sessions and their runs; and is where the
settings are edited. They are not files: they are three documents in the
daemon's database, `config`, `persona` and `agents`, and this page is the only
editor. The page is written in Traditional Chinese, and every setting shows its
key (`provider.backend`) beside its name.

It is not a second client at parity with the terminal, and it does not carry a
conversation: that stays in the chat channel. An earlier web console was
removed because a page, its own credential kind, its own pairing and a client
at parity was a second product next to a terminal that already worked
everywhere. This one is narrower, and it is the same API: the page holds no
state of its own, and decides nothing the terminal could not.

The link `jingclaw dashboard` prints works once, for a minute. A browser is not
given the credential the command line uses; it trades the link for one of its
own, kept only in that tab, which ends when the daemon stops or after twelve
hours. Run the command again to let another browser in, or the same one after a
restart. It cannot be reached from inside a container yet: the daemon listens
on loopback there and nothing publishes that, so a container's settings come in
through `JINGCLAW_CONFIG`, `JINGCLAW_PERSONA` and `JINGCLAW_AGENTS` on its first
start, and [`container.md`](container.md) says how.

### Settings

The settings are a full page, not a panel beside the board. A list of sections
runs down the left, grouped as model and turns, capabilities and limits, memory,
and connections, with a search box above it; the instruction documents, the raw
text of `config` and the version history are listed under it. The section you
pick fills the right as cards: each card is a table of `config`, its rows are
the settings, and a toggle, a select or a field sits at the right of each. The
button that writes your changes is in the section's header, next to a count of
what is unwritten, and stays off until there is something to write. Nothing
else about saving changed: the diff you confirm, the refusal of a text that
fails the check, the marks for saved-not-running and the history are all as
described below.

Each section of `config` is a form, and a save writes only the lines you
changed: comments, order and settings this version does not know are left as
they are. A setting that an environment variable or a flag decides is shown as
locked, with what decides it, because a change to the saved text would do
nothing. `server.data_dir` and `server.runtime_dir` are not offered at all: they
say where the database is, so they are given when the daemon starts
(`--data-dir`, `JINGCLAW_SERVER_DATA_DIR`, `JINGCLAW_SERVER_RUNTIME_DIR`).

The two instruction documents, `persona` (who the agent is) and `agents` (how
this project is worked on), are plain text with an editor each, and the whole of
`config` is there too under advanced, as the raw text that is stored. A change
that fails the daemon's own check of the settings, the one it runs at startup,
is refused with its explanation. That check is of the settings alone: a missing
key, a missing package or the sandbox on a machine that cannot hold it show only
when the daemon starts (see Repair mode, below). A document saved from
somewhere else while you were editing it is not written over unseen: the page
says so and loads the newer text, and the version you replace stays in the
history.

### Sign in with ChatGPT

Instead of an API key, the model can be served from your own ChatGPT plan.
Choose it under Model provider (`provider.backend = "chatgpt"`) and sign in. The
account card is at the top of that section, and in repair mode it is on the list
of what is wrong, so a deployment that has no key and no sign-in can be put
right from the page. Without a sign-in the daemon stays up without an agent:

```
Why there is no agent:
  - chatgpt: not signed in; sign in with ChatGPT from the dashboard
```

To sign in:

1. Press the sign-in button on the card. The daemon opens a listener on
   `127.0.0.1` (port 1455 if it is free, otherwise any free port) and the card
   shows an `https://auth.openai.com` link, which the page also tries to open in
   a new tab. The wait is ten minutes.
2. Approve on the ChatGPT page, including using your ChatGPT plan. Without that
   grant the card says so and the account cannot be used; sign in again and
   approve it.
3. The browser is sent back to `127.0.0.1:<port>` and the card reports that you
   are signed in. Pick the model from the list the account can use, set the
   provider to ChatGPT (the card has a button for it), write, and restart:
   `jingclaw stop`, then `jingclaw`. The sign-in is read when the daemon starts,
   like the settings.

**A browser on another machine** (a container host, an LXC, a server you reach
over SSH) cannot finish step 3 by itself: after you approve, it is sent to
`127.0.0.1:<port>`, which means the machine the browser is on, so that page fails
to load. That is expected. Either copy the whole address from the browser's
address bar and paste it into the field under the link on the card, or forward
the port first and the sign-in completes by itself:

```bash
ssh -L <port>:127.0.0.1:<port> <the machine running JingClaw>
```

The pasted address holds a one-time code. It goes to the daemon and is not kept
on the page.

**Sign out** is on the card too. It asks ChatGPT to revoke the sign-in and
removes the tokens from this machine, and then says whether ChatGPT confirmed
the revocation; when it did not, check your ChatGPT account yourself. The
registration and the host id stay in the file, so signing in again reuses them.
A model backend of ChatGPT with nobody signed in does not start an agent.

**Usage and limits** are managed by ChatGPT, under Settings, Usage. OpenAI
documents no usage endpoint, so JingClaw shows no usage numbers and never guesses
a reset time. When the plan's limit is reached the run stops with that reason,
the card shows the last error, and nothing is retried or moved to another way of
paying.

What to know before relying on it:

- It is implemented from OpenAI's documentation (developers.openai.com/siwc) and
  tested only against fake servers built from the documented shapes. It has not
  been run against the live service. The least certain part is how the agent's
  tools are declared to it: the documentation allows function tools but shows no
  example of the shape, so a refusal from the service may need a change here.
  If a run fails with "unsupported" naming `tools`, that declaration is the
  likely cause, not your setup.
- One account at a time.
- OpenAI documents this flow for open-source and locally hosted apps. A paid or
  remotely hosted app has to file OpenAI's interest form instead. The grant gives
  no access to your ChatGPT conversations.
- What the sign-in holds is in `chatgpt-auth.json` in `~/.jingclaw`, readable by
  its owner alone. It is not in the database, the settings, a log or any reply
  the page receives.
- A container cannot reach the dashboard yet, so it cannot be signed in this way.

### Saved, not running yet

The daemon reads all three documents once, when it starts. A saved change shows
as saved and not running yet, across the top of the page and next to the
document, until the daemon is restarted: `jingclaw stop`, then `jingclaw`. The
page compares what is saved with what the running process started from, so the
mark is there for as long as it is true. A restart ends the browser's access to
the dashboard as well; run `jingclaw dashboard` again.

### Versions

Every save that changes a document is kept, and the last 20 of each can be read
in full. The history page shows when each was saved and where it came from, how it
differs from what is saved now, and restores it, which is a new save and so goes
through the same check. Taking a secret out of the current text does not take
it out of the old ones: a key typed into an MCP server's headers or environment
is in every version saved while it was there, until 20 later saves push those
out. Put a key in `pass_env`, or in a credential file, and it is in none of
them.

### Repair mode

If the saved settings cannot be used, or the agent cannot be built from them
and the environment (a key that is missing, a model the provider does not
serve, a text that does not read), the daemon does not exit. It stays up
without an agent and serves only what is needed to put that right, so the page
opens on a list of what is wrong, each with its fix and a way to the setting or
key it is about. Conversations, approvals and schedules are not there; nothing
is running to serve them. The terminal says so too: `jingclaw` prints `agent not
running`, `jingclaw status` shows the mode, and `jingclaw dashboard` lists the
problems before it prints the link. A setting that a variable or a flag decides
is fixed there, not on the page. Fix it, then `jingclaw stop` and `jingclaw`:
there is no restart from the page.

### Keys

A credential comes from an environment variable that a setting lists or, when
there is none, from a file in `~/.jingclaw` that a setting names (`gemini.key`,
`discord.token`). Under each such file setting the page says where the key would
come from, what is in the way (a file that others can read is refused, not used),
and lets you set or replace it, remove it, or tighten its mode. A key is typed
into a password field and written to the file readable by its owner alone; it is
never shown again, and nothing the daemon returns carries a key, its length, a
prefix or a hash. A write replaces the file with no backup. Like the settings, a
key is read when its process starts, so a changed one is saved and not running
yet.

### Skills

A skill is a directory in `~/.jingclaw/skills`. The page adds one from a git
source written `git:<url>#<40-hex commit>[:path]`: a commit, because a branch or
a tag is a name somebody else can move. The fetch goes to a staging area and
installs nothing. You then read the whole of SKILL.md, every file, where it came
from and, if it replaces one, how it differs from the installed one, and only
then activate it, against the digest of what you were shown, so a skill that
changed after it was staged cannot be activated. Installed skills can be removed,
and the list says which were placed by hand and which have been edited since.
`jingclaw skills` installs, lists and removes them from a terminal too. A skill
grants nothing: what it tells the agent to do goes through the same checks and
approvals as anything else.

### Memory, channels and schedules

The memory page lists what the agent believes and where each belief came from,
and forgets one. The channels page lists which channels may reach the agent and
who may talk to it there, and adds or removes a binding; both take effect at
once. A channel the settings declare is bound again at every start, so removing
it here lasts only until the next one. Who may approve cannot be set from the
page: that is `approvers` in `gateway.discord.channels`. A new schedule is a
time (five cron fields, or `@hourly`, `@daily`, `@weekly`, `@monthly`), what to
do, a time zone, and whether firings missed while the daemon was down run once or
not at all. It runs with nobody watching, so it can read and search and cannot
write, run commands or remember, and its answer stays in its own conversation.
Schedules can be paused, resumed and deleted.

## Who is talking

In front of every turn a person takes, the agent is shown one line this
machine wrote — never mixed into their words:

```
[2026-09-04T22:46:07+08:00 · from <@123456789012345678> (doeshing) on discord via the gateway]
```

When it was sent, who sent it, and the way it came in. On Discord the sender
is written as the platform addresses them — `<@id>` — because the id is the
platform's stable name for a person where a display name is whatever they
typed this week and may be somebody else's, and because written that way it
is what a reply can use to reach them; the name they go by sits beside it. A turn typed at the terminal says `from this machine`; a schedule
says which schedule. In a room several people type in, this is how the agent
tells them apart and knows who it is answering — before it, everybody in a
channel was one person to it.

It is a label, not a control. Somebody can type those characters into a
message, and nothing here prevents it; what holds is that permissions come
from where a run came from, and no line of text raises them. The time is the
event's own, so the same turn reads the same on every rebuild and the prompt
prefix a provider caches does not change for nothing.

## Images

Send the bot a screenshot on Discord — **attached to the message that mentions
it**, not sent separately — and it looks at it. Without the Message Content
intent the bot only sees messages addressed to it, so a picture posted on its
own is a picture it was never shown.

From a terminal it is the same picture, a flag away:

```bash
jingclaw send <session-id> "what is wrong with this layout" --attach shot.png
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

On by default. What is written here is read by every later session, by an
agent that no longer knows where it came from, so turning it off is a single
line:

```toml
[memory]
enabled = false
```

Otherwise `remember` writes something down and `recall` looks it up. Three rules
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

**It takes notes by itself, and says so.** Once a person has been answered,
the machine reads their own messages from that turn — not the reply, not what
a tool returned, not what a page said — and asks the model what in them is
worth knowing next month: their name, their timezone, a preference, a
decision about the project. A turn that failed is read with the next one that
is answered, so "try again" does not lose what was said before it; a turn
that was stopped or taken back is not. Each note is written in the language
the person used, so it is found again by how they ask. Each proposal has to quote the message it rests
on, verbatim, or it is dropped; a model that wants to note something nobody
said has to invent words that are then not found. What survives is written as
a retrieval memory in the person's own scope (a turn typed at this machine
may also add to the project's), with the session and message it came from,
and with `nobody (noted from discord:…)` where an approver would be. At most
three per turn, never the same note twice, and never a standing one: a note
is a claim, and authority is a person's to grant. The model is shown what is
already noted, so a preference said again in other words is not written
again. What was noted is a `memory.noted` event in the session: a chat
channel shows the count under the answer, and the console shows the notes.

Those notes come back on their own. Before the turn being answered, the
machine looks up the notes its sender may read — the project's and their own,
never another account's — chosen by what the turn says, and puts the top few
in front of it as a bracketed block that names who said each and when, and
which arrived from outside this machine. The prompt tells the model the block
is its own notes, claims and not instructions, possibly wrong or stale. Only
the turn being answered gets the block: earlier turns are history, and the
front of the conversation stays a prefix the provider can keep caching.

```toml
[memory]
curate = true          # note what people say, one small model call per turn
auto_recall = 3        # notes put in front of a turn; 0 for none
auto_recall_bytes = 1024
```

You can see all of it, which is the control that matters:

```bash
jingclaw memory list             # everything, with where each came from
jingclaw memory list --history   # including what has been superseded
jingclaw memory forget mem_01K…  # gone, not merely superseded
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

Retrieval by words is SQLite FTS5, and it fails silently — it does not crash,
the agent just looks as though it forgot. `TestRecallOnParaphrase` measures
exactly that against a corpus of realistic paraphrases and prints what it
missed. It stands at **6 of 11**, and the misses are all the same shape: the
same thing said in other words, or in the other language.

Name an embedding model and memories are found by meaning as well. Any
OpenAI-compatible `/embeddings` endpoint works; Ollama on this machine is the
one that sends nothing anywhere:

```toml
[memory.embedding]
model = "qwen3-embedding:0.6b"          # ollama pull qwen3-embedding:0.6b
base_url = "http://127.0.0.1:11434/v1"
```

Each search runs both ways and merges the two lists by rank, so a note found
by its exact words and a note that means the same thing in other words both
come back. A note is found by meaning alone only when it is alike enough:
`min_similarity` for the notes put in front of a turn unasked, and the lower
`recall_min_similarity` when the model searches and can judge what it gets.
Memories written before the model was named, or while the endpoint was down,
are embedded when the daemon starts; until then they are found by their words,
and a search whose embedding fails is a search by words. Every memory and
every turn a person types is sent to the endpoint, which is why it is off
until a model is named.

`TestRealRecallWithEmbeddings` measures it against a real model. On
qwen3-embedding:0.6b, over seventeen questions in other words and both
languages, the first note returned was the right one for **17** when asked
and **11** unasked, against **7** by words alone; none of eight unrelated
turns had a note put in front of them. The two default lines were measured on
that model, and another model draws them elsewhere.

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
jingclaw artifact get sha256-2d932a66... > build.log
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
sets literally, and whatever `pass_env` names — nothing else. What `env` and
`headers` set literally is kept in the saved settings as typed, and in their
older versions; `pass_env` keeps a secret out of them.

A server behind an authorization server (`oauth = true`, which only a url server
can be) is signed in to once from a terminal, with `jingclaw mcp login <name>`.
`jingclaw mcp list` says which servers still need it and `jingclaw mcp logout
<name>` forgets a session. All three ask the running daemon which servers are
saved, so it has to be running. The daemon never opens a browser itself, since
nobody is watching it, and a session that has run out is reported rather than
left to block a run.

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

Each fold covers one range of the log and is written from those events alone.
The next compaction does not hand the model the summary already in front of
the conversation to condense again; it folds the turns after it, and the
conversation opens with every fold that stands, in order, followed by the
verbatim tail. A turn is condensed once, in the words that condensed it, and
a fold once written is sent byte for byte on every later turn — which is what
keeps the front of the conversation a prefix the provider can go on caching.
Only when six folds have piled up are they condensed into one, and the model
is told that is what it is being asked to do.

The window comes from the provider. It can be set in `[context]` for a local
model served by something that does not report one; with neither, compaction
stays off, because summarising against a guessed window would either throw work
away early or fail to save the session that needed saving.
