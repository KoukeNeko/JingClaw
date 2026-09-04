# Where JingClaw is

Updated 2026-09-02.

## Done

**M0 — walking skeleton.** Proto contract, event log, runtime, Connect API,
CLI, loopback auth with scoped credentials, discovery file. The FakeProvider
harness is still in use and should stay.

**M1a — usable for a day of work.**

| Item | State |
|---|---|
| SQLite persistence, restart resume | done |
| Provider (Gemini/Gemma; fake for offline) | done |
| Context builder, token budget, compaction | done |
| Permission engine and durable approvals | done |
| Artifact store | done |
| Built-in tools: read / glob / grep / write / edit / exec / read_artifact | done |
| `AGENTS.md` loading, layered system prompt | done |
| MCP stdio tool servers | done |

The plan also listed Anthropic and OpenAI/Ollama providers. Only Gemini is
implemented, because that is the key this deployment has; the provider
abstraction is what the others plug into.

**M1b — Discord gateway.** Verified live against a real bot: mention → run →
tools → reply posted back to the thread.

**Cross-session memory.** Off by default. `remember` / `recall`, provenance on
every entry, correction by invalidation, real deletion, and `agent memory list`
so a person can see everything the agent believes and where each belief came
from. Gateway-origin memories are permanently untrusted and never become
standing directions. Researched first: `docs/research/05-memory.md`.

**Reading the web.** Off by default. `web_read` returns a page's visible text
and links and can do nothing else — no clicking, typing, signing in or
submitting, which are a separate capability that does not exist yet. Pages are
fetched by driving a real browser, because a plain HTTP request now gets a
challenge page from a large part of the web and gets nothing at all from
anything that renders client-side.

Measured rather than assumed: it fetches live pages end to end through a real
model, and it does **not** get past Cloudflare/DataDome-class protection —
g2.com and indeed.com answer 403 in headless, headed and humanized modes alike
(cloakbrowser 0.3.30). The backend is behind a `web.Fetcher` interface for
exactly this reason.

The guarantees that do not depend on the backend: addresses are refused by what
they resolve to rather than how they are spelled, on every resolved address,
and again inside the browser for wherever a redirect leads; results open with
their provenance and a statement that the text is somebody else's; and the
gate depends on who chose the address — unattended locally, an operator
approval from a chat channel.

**Provider backends.** Gemini, Ollama (local and cloud, through its own API),
and any OpenAI-compatible endpoint behind a named dialect profile —
generic / vllm / lmstudio / llamacpp / openrouter / groq / together.
Researched first: `docs/research/06-provider-backends.md`.

The load-bearing part is not the protocol but the context window. A model
trained for 128k is routinely loaded with 4k, so a window now carries its
provenance and the order of belief is operator, then a server reporting what it
has loaded, then a catalogue, then the model's own maximum. `verify-providers.sh`
drives both adapters against servers reporting exactly that mismatch.

Reasoning is its own event, never assistant text: backends expose it under three
different field names and one of them inline, and folded into the answer it
would be posted wherever the answer goes. `resource_exhausted` is its own error
kind, because a machine out of memory is not an account out of allowance.

**Provider failures.** Retry had never once run against Gemini: the wrapper
watched `Generate`, and that adapter returns a lazy stream, so every retryable
failure arrived at `Recv` instead. Five of twenty-eight runs in the live
deployment failed on rate limits, none retried. Retry now covers a stream that
failed before emitting anything, and never one that has already produced output.

A server's stated delay is honoured in full rather than capped, since asking
again early earns the same refusal; a delay beyond the request's budget ends it
with a reason instead. 429 is split by Google's structured quota details into a
minute's rate limit and a spent allowance, on the quota identifier rather than
the English message.

**A channel as a console.** "Discord" was one trust level and is not. A binding
names `gateway.channels` or `gateway.consoles`, and which list it is in decides
its powers — there is no profile field to misspell or set to the one meant for
somebody at the machine. A console reads and fetches unattended, answers its own
approvals in the channel (`pending`, `approve <id>`, `deny <id>`), and hands over
stored output on request (`artifact <id>`, pull never push). Neither list can run
programs: channel permissions settle who is in the room, not whether an account
still belongs to its owner.

Channels are declared in the configuration and applied at startup. Removing one
does not unbind it; the startup log names every bound channel the file does not.

**Session views.** `GetSessionView` answers what a session looks like without
replaying its log: assembled messages, the tools each turn asked for, what it is
blocked on, and a sequence number to subscribe from.

**MCP over Streamable HTTP.** A server is a child process or an HTTP endpoint,
never both, with headers for the credential. What its tools may do still comes
from the configuration rather than from the server.

**A `.JingClaw` directory.** Configuration, workspace, database, runtime files
and credentials in one place, found by walking up from the working directory.
`JINGCLAW_HOME=none` says explicitly that there is none, which is what every
check sets so that none of them can reach the operator's own deployment.

**Event retention.** Pruning only ever removes what compaction has already
folded, and a client resuming below the oldest kept sequence is told to resync
rather than handed a gap it cannot see.

**A second gateway platform.** Telegram, which is what shows whether the
abstraction holds. `gatewayd` was typed to `*discord.Adapter`, so nothing said
what a platform owed the gateway; it is now two methods, and the platform comes
from the configuration.

Telegram disagrees with Discord about enough to be worth it: a mention is a
byte range counted in UTF-16 rather than a token, a status line has to be text
because a bot may only react with a fixed set of emoji, and an upload is
multipart where everything else is JSON. Two things fell out of the exercise —
the daemon was reading Discord's configuration section to pace the projector,
which has no platform, and the Discord adapter's own status-line path had no
caller at all.

The dispatch rendering moved out of the Discord adapter with it. The wording of
a run summary is not a Discord decision; what a platform decides is how long a
message may be, how a subdued line is marked, and whether emphasis renders.

**Reasoning, shown where it may be seen.** Both local adapters produce it and
the runtime dropped it, so a user who turned thinking on saw nothing. It now
travels under its own event kind, which is what lets each place decide: the
projector refuses it — console channel included, since a platform account is
somebody's account — and the CLI shows it apart from the answer.

**Long-running programs.** `start_process`, `process_io`, `stop_process`: a dev
server, a REPL, an installer that asks a question. A process belongs to its
session rather than its run, so a run that starts a server and ends does not
take the server with it; nothing else would end them, so closing the session
does and so does stopping the daemon. A pseudo-terminal where there is one, and
an honest report that there is not where there is not.

**Reviewing a call before allowing it.** An approval carried the arguments and
nothing else, so allowing an edit meant reading nine hundred characters of
old text against nine hundred and fifty of new. The tool that defined the
arguments now renders them — a diff for an edit, the command line for an
execution — and the CLI shows that rendering rather than the raw call. The exact arguments stay one click away: a decision made against a
rendering that disagreed with the call would be a decision about something
else.

**Artifacts, everywhere and after the fact.** The id was on the event and not
in the session view, so reopening a session lost every way to the build log
explaining the failure being shown. All three clients can now open one inline
and save it whole.

**A credential per paired browser.** Pairing handed out one shared credential:
a page paired last week still worked, nothing said what had been let in, and
revoking one browser meant revoking every one. Each pairing now mints its own,
listed with when it was paired and last used, revocable one at a time or all
at once, expiring from last use rather than from pairing.

**Channels in the GUI.** What is bound, what each channel may do in words
rather than as a profile name, who may trigger work there, and a way to unbind
one without editing a file and restarting.

**A model per session.** One model per daemon is wrong for a machine where the
small one fits in memory and the large one does not. A session may name its
own; the provider stays fixed, because a conversation carries blocks only its
own provider can read back. The run summary names the model that actually
answered rather than the daemon's default — a line naming the wrong one is
worse than no line, since it is what somebody reads to work out why an answer
was poor.

**The tools the plan called M1 and the agent did not have.** All six.

`todo_update` keeps a plan as agent state: patch-style operations, because a
model asked to rewrite the list drops ids it does not think matter and revives
steps it already finished, and none of that is visible as an error. It is put
back in front of the model every turn, survives a restart, and every client
shows it.

`ask_user` parks the run and the answer comes back as the result of the call
that asked. `awaiting_input`, not `awaiting_approval` — every client offers a
different control for the two. Not built on approvals: an approval asks
whether something may happen, and this asks what a person wants.

`apply_patch` changes several files as one thing, working out all of them
before writing any, so a patch that cannot apply cleanly changes nothing. Both
editing tools now go through one engine, which the plan asked for.

`git_status` and `git_diff` read the repository without an approval, which is
what they are for: "what have I changed" is the question before and after
every edit, and going through `exec_command` asked every time. The arguments
are fixed here — a tool that ran whatever git subcommand the model named, at
read level, would be most of a shell. `--no-ext-diff` and `--no-textconv`
matter: a diff otherwise runs a program the repository being read configured.

`web_search` finds an address rather than reading one somebody named. Its own
switch, off by default, because letting the agent choose where to go is a
different thing to be trusted with.

`list_processes` closes the gap the process tools left behind.

**Trust means what it said.** `Memory.Trust` was documented as "the least
trusted thing that contributed" and was the trust of the turn's origin, so a
local turn in which the model read a hostile page and wrote down what it said
produced a memory recorded as the operator's own word — eligible to become a
standing instruction. A tool now declares whether its results carry somebody
else's words, the completion event records it, and the conversation's trust is
derived from the log, bounded by the call's own position.

This is the hole OpenClaw documents in its own architecture and has not
closed. Its structural rule — trust is promotion eligibility, not a retrieval
score — is the part worth having, and JingClaw already had it; what was wrong
was the trust value being fed into it.

**Memory keeps two timelines.** `valid_from` / `valid_until` beside
`created_at` / `invalidated_at`, so a fact can stop being true without anybody
having been wrong about it. A correction now records both moments: when this
agent stopped carrying the old memory, and when the thing it described stopped
being true. Those are rarely the same, and one timeline has to lose one of
them.

An inactivity expiry was built here and removed after review. It retired a
memory nobody had recalled in ninety days, which is anti-correlated with its
own purpose: corpus rot is caused by near-duplicates, and those are recalled
constantly and never expired, while a correct, important, cold fact — the
production namespace of a service nobody has deployed since spring — died on
schedule. It also let retrieval frequency decide truth, and made a read a
write. Nothing is forgotten now for being unpopular.

OpenClaw reaches the same place from the other direction: its FAQ says memory
files persist until deleted, and it uses a ranking half-life rather than
expiry. Zep is the one that models valid time properly.

**A lookup that misses is tried again with other words.** Memory is searched by
word, and the way that fails is silent: "prefer reusing an existing component
over building a second one" and "should I add a new modal?" are the same
subject with no word in common, so the index returns nothing and reports
nothing missing. When a search comes back empty the model is asked for the
vocabulary the same note might have been written in, and the search runs once
more with those words added to the original. Results found that way are
labelled, because they answer a question near the one that was asked.

The cost is one small completion, paid only on a search that already failed —
a search that matched never reaches the provider. What comes back is used as
search terms and nothing else: it is quoted the way any model-written query is,
so it cannot become MATCH syntax, and the scopes a turn may read are still
decided by the turn rather than by anything in the answer.

This is the cheap half of what a vector index would buy. It does not survive
the provider being unreachable, and it cannot rank by meaning — it only widens
what is matched. Embeddings remain the thing most likely to be missed later.

Two things it does not do. Its tokens are not counted in the run's usage,
which is the same gap compaction's summary call already has: both go straight
to the provider rather than through the turn that reports usage. And it cannot
help a memory written in Chinese, because the search index behind it cannot
find one at all — fts5's default tokenizer reads a run of Han characters as a
single token, so 「元件」 matches nothing in a memory containing 「既有的元件」.
Expansion then fires on every such lookup and pays for a call that cannot
help. That is a defect in the index, not in this, and it is open.

**Approving from a channel, by name.** A room can now hold three separate
powers: being in it, being allowed to ask the agent for something, and being
allowed to permit what it asks. `approvers` and `approver_roles` sit beside
`users` and `roles` in a channel's entry, empty meaning nobody, and nothing
about being allowed to ask implies being allowed to permit.

The press arrives on the gateway ingress rather than on the session service,
and that is the whole shape of it: `SessionService.DecideApproval` settles
anything by id and belongs to an operator's client, while the new
`GatewayIngressService.DeliverDecision` only reports that a named person
pressed something in a named channel and lets the daemon decide whether that
counts. A gateway that could answer that question itself would be a bot token
that can approve. `verify-approval-buttons.sh` checks that the gateway
credential is refused by the first and accepted by the second.

Typing is still not enough in a shared room, and does not become enough: a
message says which account posted it, and that is all. A button press is
delivered by the platform with the presser's authenticated identity attached,
which is a different claim — the same distinction OpenClaw's Discord
CVE-2026-27484 got wrong in the other direction.

`ConsoleRuntime` became `DecidingRuntime` in the process, because an ordinary
channel can now decide and the old name had stopped being true. The gateway
also briefly grew a dependency on the runtime, for one error value; the value
moved to `domain` and `internal/architecture` now has a test that fails if the
dependency comes back.

**Tables reach a channel readable.** No platform here renders Markdown table
syntax, so a model's table arrived as a wall of bars — worse in Chinese, where
the columns do not line up even in a monospaced font unless the padding is
computed by display width rather than by counting characters.

A table is now found by parsing rather than by splitting on bars, because a
cell may hold an escaped one or a code span containing one, and splitting
turns that row into more cells than it has, silently. Narrow tables become an
aligned block; wide ones become one paragraph per row, labelled with the
header so a row still says what its values mean.

Padding is by grapheme cluster and East Asian width: "中" is one code point
and two columns, a flag is two regional indicators and one glyph, and an
escape sequence is bytes and no columns. len, the rune count and the code
point count are three different numbers and none of them is this one.

The alignment is the best a sender can do rather than something the receiver
guarantees. Discord's Latin code face is monospace, but a CJK glyph may come
from a fallback font and standard emoji differ by client — desktop and Android
use Twemoji, iOS uses Apple's. So a table whose alignment must be exact does
not belong in a message at all. Where emoji are in play, put them in the last
column or write the status as a word.

A block longer than a message goes as a file rather than in pieces. Cutting a
preformatted block closes the fence and reopens it, so one block becomes two —
and a table cut two rows from its end leaves a second message holding nothing
but its bottom rule, which is what a channel actually showed. `Split` still
does the repair, because a caller may have nowhere else to put the text;
`SplitsAFence` is how a caller that does find out. Found by looking at Discord,
not by a test: the table this came from was one the model had preformatted
itself, so nothing in the table renderer was involved.

The Discord assertions live in the adapter and read its own style rather than
a copy of it in a test: the fence, the width budget and the emphasis markers
are configuration, and a test carrying its own version can pass while what the
channel receives is something else.

**One client, and it is the terminal.** The embedded web console, the macOS
client and the Windows placeholder are removed. Conversation happens in
Discord; a client's job is to watch and to decide, and three implementations of
that — one of them a `.gitkeep` — were three chances for the same event log to
be folded into three different screens.

What went with them: the pairing flow, `ScopeConsole`, `ConsoleService`, and
`server.web_console` / `pairing_ttl` / `console_ttl`. There is now no
unauthenticated path into the daemon at all; the browser needed one because it
could not present a bearer token on the request that fetched the page.

`verify-console.sh` became `verify-api.sh`. Its browser-specific checks went,
and five did not: an uncredentialed call is refused, a rebound host is refused
even with a credential, a gateway credential reaches only the ingress, and
Connect works as both plain JSON and length-prefixed frames. Those were never
about the browser, and the TUI will use the same transport. The removal was
done test-first — the new assertions were written and watched to fail before
the code went — because a test edited alongside the code it checks relaxes
with it and nobody notices.

`fixtures/session-view.json` stays, with the JS and Swift checks gone. It is
the recorded behaviour of the session view rather than a cross-language
contract now, and the TUI will be the next thing checked against it. Writing
it again later from the implementation would make it a copy of the code
instead of evidence.

Not done: the TUI itself. Between now and it, `agent` is the only way to see
a session from this machine.

**One deployment, wherever it is started from.** A deployment used to be found
by walking up from the working directory looking for `.JingClaw`, falling back
to the platform's own location, with an extra rule for the XDG convention on
macOS. Three rules produced one path, and which one applied depended on where
somebody typed the daemon's name.

That cost three failures in one day, none of which looked like a failure: an
approval button that did not appear because the config being edited was not the
config being read; "are you sure you changed it?"; and a provider switch that
did nothing. Two directories both looked real, only one was live, and nothing
said so.

Now: `--config`, then `$JINGCLAW_HOME`, then `~/.jingclaw`, on every platform.
The working directory does not take part. `verify-home.sh` starts the same
deployment from three directories — two of them with a decoy `.jingclaw` above
them — and requires `--print-paths` to agree. Putting the walk back fails it.

The workspace is fixed too. It used to be the working directory when there was
no deployment directory, which handed a fresh install the contents of the first
project somebody started it from. OpenClaw reached the same layout after the
same confusion, and its docs say plainly that two workspace directories caused
"auth/state drift because only one workspace is actually active".

**Identity is files, not settings.** `agent.name`, `agent.persona`,
`agent.instructions` and `agent.instruction_files` are gone. `AGENTS.md` and
`PERSONA.md` are created by `--init` and read every run; the names are fixed.
Each setting had been a second place to say the same thing, and a second place
is one somebody edits while the first is what runs. Asked independently, the
same conclusion came back: Hermes has `SOUL.md` *and* `agent.system_prompt`
*and* `personalities`, three ways to change one thing, and that is the part of
it not worth copying.

**A starter, not a reference.** `config.example.toml` went from 405 lines to
215, prose from 203 lines to 56, with every setting still present. The rule
kept: a comment earns its place only when deleting it would make somebody
editing that line more likely to get it wrong. The rest is
`docs/configuration.md`. The file moved under `docs/` as well — a template
sitting where a config file could be read is one somebody will edit and then
wonder why nothing changed.

Sections are ordered by what somebody is looking for rather than by how the
code grew: agent and provider, then workspace and durable state, then
capabilities, then interfaces.

**Each backend keeps its own credential and model.** `[model]` is
`[provider]`, `provider =` is `backend =`, and `api_key_env`, `api_key_file`
and `model` moved from the shared section into `[provider.gemini]`,
`[provider.ollama]` and `[provider.openai_compat]`. Shared, only one backend
could be set up at a time: switching meant editing the key settings too, and
switching back meant editing them again from memory. Now switching is one line.

**Shutdown that waits.** One deadline covered both phases, and a held-open
stream made http shutdown consume all of it, so the runtime was asked to drain
with none left and was never actually waited for. Each phase has its own
window now; stopping went from ten seconds to two.

**One program you open.** `agentd`, `gatewayd` and `agent` were three binaries
that had to be started in the right order by hand, with a shell script holding
them together. They are now subcommands of one `jingclaw`, and running it with
no arguments starts the daemon and the gateway and stays with them — the shape
a game server has. It stops what it started and leaves alone what it found:
run it twice and the second says so rather than putting a second daemon on the
same database. `restart-agentd.sh` is gone; it existed because there was no
executable.

Still two processes. The gateway holds somebody else's bot token and a socket
to the internet, and the process that owns the shell, the workspace and the
event log must not go down with it.

**A service, for when nobody is at the keyboard.** `jingclaw service
install/uninstall/status` writes a launchd job rather than handing somebody an
XML block to edit a path into. Its PATH is asked of the login shell in a clean
environment, not taken from the installing process: a service inherits nothing,
and the process installing it may be an editor or an agent carrying directories
that will not exist next week.

Not verified end to end, deliberately. Loading the job would change the login
session of whoever ran the check — a second daemon on their own database, or a
service they already had replaced — and a check that does that is worse than a
gap. `verify-service.sh` proves the description instead: launchd can parse it,
it names the executable that printed it, its PATH has the tools the agent runs.

**The console.** One scrolling log across every session and a line to type
at, which is what opening JingClaw gives you. The log has a position of its
own (`global_seq`) so a console that reconnects can say how far through it has
read — a per-session number cannot answer that, and a timestamp is not a
position: a clock that goes backwards puts an event behind a cursor that has
already passed it.

Drawn the way a game server's console is, and for the same reason: the log and
the person typing both want the terminal, so a log line erases the input line,
writes itself, and puts the input line back. Verified through a real pty,
since raw mode and redrawing the bottom row do not happen against a pipe.

**Skills.** Instruction packs an operator installs by copying a directory in.
The catalogue in the prompt is names and descriptions; the instructions
themselves arrive only when the model asks for one by name. What holds it up
is that a skill can make the model want to do something and can never make the
runtime allow it, and the check installs a skill that tries — it declares
permissions, demands no approvals, and tells the model to ignore everything
before it, and the write it asks for still stops for a person.

**Tables as pictures, for a chat channel.** Off by default. A code block is
laid out by whatever font the reader's client has, so a table of Chinese and
Latin arrives bent however carefully its columns were counted; a picture is
the layout rather than a description of one. An answer with a table in the
middle becomes three messages, because an attachment sits below the content
rather than inside it.

**A line for the console.** `internal/console` turns an event into one line:
fields in a fixed order with the payload last, because the payload is the only
one that can be arbitrarily long and cutting from the right takes the session
and the state with it. An approval shows the command it was asked with and
never a summary of it. Clipping counts what the terminal draws, not bytes or
runes. No terminal, no state, no dependency — the rest of the console can be
argued about without this changing.

**A clock the agent can read.** `current_time`, a tool rather than a line in
the prompt. The prompt is a stable prefix that providers are paid to remember
and replay, so a clock written into it is correct once and then replayed as
fact for as long as the prefix survives — not a small inaccuracy but a stale
answer with nothing marking it stale. It gives the instant with its offset,
the weekday, the same instant in UTC, and the zone by name where the machine
has one to give. By name and not by abbreviation: "CST" is China Standard
Time at +08:00 and Central Standard Time at -06:00, and a reader given only
that converts to whichever it guesses. Every turn a person took now carries the time
they took it, taken from the event rather than from the clock: the
conversation is rebuilt from the log on every request, so a time read during
that rebuild would date an old turn to now and would change the prefix a
provider is paid to remember on a request where nothing about the history
changed.

It sits in its own block rather than in their words, and that block is marked
as this machine's. Otherwise anything asking what somebody said gets a
timestamp back, which is how the fake provider came to echo one.

**A gateway that follows the daemon.** The daemon publishes a fresh address
every time it starts. The gateway read that once, kept it, and went on
dialling a port nobody answers — while still connected to the platform, still
marking every message as seen, and delivering none of them. Nine hours of
that in a live deployment, and from the room it was indistinguishable from an
agent that had decided not to reply. Every request now resolves the daemon
from the discovery file as it is made, and a room whose message went nowhere
is told so: once per outage, per room, and again after the next one.

**A terminal panel, built and then removed.** `docs/PROPOSED_PLAN-tui.md` says
at the top that it is superseded — the console replaced it. Read by its
headings, that banner was invisible, and what got built was the shape the plan
had already rejected: a session list, a full-screen session view, several
screens. Two halves of it were worth keeping and are now console verbs.
`show <id>` prints a waiting call in full, because deciding whether to run
something means deciding about that thing rather than about its first seventy
characters. `open` hands stored output to whatever reads that kind, with the
extension taken from the media type and never from anything the agent chose,
a short allowlist, and a file that is never executable.

**A deployment that can bring its own files.** `JINGCLAW_CONFIG`,
`JINGCLAW_PERSONA` and `JINGCLAW_AGENTS` write `config.toml`, `PERSONA.md` and
`AGENTS.md` on first start. Some container platforms offer exactly two inputs,
a mounted volume and a list of variables, and all three of these are files in
a directory that nothing can put a file into before the first start.
Individual settings do not survive that trip — the environment reaches
`provider.backend` and stops, because a name like `api_key_env` makes any
deeper underscore ambiguous, and a list of tables like the channel bindings
has no spelling as a variable at all — so the file goes in whole. A value
prefixed `base64:` is decoded first, because a platform's single-line form
drops the newlines and a persona without its line breaks is a different
persona; the prefix is required rather than guessed, since short markdown is
often valid base64 by accident.

Never over a file that exists. The variables are set on the service and arrive
again on every restart, so overwriting would discard what somebody edited in
the volume, on the restart after they edited it. Everything is decoded and
checked before anything is written: these files are created once and never
replaced, so a run that wrote the first and refused the second would leave a
deployment holding a file that correcting the variable can no longer change.

**A model the container can reach.** A container's `localhost` is the
container, so the Ollama default that is right on a laptop finds nothing in
one. `compose.yaml` runs JingClaw and Ollama as two services with a volume
each and no published model port; the alternative, and the better one on a
Mac, is the host's own Ollama at `host.docker.internal:11434` — Docker Desktop
proxies that to a server listening only on loopback, so nothing on the host
has to be opened up, and the host's Ollama can use a GPU that Docker cannot
reach. On Linux the same arrangement needs `--add-host=host.docker.internal:host-gateway`
and `OLLAMA_HOST=0.0.0.0`, because there the name resolves to a real address
and a loopback-only server is not on it.

**The files meant to be edited, on any start.** `PERSONA.md` and `AGENTS.md`
were created by `--init` and by nothing else, so every deployment that was not
started by hand — a container, a service, anyone who ran `jingclaw` and never
read about the flag — ran with neither. The reason for creating them rather
than documenting them ("a file that exists is a file somebody edits; one they
have to know to create is one that stays absent") reached only the people who
already knew. The daemon now writes them on every start, after whatever the
environment supplied and never over a file that exists, so a redeployment
finds them already in the volume.

**Web reading in the container, up to the line the licence draws.** The image
is built on `python:3-slim` and installs the `cloakbrowser` package into that
python, so `web.enabled = true` starts instead of refusing — a container that
cannot import it is a wall rather than a missing package, because an image
carries what it carries. (An earlier version bolted a venv onto debian-slim
and a symlink into it resolved back to the system python, losing the package;
the python base has no venv and no symlink, so neither does the bug.) The
Chromium the wrapper drives is *not* in the image and must not be: it is
licensed free to use and not to redistribute, and a published image containing
it is redistribution. It is fetched on the first page read into
`CLOAKBROWSER_CACHE_DIR`, pointed at the volume so the ~200MB arrives once
rather than every time the container is replaced. The container check asserts
both halves — the wrapper importable, and no browser-sized binary in the image
— because "just bake it in and make the first read faster" is exactly the kind
of change that gets made later by somebody who never read the licence.

**An agent can propose a skill, and a person still decides.** Installing a
skill used to be operator-only at the CLI, because a skill is standing
instructions placed in front of the model — text it will follow in every
future session. The agent now has two tools: skill_stage fetches a pinned
commit into a staging area that steers nothing (network_read, so where an
operator is present the agent may fetch and look), and skill_activate installs
a staged skill (remember, the level every attended profile stops for). The
activate approval is built from the staged bytes, not the model's claim: it
shows the source, the exact commit, a digest of the whole directory, the size,
the instructions themselves, and says plainly that this becomes standing
instructions — because a description alone is the author vouching for their own
skill. The invariant underneath is a test rather than a claim: the permission
engine has no skill input, so the most permission-hostile skill anyone could
write decides no exec_command call differently, and a tool cannot lower the
level it is judged at.

**A streamed answer with a table is said once.** While an answer is written
it grows in one Discord message. When it finishes and a table is to be drawn,
the drawn-table path now finishes that growing message in place with what came
before the table — the same shape as an answer with no table — and posts the
picture and what follows after it. An answer that opens with a table has
nothing at the front to finish the message into, so the growing message is
taken down rather than left beside a picture of itself. Either way the answer
is released, so a later one cannot extend it by mistake. Three mutations are
caught: never releasing, never finishing in place, and leaving a table-first
answer standing.

**Tool servers the prompt names in one line.** Every declared tool costs
every request its schema, and one MCP server can carry forty — ssh-manager on
the author's machine exposes 37, playwright 24 — so a handful of servers put a
hundred schemas in front of the model a turn. A server marked `defer = true`
registers its tools as before and declares none of them: the prompt names the
server once, and the model reaches a tool through `tool_search` and
`tool_load`, the shape Claude Code uses for its own MCP tools and JingClaw
already used for skills. What a session has loaded is read from its log — a
`tool_load` that completed without error — and declared on every later
request, so a daemon restarted mid-session reaches the same set; a load the
registry rejected loads nothing, and a tool that stopped being deferred is not
declared twice. Found on the way: the declarations were computed once per run
and reused for every request in it, so nothing loaded in one turn could have
reached the next.

**The footer names every tool that did the work.** A worker's steps stay out
of the conversation, and rightly, but the tools it ran were run on the
parent's behalf. Its calls now count towards the parent's record as well as
its own, so the line under an answer says `mcp_zhtw_zhtw` or `skill_load`
rather than only `investigate`.

**A worker's work shows on the parent's line.** A worker now carries the
parent's delivery target, and the projector routes its still-going statuses —
thinking, working on a tool — under the parent's run id, so the one line under
the conversation says `mcp_zhtw_zhtw …` while the worker runs it. Nothing
else of a worker's reaches the channel: its text is the parent's tool result,
and its ending must not take the parent's line down. Found on the way: the
projector's tool records were behind the target check, so the footer fold
never ran for a worker in production; recording now happens first. The
working line also names what a server's tool is working on — `text`,
`content`, `input` are subject keys now — and only those: a tool's other
arguments may be a credential, and the line is posted to a room.

**A console channel is the terminal console, for somebody not at the
machine.** A room bound as a console used to see only the finished tool calls
of runs that happened in it. It now mirrors every run's feed — the message
that started it, the run, each tool going out and coming back, the answer —
in the terminal console's own vocabulary (`console.Describe` draws both, so
the two never disagree), for a run in a public room as much as its own. The
public room sees none of it. What a console must never show is a command
having run from it, and the check for that is now "no finished exec that did
not fail, and no output", because the call going out with its arguments is
exactly what a console is for.

**A turn says who sent it, and the way it came in.** The line this machine
writes in front of a person's turn carried only the time. In a room several
people type in, the model answered everybody as one person, because nothing it
was shown told it otherwise. The line now reads
`[2026-09-04T22:46:07+08:00 · from <@1234…> (doeshing) on discord via the
gateway]` — who, written the way the platform addresses them, `<@id>` on
Discord, because the id is stable and a reply can ping it where a display
name is neither; the way in, so a turn typed at
this machine says "this machine" and a schedule says which one, and neither
is dressed up as a person nobody named. From the event and nothing else, for
the same reason the time is: rebuilt on every request, it has to be the same
line every time or the cached prefix changes for nothing. The prompt's
contract now explains the bracketed line — it never had, and the model had
been guessing what the time meant; with a sender on it, guessing wrong means
answering the wrong person.

## Not done

**Built but nothing uses it**

- `ShellFor` in `internal/tool/builtin/exec.go`. See the hole below.

**Known holes**

- ~~**On Windows, the credentials are readable by anybody on the
  machine.**~~ Closed. Windows has no POSIX mode, so `os.Chmod` there only
  toggled the read-only bit and `Stat` reported 0666 for every readable
  file — which did not weaken the check, it emptied it, and the daemon
  refused its own token besides. `internal/fsperm` now expresses owner-only
  as a chmod on Unix and as a protected DACL on Windows, granting the owner
  and nobody else. `internal/discovery`, `internal/secret`, `internal/home`,
  `internal/config` and the MCP session store write through it, and the load
  path reads the DACL back rather than a mode it cannot. The cases that
  assert an exposed file is refused now expose their fixtures with a real
  Everyone-readable ACL, because a mode a DACL cannot express proves nothing
  there.

- ~~**On Windows, stopping a process leaves its descendants running.**~~
  Closed. Each process is assigned to a job object created with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` — started suspended and resumed once
  it is in the job, so no child escapes into the window before the
  assignment — and stopping terminates the job, which takes the whole tree.
  That ends the hang it caused as well: a killed process's children no
  longer inherit a live output pipe, so `cmd.Wait` sees EOF instead of
  waiting on a copy that never does. `internal/winjob` is shared, so the
  daemon's own parts under `internal/cli/supervise` contain their trees the
  same way — untested on Windows, since the containment test there is
  Unix-only, but riding on the mechanism `internal/process` does verify.

- **Trust does not survive the run it was earned in.** A tool that returns
  somebody else's words lowers the trust of everything written after it in
  that run. It does not follow the words out of the run: the agent reads a
  page, writes what it said into a file, and the human approves that write —
  approving "write this file", not "this content is trustworthy". A later run
  reads the file back with `read_file`, which returns no foreign content, and
  what the page said is now something the agent may make a standing
  instruction. The taint is per-run and the workspace is not, so any tool that
  persists is a way across. Closing it means provenance on the bytes rather
  than on the run, which is a larger change than the one that made run-level
  trust work.

- ~~**MCP OAuth.**~~ Written, and checked against a stub that runs the whole
  authorization-code flow. The transport authorizes twice and the two are
  routed by `state`, which is how a single-use exchange came to be attempted
  twice; failures are remembered so the second attempt is refused rather than
  retried.

- ~~**A stale client resolves the old way.**~~ Fixed by there being one
  binary: a client cannot be built from a different revision than the daemon
  it talks to when they are the same file. The supervisor also starts its
  parts from `os.Executable()` rather than the name on PATH, so an installed
  copy and a freshly built one cannot be mixed.

- ~~**Nothing is sandboxed.**~~ There is now seatbelt on macOS and Landlock on
  Linux, in `internal/sandbox`. Approval stops what should not be done; the
  sandbox stops what is being done from reaching further than it said, which
  is a different thing — an approved `npm test` no longer reads `~/.ssh`.
  Landlock's network rules need ABI 4, and a policy asking for something the
  kernel cannot promise is refused up front rather than quietly downgraded.

- **A tool's declared effects are not this call's effects.** The lines under
  "This will" come from the tool's static capabilities, not from the arguments
  in front of the person: every `exec_command` reads the same four whatever it
  is about to run. Conservative, and safe while every approval is one-time —
  but it means those lines carry no information, which is how people learn to
  click through them. If a remembered "allow always" is ever added, this stops
  being a blemish and becomes the hole, because what somebody remembers is
  something they never actually read.

- **`ShellFor` is built and unused.** It settles which shell a future shell
  mode would run. Nothing calls it, so the compound-command bypasses other
  agents have shipped cannot happen here: `exec_command` takes a program and
  an argument list and never spawns a shell. The day that changes, per-segment
  authorization stops being unnecessary and becomes required.

- **Content pasted by the operator.** A tool that returns somebody else's
  words now lowers the trust of everything written after it in that run, so a
  page the agent read cannot become something it believes. What is still not
  covered is the operator pasting third-party text into their own turn: there
  is no tool call to attribute it to. OpenClaw documents the same limit —
  "the current runtime does not propagate content origin within an owner
  turn" — and has not closed it either.

**Windows, in general**

Its tests ran for the first time on 2026-09-02 and 28 of them failed. None
do now. Sixteen were checks that assumed the machine they were written on —
event ids from a millisecond clock, a temporary-directory warning that knew
only Unix names, font substitutions asserted on a machine whose typeface has
the glyph. The rest were the real defects above: the credentials, the
process trees, two about path semantics — `/etc/passwd` is not an absolute
path there, so the case is written with the workspace's own volume now, and
a reserved device name reaches outside any directory, so `read_file("CON")`
is refused where it used to open the console device — and one where the
test's own Windows counting command started at one and dropped the line it
then went looking for.

Linux and macOS are green, the container image is built and published from
Linux, and the Windows tests now pass too. Two things a run here does not
cover, and saying so is more use than a matrix entry that does not: the race
detector needs a C toolchain this machine has not got, and ConPTY is still
an honest pipe rather than a terminal. Whether that adds up to "supported"
is a heading this does not claim on its own.

**Never exercised against the real thing**

- **Telegram.** The adapter has run only against a stub that answers whatever
  it is asked, so a wrong method name or a missing field would pass. It is
  written from the documented API and has never been near the real one, which
  needs a bot token this session must not ask for
- The console channel has had no message typed into it; that needs somebody's
  own account, not the bot's
- `openai_compat` has run against Ollama's `/v1` and nothing else. vLLM,
  LM Studio and llama.cpp are still stand-ins written from documentation
- The 429 handling was rebuilt from Google's documented error shape and has
  not seen a real rate limit since
- **Brave search.** Written from the documented response shape and run only
  against a stub, which answers whatever it is asked. It needs a subscription
  token this session must not ask for
- ConPTY. The Windows build reports honestly that it gave out pipes rather
  than a terminal, and that path has not been run on Windows at all

**Not started**

- **Subagents, in general.** `investigate` exists and is the bounded case: one
  question, a fixed budget, a worker that acts as itself rather than as
  whoever asked. The general shape still needs the four decisions made —
  whose run, whose approval, what context, whose budget — and none of them
  are answered by the bounded one.
- **How many pictures fit in one request.** There is a bound on how large one
  attachment may be and none on how many go in. Anthropic refuses a request
  with more than a hundred images, and refuses the whole request rather than
  the extra ones.
- **A schedule that overran has one policy.** Everything in
  `internal/schedule/due.go` coalesces: a firing missed while the last one was
  still going becomes one firing, not none and not all of them. Skip and
  catch-up are named in the plan and not written.
- OpenAI's own API. The Anthropic adapter landed; `openai_compat` covers a
  server that speaks the shape, and OpenAI itself is not separately written
- Ollama structured output (`format`), and its model-pull progress
- No client shows a running process; `list_processes` and its two neighbours
  are model-facing only

## Defects that only end-to-end testing found

Kept because the pattern is the point: every one was in an assembly seam
rather than in logic a unit test was looking at.

1. Usage events flooding the log — needed time-bounded coalescing
2. Coalescing then lost buffered text on interrupt
3. Gemini thought signatures dropped when the conversation was rebuilt
4. `NewApprovalID` nil — segfault mid-conversation
5. `agentd` never wired the projector: runs completed, Discord got nothing
6. Discord self-identity race: connected, logged fine, ignored everything
7. The storage codec did not know the compaction payload — worked in memory,
   dropped on SQLite
8. A mistyped field name on an approval was read as a deny and reported as
   success; found within minutes of writing a second client
9. The gateway kept the daemon's first address and dialled it forever after a
   restart — connected, acknowledging, delivering nothing, for nine hours
10. A tool result was matched to a call by the tool's name, so two calls of one
   tool coming back out of order put the failure on the one that succeeded;
   the runtime's own view had always used the call id and only the reference
   every client is checked against was wrong
11. The console orphaned everything it started when the terminal closed: it
   listened for interrupt and terminate and not for the hangup, and Go's
   answer to a hangup is to die without running anything deferred
12. Windows had never been built, let alone tested, and three separate
   failures were hiding one another — CRLF broke formatting, formatting ran
   before the build, and the build would not have compiled
13. Two writers for `config.toml` — the example and the environment seeding —
   so whichever ran first decided whether the startup line called a supplied
   configuration "all defaults", and it was the line an operator would read
   while looking for why their settings were ignored
14. Seeding wrote each file as it decoded the next, so an unusable persona
   left the configuration behind: created once, never replaced, and no longer
   correctable by fixing the variable the error named
16. The footer under an answer listed only the run's own tool calls, so a
   run that delegated to a worker said "investigate" where a person wanted to
   see the MCP tool or the skill that did the work — the worker's steps are
   hidden from the conversation on purpose, and its tool use went with them.
   The live line had the same gap: a worker had no delivery target, so while
   it ran an MCP tool the channel saw "thinking" and nothing else
15. A streamed answer that turned out to hold a table was said twice on
   Discord: the message it had been growing in was left standing with the
   whole answer, table written out, and the same answer was then posted again
   beside it as text, picture, text. The drawn-table path posted fresh
   messages and never finished the growing one in place the way the ordinary
   path did — found by a person reading it on a phone, where two copies of a
   long answer is the whole screen

Two were found by reading rather than running: the daemon deleting a
replacement's discovery file, and compaction summarising its own summary.

Two were found by a person using it on a phone, which no test would have:

9. Every Discord mention started a new session, so the agent had no memory of
   anything — the conversation key was the arriving message's id
10. The status line was keyed by channel, so a new run edited the line the
    previous run had left at the bottom of the previous answer

Three came out of writing a second implementation of something, which is the
same lesson in a different form:

11. Retry had never once run against Gemini. The wrapper watched `Generate`,
    and that adapter returns a lazy stream, so every retryable failure arrived
    at `Recv`. Five of twenty-eight live runs failed on rate limits, none
    retried
12. A test wrote a fixture to the path `.JingClaw` resolution had made the
    operator's real configuration, and passed while doing it. Every check now
    sets `JINGCLAW_HOME=none`, and a guard test fails if that goes away
13. The shared-fixture check named the fields it compared, in both the JS and
    the Swift client. A field added to the fixtures was therefore exempt from
    every client at once — a gap in the one place whose whole purpose is
    catching gaps. Both now fail on a field they do not know about

And two were found by a check that was passing for the wrong reason:

14. `swift test | tail` in the parity check reported the pipeline's last
    command, so it always passed; and its `[ -d macos ]` guard matched an
    empty directory
15. A process check matched "listening on 3000" in the echoed tool arguments
    rather than in the program's output, so it would have passed against a
    process that printed nothing

And one by opening the page, which nothing else would have found:

16. An approval names its id `approval_id` on the event and `id` in the
    session view and the listing. The console read only the first, so every
    approval it drew from a view keyed to undefined — two waiting approvals
    collapsed into one row, Allow sent an empty id, and the row carried
    neither the arguments nor the effects. A person opening a session where
    something was already waiting was being asked to allow a call they could
    not see and could not actually allow

And three from merging three programs into one, which is an assembly seam by
definition:

17. The client subcommands put their read-the-configuration step on the shared
    root command. Cobra runs the nearest one it finds walking up from whatever
    was invoked, so it also ran for the daemon — which reads its configuration
    from a flag of its own, and refused to start anywhere without a default
    location. Twenty-two of twenty-six checks failed on it at once, all with
    the same message, which is what made it look like a configuration problem
    rather than a wiring one
18. Under `set -e`, killing a process that has already exited ends the cleanup
    function where it stands, skipping the rest of the killing and the removal
    of the work directory. A check whose daemon never started left its Telegram
    stub holding a fixed port; the next check to want that port talked to the
    stub of a run that was over. It answered `getMe`, so the gateway logged
    that it had connected, and it had already delivered its one update, so it
    returned nothing forever. The symptom was a message that never became a
    run — which reads as a broken gateway, not as leftovers
19. The supervisor treated any part exiting as a failure and wrapped a nil
    error, so every clean `jingclaw stop` ended with
    `error: the daemon stopped: %!w(<nil>)` and a non-zero status

Two of those three came from a plain `set -e` rule that the obvious minimal
reproduction does not trigger: an AND-OR list whose *first* command fails is
exempt, so `[ -n "" ] && kill` is fine and `kill <dead pid>` is not. The first
version of the diagnosis was wrong for exactly that reason, and only running it
showed so.

---

## Delegated search: it works, and models do not reach for it

Recorded because the next person to look at `investigate` will otherwise
rediscover this, and because two of the four things below were found only by
running a real model against it.

**Two bugs, one of them mine and invisible.**

20. An edit to the sentence above `InputSchema` took the schema with it. A tool
    with no schema is not merely unvalidated: the schema is also what the model
    is shown, so its arguments have to be guessed. gemma4 called `investigate`
    four times with a field named `prompt`, was told each time that it needed a
    question — without being told what a question was called — and gave up and
    did the work itself. Nothing failed loudly. The registry compiles schemas at
    registration precisely so a broken one is caught at startup, and an absent
    one is not a broken one
21. The worker was filtered out of its own conversation, and separately could
    see the whole of the parent's. The first made it re-read its opening
    question every turn until it ran out of turns; the second meant the fresh
    context it exists for never happened. Both were in one filter that said
    "not a worker's events" where it had to say "only this run's, if this run
    is a worker"

**And a finding that is not a bug.**

Asked outright — "use investigate to find out X" — it works end to end: the
worker runs, greps, reads two ranges, and answers with evidence and line
numbers, and only that answer reaches the conversation.

Nothing reaches for it on its own. Four sessions across gemma4:31b and
nemotron-3-super, on workspaces of nine files and of 264, including a question
whose answer is exactly a conclusion — which packages append to the event log
and which only read it. The conversation made thirteen tool calls itself and
delegated none of them.

Two rewrites did not move it. The first described the cost — "when finding out
would take many reads and greps" — which is a thing a model can only know
afterwards: it starts with one glob, then one read, and there is never a moment
where it decides the search is long. The second described the shape of question
instead, which it can recognise upfront. Neither changed the behaviour.

The tool is kept. It is correct, it is cheap, and it is reachable by asking.
What is not established is that a model will find its way there by itself, and
no amount of wording in the prompt has demonstrated otherwise. Worth retrying
against a stronger model — `glm-5.3` and `glm-5.3-flash` both refused for want
of a subscription, so the strongest thing tested here is a 31B.

---

## Why the delegated search was not being used

The finding above — that nothing reaches for `investigate` on its own — has a
cause, and it was found by reading what the model actually thought rather than
by guessing at the prompt again.

In 3,892 characters of reasoning over a 264-file workspace, the word
"investigate" appears **once**, and as an ordinary English verb about what the
model is doing itself: "Let's start by investigating internal/storage/storage.go."
It then greps. The tool is never considered.

Renaming it to `delegate_search` changed nothing: twelve calls, and the word
"delegate" appears zero times in the reasoning. So the collision with a common
verb was not the cause either.

Four ways of saying "use this for that" in the prompt had failed by then. What
they have in common is that all four were static text, present from the first
turn, describing a situation that had not happened yet. None of them was news
at the moment it mattered.

**What a model cannot see is how much of its context it has spent.** It asks
for a grep, reads a file, asks for another, and at no point is there anything
to tell it this has become a long search — so the tool that exists to make
long searches cheap is one it never has a reason to reach for.

So one sentence is appended to the sixth search's result, once per run, saying
what has been spent and what `investigate` would do with the rest. It is a
fact the model has no other way to learn, delivered when it becomes true.

Measured against the same model and workspace:

| | delegated |
| --- | --- |
| long search, no notice | 0 of 2 |
| long search, notice | **1 of 2** |
| short search (4-6 calls), notice not reached | 0 of 3, correctly |

That is not a solved problem. Two runs is not evidence, and the one that did
not delegate had the same notice as the one that did. What can be said is that
the mechanism fires only where it was meant to — three short lookups went
uninterrupted — and that it is the first of five attempts to produce the
behaviour at all.

The off-by-one worth recording: the notice is written before the call it
attaches to has been recorded, so counting only the log made the threshold
mean one more than it said and the number in the sentence one less than the
truth. Invisible in the behaviour, wrong in the words, and found only because
one real run stopped at exactly six calls.

---

## Pictures in a conversation, and the one bound nothing enforces

A picture sent on the first turn is in front of the model on every turn after
it. The bytes are not in the event — an image is large and the log is replayed
each turn, so a conversation carrying copies of everything ever sent would
stop working long before the context window did — so the event holds a
reference and the bytes are read back out of the artifact store each time.

That raised a fair question: does re-sending a picture confuse the model about
when it arrived? It does not, and the reason is worth writing down rather than
rediscovering. Every provider re-sends the whole history on every request, and
each of them documents reading images from earlier turns as the ordinary case.
The model sees a position in a sequence; it has no way to know whether this is
the first time those bytes were transmitted or the tenth. What would change
the meaning is inserting the same picture again at the newest turn, which is
not what happens here.

Two things were considered and are deliberately not done.

**Labelling an image as historical.** There is no such convention at
Anthropic, OpenAI or Google, and adding one at rebuild time would edit the
conversation history after the fact and break the cached prefix — which is
currently worth 96–98% of the input on a long session. If a picture ever needs
a stable name, the place to give it one is when it first enters the log, never
on the way out. Turn numbers are the wrong name anyway: compaction and forking
do not preserve them.

**Dropping images by age.** Rejected in favour of dropping by relevance, if it
is ever needed. A picture somebody is still asking about is worth its tokens
on turn twenty; one that has become background is not worth them on turn two.
Age is a weak signal for that and a token budget is the real trigger.

**The bound nothing enforces.** Anthropic accepts at most 100 images in one
request, and there is no check anywhere that counts them. A session where
somebody posts a hundred pictures would rebuild a request the provider
refuses, and what reaches them would be the same unhelpful "something went
wrong at the model" that an empty turn used to produce. Nobody has hit it.
The fix, when somebody does, is to keep the most recent and describe the rest,
not to fail.

Sources are in the answer that produced this: Anthropic's vision guide,
OpenAI's images and prompt-caching guides, and Gemini's image-understanding
and token-counting pages.
