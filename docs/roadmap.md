# Roadmap, and what is done

The milestone names are this project's own. They are here rather than in the
README because they say where the work is, which is a different question from
what somebody arriving can rely on — that one is answered in the README, in
words that mean something without this page.

## M1b — the gateway plane

Given a repository with a failing test, JingClaw finds the cause, fixes it, and
runs the tests to confirm — stopping for a human before each change. State
persists to SQLite and survives a restart, including runs orphaned by a crash
and runs parked on an approval.

Conversation happens in a chat channel. The terminal is where you watch it and
answer it, not where you talk to it.

| Milestone | Scope |
|---|---|
| M0 | walking skeleton — done |
| M1a | durable runtime, providers, tools, permissions — done |
| **M1b** | gateway plane ✓, Discord adapter ✓, one executable ✓ |
| M2 | the console: one scrolling log, one command line |
| M3 | skills, subagents, replay, sandboxing, remote fleet |

The macOS, Windows and web clients are not being built. A terminal is a client
that works everywhere, and three of them at parity was three times the work for
the same thing.

### Where it keeps things

```
~/.jingclaw/
  config.toml    the settings
  AGENTS.md      standing instructions
  PERSONA.md     who it is
  workspace/     what the agent may read and change
  data/          the database and stored output
  run/           how clients find the daemon
  log/           what a service writes when nobody is watching
```

Created on first run. Credentials go in beside them, mode 600.

One place, always the same place. Where you happen to be standing when you type
the name does not take part: a daemon whose database depended on the working
directory is one that quietly becomes two, and then the settings you edited are
not the settings that ran.

`JINGCLAW_HOME` names a directory outright, for running against a deployment
without being inside it. Set it to `none` to say there is no directory at all,
which is how a test or a check states that it means the machine to look like
one that has never had one.

`jingclaw daemon --print-paths` reports where everything resolved, so a script
that needs the discovery file can ask rather than reimplement the rules and then
drift from them.

The workspace is *inside* the directory rather than beside it, and is not a
setting. Where the agent may read and write is one answer in one place: a
second way to say it would be a second place for the two to disagree, and
which one ran would depend on which file was edited last.

So a deployment cannot reach the project it was set up in. To work on your own
code, put it in the workspace.

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

The gateway connects a Discord bot to the same runtime, so a mention in a bound
channel starts work and the reply comes back to that thread. `jingclaw` starts
it along with the daemon; binding a channel is the only step of your own.

```bash
jingclaw bindings add --channel <id> --guild <id> --workspace ws --user <your-user-id>
```

Every default is no: a channel with no binding is unreachable, a binding with
nobody allowed permits nobody, overheard text is not a request, and bots are
refused whatever the allowlist says. Runs from a channel use a stricter
profile that denies execution outright — approving from the same account that
asked is one unbroken chain, so anything that runs a program has to be
authorised from a local client.
