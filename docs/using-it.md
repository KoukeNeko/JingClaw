# Using JingClaw

What there is to do once it is running: what a tool may reach, what stops for a person, and how a conversation is watched and answered.

## Try it

```bash
jingclaw
```

That is the whole thing: it creates `~/.jingclaw` if there is none, starts the
daemon and the gateway, and stays until you interrupt it. Run it again from
another terminal and it says so rather than starting a second one.

```bash
jingclaw status      # running, and where
jingclaw stop        # stop the one that is running
```

To be one of the parts by hand — which is what the supervisor does, and what
the checks do:

```bash
jingclaw daemon --provider=gemini --model=gemma-4-31b-it
jingclaw gateway
```

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

A console — one scrolling log across every session with a command line at the
bottom, the shape a game server has — is the next thing being built. There was
a web console; it was removed. A page served over loopback, its own credential
kind, its own pairing code and its own client at parity was a second product
next to a terminal that already worked everywhere the first one did.

## Who is talking

In front of every turn a person takes, the agent is shown one line this
machine wrote — never mixed into their words:

```
[2026-09-04T22:46:07+08:00 · from doeshing on discord via the gateway]
```

When it was sent, who sent it (the platform's own name for them), and the way
it came in. A turn typed at the terminal says `from this machine`; a schedule
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
