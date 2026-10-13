# Running JingClaw in a container

The image is the program. A volume is the deployment.

```
docker volume create jingclaw
docker run -d --name jingclaw -v jingclaw:/var/lib/jingclaw \
  ghcr.io/koukeneko/jingclaw
```

The first run against an empty volume comes up with every setting at its
default, says which credential the chat platform wants and where to put it, and
carries on without chat:

```
chat is off: the discord bot token: no credential: set DISCORD_BOT_TOKEN, or write it with mode 600 to the file that gateway.discord.token_file names (put that right, then restart: jingclaw stop, then jingclaw)
```

That message is the setup instructions. The container stays up: the daemon is
running and only the chat connection is off. In a container, "restart" is
restarting the container. At the defaults the provider is an offline stand-in
that echoes, so this proves the container and does nothing else.

## What lives where

Everything that makes a deployment *this* deployment is in the volume, mounted
at `/var/lib/jingclaw`, which is what `JINGCLAW_HOME` points at:

| | |
|---|---|
| `data/jingclaw.db` | the database: the settings, the event log (which is the source of truth), memory, schedules and channel bindings |
| `data/artifacts/` | stored output, beside the database |
| `workspace/` | what the agent may read and write |
| `skills/` | installed skills |
| `mcp-auth/` | sign-ins to tool servers that use OAuth |
| `run/daemon.json` | written at start, regenerated, never yours to edit |
| `*.token`, `*.key` | credentials, mode 600 |

The image carries a binary and the things a command needs to run. It carries
no settings, no database, no workspace and no credentials. There is nothing in
it that identifies whose deployment it is, which is what makes it safe to
publish.

The settings are not files, in the volume or anywhere. They are three documents
in the database, `config`, `persona` and `agents`, and the dashboard is the
only editor. The daemon reads them once, when it starts. A container cannot
reach the dashboard yet ([below](#the-dashboard)), so a container's settings
arrive in environment variables, on its first start, and the rest of this page
is about that.

There is no example file in the image either. The example is the binary's own
text — it is what a first start saves when nothing else is given — and it
prints it:

```
docker run --rm ghcr.io/koukeneko/jingclaw daemon --print-config > config.toml
```

The same text is [`config.example.toml`](config.example.toml) in this
repository. Edit it and give it to the first start as `JINGCLAW_CONFIG`.

There are deliberately no settings in the image. Ones shipped inside would be
saved by the first start as though the operator had chosen them, and from then
on could not be told apart from what they did choose.

### A volume from an older image

An older image kept the settings in the volume as `config.toml`, `PERSONA.md`
and `AGENTS.md`. The first start of this one imports each of them into the
database and renames it `<name>.imported`; it never deletes one. The start says
so:

```
Settings:  database (imported from config.toml)
```

A file that is there wins over a `JINGCLAW_CONFIG`, `JINGCLAW_PERSONA` or
`JINGCLAW_AGENTS` for the same document, which is what the existence of the
file always did. After that, nothing reads the files again; the renamed copy is
there to be kept or deleted by you. A `config.toml` that sets `server.data_dir`
or `server.runtime_dir` to somewhere other than where this process keeps them
is not imported: the start stops and says which variable to give, because
starting would open a different, empty database.

## Credentials

Two routes, both already supported, and they are not equally private.

**A file in the volume** — the documented default:

```
docker run --rm -v jingclaw:/var/lib/jingclaw --entrypoint sh \
  ghcr.io/koukeneko/jingclaw \
  -c 'umask 077 && cat > /var/lib/jingclaw/discord.token'
```

The names come from the settings: `gateway.discord.token_file` for a gateway,
`provider.gemini.api_key_file` for a provider, each a plain file name in the
top of the volume and never a path. Mode 600 is expected and checked.

**An environment variable** — for a one-off run:

```
docker run -d -v jingclaw:/var/lib/jingclaw \
  -e DISCORD_BOT_TOKEN=… -e GEMINI_API_KEY=… \
  ghcr.io/koukeneko/jingclaw
```

What to know before choosing it: a value passed with `-e` is in `docker
inspect` output, in the shell history of whoever started the container, and in
the process list of anything that can read `/proc`. A file in the volume is
readable by whoever can read the volume, which is a smaller set. Compose and
Kubernetes both mount secrets as files for this reason.

## Reaching a model

A container's `localhost` is the container. `base_url =
"http://localhost:11434"`, which is right on a laptop and is the default,
finds nothing in here. Two arrangements work, and which one you want depends
on where the GPU is.

### Ollama on the host

The one to prefer on a Mac, and the one to prefer anywhere the host already
has models pulled and signed in:

```
docker run -d -v jingclaw:/var/lib/jingclaw \
  -e JINGCLAW_CONFIG="$(cat config.toml)" \
  ghcr.io/koukeneko/jingclaw
```

```toml
[provider.ollama]
model = "gemma4"
base_url = "http://host.docker.internal:11434"
```

Like every seed, `JINGCLAW_CONFIG` is saved on the first start only; against a
volume that already has settings it changes nothing.

On Docker Desktop that name resolves and reaches the host, including an
Ollama listening only on `127.0.0.1` — the desktop VM proxies it, so nothing
on the host has to be opened up.

On Linux it needs both halves: `--add-host=host.docker.internal:host-gateway`
so the name resolves, and `OLLAMA_HOST=0.0.0.0` on the Ollama service so it
listens on something the container can reach. A loopback-only Ollama is
unreachable there however the name resolves.

The reason to prefer this on a Mac is the GPU. Docker cannot reach it, so a
model running in a container on a Mac runs on the CPU; the host's Ollama uses
Metal.

### Ollama as a second container

[`compose.yaml`](../compose.yaml) in the repository root is the whole stack —
JingClaw, Ollama, a volume each, no published model port:

```bash
cp .env.example .env          # the bot token goes in it
docker compose up -d
docker compose exec ollama ollama pull gemma4
```

The pull is a separate step because models are gigabytes and the image
carries none. Until it has been done, the daemon comes up without an agent and
says so by name:

```
Why there is no agent:
  - provider ollama does not serve model "gemma4"; run --list-models to see the options
```

Pull the model, then restart the container: `docker compose restart jingclaw`.

A `:cloud` model needs `ollama signin`, which wants a browser, so those are
easier on the host than in a container.

An NVIDIA host can hand the GPU to the Ollama service; `compose.yaml` has the
block to uncomment. A machine without one fails to start with it present,
which is why it is not on by default.

## Reading the web

`web.enabled = true` works in this image. Its base is `python:3-slim`, so
python3 is already on PATH, and the `cloakbrowser` package is installed into
it — which is what the daemon checks for at startup. A container that cannot
import it runs no agent, and that is a wall rather than a missing package
when there is nowhere to install anything.

Reading is still off by default. What the image changes is what happens when
somebody turns it on.

**The browser itself is not in the image, and cannot be.** `cloakbrowser` is
an MIT wrapper around a compiled Chromium that is licensed separately: free to
use, and **not to redistribute**. Putting it in a published image is
redistribution, so it is fetched by the deployment that runs it instead —
about 200MB, on the first page read.

It lands in the volume rather than the container's writable layer:

```
CLOAKBROWSER_CACHE_DIR=/var/lib/jingclaw/browser
```

which the image sets. Left in the layer it would be fetched again every time
the container was replaced. In the volume it is fetched once and outlives the
deployment that fetched it.

Two things follow from the download being real:

**The first page read is slow.** A couple of hundred megabytes arrive before
the first answer does. Everything after it is the ordinary few seconds a
browser costs.

**A deployment with no outbound access cannot do it.** Somewhere that reaches
your model and nothing else will start, validate, and then fail at the first
page. Give it a browser instead of a network: `CLOAKBROWSER_BINARY_PATH`
points at one you mounted, and `CLOAKBROWSER_CACHE_DIR` at a volume you
pre-populated somewhere with a network.

**On the licence.** The free binary covers personal and commercial use; newer
builds need a CloakBrowser Pro subscription, which a deployment supplies with
`CLOAKBROWSER_LICENSE_KEY` like any other credential. Serving third parties
wants an OEM licence. The terms are theirs, not this project's — read them
before shipping this to somebody else's users.

And `web_read` is not a way past serious bot detection either way. Cloudflare
and DataDome-class protection refuse a browser in a container as readily as
anywhere else, and the tool reports the refusal rather than inventing a page.

## A platform whose only inputs are a volume and some variables

Managed container platforms — Cloud Run, Fly, Zentring Run and the rest —
usually offer exactly two ways in: a persistent volume mounted somewhere of
their choosing, and a list of environment variables. There is no shell to copy
a file with.

Three things to set, and the first is the one that catches people.

**Mount the volume at `/var/lib/jingclaw`.** Not at `/data`, and not at
anywhere else the platform offers by default, unless you can also set the
directory's owner.

The reason is how a volume gets its ownership. Mounted over a directory the
image already has, it inherits that directory's owner — and the image creates
`/var/lib/jingclaw` owned by the user it runs as. Mounted anywhere else, the
path does not exist in the image, so the container runtime creates it owned by
**root**, and the image does not run as root:

```
$ docker run --rm -u 10001 -v somevolume:/data … touch /data/x
touch: /data/x: Permission denied
```

Pointing `JINGCLAW_HOME` at a subdirectory does not get around it: the
directory the runtime creates is mode 755, so a user who is not its owner
cannot create anything inside it either.

Where the platform will only mount at a path of its own and offers no way to
own it, the remaining option is to run as root — `--user 0` on Docker, or
whatever the platform calls it. That gives up what the non-root user was for,
which is that this agent runs commands somebody approved rather than commands
somebody wrote. Prefer the mount path.

If you do move it, the two have to agree:

| | |
|---|---|
| mount path | `/var/lib/jingclaw` |
| `JINGCLAW_HOME` | unset — the image already says it |

Out of step, everything appears to work: the daemon writes its database into
the container's own filesystem, the volume sits empty, and the sessions are
gone when the container is replaced.

**`JINGCLAW_CONFIG` carries the whole settings text.** Individual settings do
not survive the trip through an environment: the variables reach
`provider.backend` and stop, because a name like `api_key_env` makes any
deeper underscore ambiguous, and a list of tables like the channel bindings
has no spelling as a variable at all. So the text goes in whole:

```
JINGCLAW_CONFIG=[provider]
backend = "gemini"

[provider.gemini]
model = "gemini-2.5-flash"

[gateway]
platform = "discord"

[gateway.discord]
account_id = "main"

[[gateway.discord.channels]]
channel_ids = ["1477…"]
tenant_id = "…"
workspace_id = "default"
```

It is saved as the `config` document in the database on the first start, and
**only if there is none already**. The variable arrives again on every restart;
saving it again would discard whatever was changed since, on the restart after
somebody changed it. So changing the variable afterwards changes nothing, and
neither does removing it.

Text that will not parse, or that parses and would not pass the checks the
daemon makes when it starts, is refused with the problems named and nothing is
saved — not the configuration and not the other two documents either. They are
created once and never replaced, so a run that saved one and then refused the
next would leave a deployment holding a document that fixing the variable can
no longer change.

**Changing a setting later** is what the dashboard is for, and a container
cannot reach it yet. From outside, two things work:

- A variable named for one setting, `JINGCLAW_<SECTION>_<KEY>` — for example
  `JINGCLAW_PROVIDER_BACKEND` or `JINGCLAW_MEMORY_ENABLED` — is read on every
  start and wins over the saved text. It reaches the settings that have a
  section and a key and nothing deeper, for the reason above.
- Anything else means a new volume with the new text in `JINGCLAW_CONFIG`. That
  starts an empty database too: the sessions, memory and schedules of the old
  one do not come with it.

The startup line says which happened:

```
Settings:  database (created from JINGCLAW_CONFIG)
Settings:  database (created, all defaults)
Settings:  database (3 documents)
```

**`JINGCLAW_PERSONA` and `JINGCLAW_AGENTS` carry the other two documents.** The
configuration is not the only thing a deployment has to bring. `persona` says
who the agent is and `agents` says how the project works, and on a platform
whose only inputs are a volume and some variables they are as unreachable as
the configuration was: they are written in the dashboard. A deployment started
without these variables has neither. Nothing is made in their place, and the
agent runs without that text.

```
JINGCLAW_PERSONA=# Who you are

Answer briefly. Say when you are unsure.
```

They follow the same rule as `JINGCLAW_CONFIG`: saved on first start, never
saved over one that is already there. Nothing validates their contents,
because a persona has no shape to be wrong.

| Variable | Becomes | Checked as |
|---|---|---|
| `JINGCLAW_CONFIG` | the `config` document | TOML, and the daemon's own checks |
| `JINGCLAW_PERSONA` | the `persona` document | — |
| `JINGCLAW_AGENTS` | the `agents` document | — |

**A whole document through a single-line form: `base64:`.** Some platforms
give you one input box per variable and drop the newlines, and a persona
without its line breaks is a different persona. Prefix the value with
`base64:` and the rest is decoded before it is written:

```bash
printf '%s' "$(cat PERSONA.md)" | base64 | tr -d '\n'   # paste with base64: in front
```

The prefix is required rather than guessed. Short markdown is often valid
base64 by accident, so a value that merely looks encoded is written as it
reads. A value that announces `base64:` and then does not decode is refused
and the daemon does not start — the alternative is a persona full of line
noise that nobody notices for a week. All three variables take the prefix,
and `JINGCLAW_CONFIG` is checked as TOML after decoding.

### The whole thing, assembled

Nothing above needs a shell in the container. This is a complete deployment —
settings, persona, standing instructions and a credential — started from one
command.

**From a shell, the files go in as they are.** `-e VAR="$(cat file)"` carries
the newlines through unchanged, so there is nothing to encode:

```bash
docker volume create jingclaw

docker run -d --name jingclaw \
  -v jingclaw:/var/lib/jingclaw \
  -e DISCORD_BOT_TOKEN="$DISCORD_BOT_TOKEN" \
  -e GEMINI_API_KEY="$GEMINI_API_KEY" \
  -e JINGCLAW_CONFIG="$(cat config.toml)" \
  -e JINGCLAW_PERSONA="$(cat PERSONA.md)" \
  -e JINGCLAW_AGENTS="$(cat AGENTS.md)" \
  ghcr.io/koukeneko/jingclaw
```

**A web form is where `base64:` starts to matter.** A managed platform gives
you three input boxes instead of a command line, and many of them keep only
the first line of what you paste. The same three values, encoded so that one
line is all they need:

```bash
for file in config.toml PERSONA.md AGENTS.md; do
  printf '%s -> base64:%s\n\n' "$file" "$(base64 < "$file" | tr -d '\n')"
done
```

Paste each line's value, `base64:` prefix included, into the box for that
document's variable. A box that does take newlines needs none of this — the
prefix is what says a value is encoded, so an unencoded one is simply used as
it reads.

Then check what the first run made of it:

```bash
docker logs jingclaw | head -12
```

```
Settings:  database (created from JINGCLAW_CONFIG)
```

`(created, all defaults)` on that line means the variable did not arrive, and
the deployment is running on the example. `(created from …)` appears once, on
the run that saved the document; every later start shows how many documents
there are, because by then they are whatever is in the database.

**Credentials stay variables.** `DISCORD_BOT_TOKEN`, `GEMINI_API_KEY` and the
rest are read from the environment by name, so they need not go in the
settings at all — which is the better arrangement here. The settings are in the
database in the volume, with their last twenty versions, and the volume is a
backup away from somewhere else. A header or an environment value typed into
`mcp.servers` is in that text as typed; `pass_env` names a variable instead and
keeps the value out of it.

### It is a worker, not a web service

The thing to check before choosing a platform. JingClaw's HTTP API is how its
own clients reach it — the console, the CLI, the gateway — and it can run
programs, so `server.addr` is refused unless it is on the loopback interface.
There is no port to publish, no `EXPOSE` in the image, and nothing for a
platform's health check to open.

That rules out anything request-driven. A platform that routes traffic to your
container, waits for it to bind `$PORT`, or stops it between requests will
report an unhealthy deployment however well it is running. What works is the
kind of service a platform calls a *worker*, a *background service* or a
*private service*: always on, no ingress, with a disk.

Which leaves four requirements, and a platform either offers all of them or is
the wrong shape for this:

| | |
|---|---|
| a persistent volume | the event log is the program's memory |
| mounted at `/var/lib/jingclaw` | or owned by uid 10001 wherever it lands |
| one instance, never zero | one writer for the database, and a run in progress is in memory too |
| no HTTP health check | there is nothing listening for it |

**And the model has to be somewhere the container can reach.** A PaaS cannot
see the Ollama on your laptop, and `compose.yaml`'s second service is not
available where you do not control the compose file. Two ways out:

A hosted provider — Gemini, Anthropic, or anything speaking the OpenAI shape —
which is one `[provider]` section and a key in a variable.

Or Ollama's own hosted service, which speaks the same API as the local daemon
with a credential attached:

```toml
[provider]
backend = "ollama"

[provider.ollama]
model = "gemma4:31b-cloud"
base_url = "https://ollama.com"
api_key_env = ["OLLAMA_API_KEY"]
```

The model name is the deployment's, not the machine's, so the same
configuration works on a laptop pointed at a local daemon and on a platform
pointed at the hosted one.

### The dashboard

The dashboard is a page the daemon serves on the address of its API, and inside
the container that is loopback. `docker exec jingclaw jingclaw dashboard`
prints a link, and only something inside the container could open it: a port
published with `-p` does not reach a loopback address, the port is a free one
chosen at each start (`server.addr` defaults to port 0), and `server.addr` is
refused unless it is on loopback. There is no supported way to reach it from the
host yet.

So the dashboard's work is done another way in a container:

- **Settings and the two instruction documents** are seeded through the
  environment, on the first start, as above.
- **Credentials** are variables, or files written into the volume as above.
- **Reasons the agent did not start** are in `docker logs`, and
  `docker exec jingclaw jingclaw status` shows `repair` as the mode. The daemon
  says "Fix it in the dashboard"; in a container that advice cannot be followed.
  Most of what causes it is the environment — a missing credential, a model
  the provider does not have, a sandbox the machine cannot provide — and is put
  right there, followed by restarting the container. A saved setting that is
  itself the cause can be overridden with a variable named for it, as above.

### Two settings on the platform, not in the settings

**Do not let it scale to zero.** JingClaw holds a socket open to the chat
platform and a run in progress is a run in memory as well as in the log.
Scaling to zero disconnects it and cuts a run mid-way. Minimum instances: 1.

**One replica.** The daemon owns a SQLite database on a volume, and two of
them on one volume is the failure this project is built to avoid rather than
the load-balancing it looks like.

## A directory on the host instead of a volume

The image runs as uid **10001**, so a bind-mounted directory has to be
readable and writable by it:

```
mkdir -p ~/jingclaw && sudo chown 10001:10001 ~/jingclaw
docker run -d -v ~/jingclaw:/var/lib/jingclaw ghcr.io/koukeneko/jingclaw
```

Running as root instead would avoid this and is not offered. The agent runs
commands somebody approved, which is a different thing from commands somebody
wrote, and root in a container is one careless flag away from root on the
host.

## Backing up

Everything is in the volume, and the database in it is SQLite in
write-ahead-log mode. While the daemon runs, recent commits can be in
`data/jingclaw.db-wal` and not yet in `data/jingclaw.db`, so a copy of
`jingclaw.db` alone from a running container can leave them out or catch the
file between two states. There are two ways to do it that are right.

**Stopped.** `docker stop` the container and copy the whole volume. Nothing is
writing, and the `-wal` and `-shm` files beside the database, if there are any,
go with it. Copy them together or not at all.

**Running.** Ask SQLite for the copy. It takes a consistent one while the
daemon writes, and the image has python3, which has SQLite's backup built in:

```
docker exec jingclaw sh -c 'umask 077 && python3 -c "
import sqlite3
live = sqlite3.connect(\"/var/lib/jingclaw/data/jingclaw.db\")
live.backup(sqlite3.connect(\"/tmp/jingclaw-backup.db\"))
"'
docker cp jingclaw:/tmp/jingclaw-backup.db .
docker exec jingclaw rm /tmp/jingclaw-backup.db
```

That is the database only. Stored output is in `data/artifacts/`, and the
workspace, skills, `mcp-auth/` and the credential files are not in the database
at all.

**To restore** a database copy, stop the container and put the copy in place as
`data/jingclaw.db` with no `-wal` or `-shm` file beside it, owned by uid 10001
and mode 600. SQLite replays a write-ahead log it finds beside a database, and
one left by the database being replaced does not belong to the copy.

Treat a copy like the credentials. It holds every conversation, and the
settings with their last twenty versions each: a secret typed into an MCP
server's headers or environment is in every version saved while it was there,
until twenty later saves push those out.

## Stopping it

`docker stop` signals PID 1 and nothing else, waits, and then kills. Two
things follow from that, and both are handled here rather than left to be
discovered.

**PID 1 is tini, not the supervisor.** With the supervisor as PID 1 a
grandchild orphaned by its parent exiting is re-parented to it and stays a
zombie: Go waits on the children `os/exec` started and on nothing else. This
agent runs shells, build tools and whatever somebody approved, all of which
fork, so those accumulate until the process table is full and the container
can start nothing at all — silently, because everything looks fine until it
does not. Measured, not assumed: one orphan is one zombie, and the check
beside this file is what keeps it that way.

`docker run --init` does the same job from outside. It is not needed here and
does no harm if passed: it puts docker-init in front of tini.

**Give it longer than ten seconds.** Docker's default stop timeout is 10s and
the supervisor allows each part 15s to close its database and let go of its
runs. A part that takes its time is killed by Docker before the supervisor has
finished waiting for it.

```
docker run --stop-timeout 30 …
```

```yaml
services:
  jingclaw:
    stop_grace_period: 30s
```

**What that gets you today is less than it says.** The signal reaches the
supervisor, and the supervisor started its parts on the context that signal
cancels, which kills them at once rather than waiting for them. Seen on this
image: the daemon is gone within milliseconds of `docker stop`, with no
shutdown line in `docker logs`, and `run/daemon.json` is left in the volume.
SQLite recovers its write-ahead log the next time the database is opened, and
a process that is killed, as against a machine that loses power, loses nothing
it had committed.

The leftover file is the problem, as far as it was traced. A container's process
ids repeat from one start to the next, so the file can name the new daemon's own
id before the new daemon has written its own, and the supervisor appears to
take that for the daemon being up: it starts the gateway against the old port,
and the run ends with the gateway's "connection refused". Two of three new
containers started on a volume stopped that way failed like this here, and a
stopped container started again with `docker start` came up. Removing
`run/daemon.json` from the volume while no container is running cleared it once.
This is a known hole, in [`STATUS.md`](STATUS.md#not-done).

## The sandbox

Off by default, in the image as everywhere else, and left off here.

Turning it on (`[sandbox] enabled = true`) does not work in the image: the
daemon comes up without an agent and says so. The sandbox always hides the
deployment's own directory from a command, and Landlock — all Linux has —
grants access rather than removing it, so it cannot do that. A sandbox somebody
believes in and does not have is worse than none, so no agent runs, rather than
commands that run unconfined. `JINGCLAW_SANDBOX_ENABLED=false` brings the agent
back without touching the saved settings. See
[`configuration.md`](configuration.md#sandbox).

## What this is not

The daemon binds loopback inside the container and its credential is in the
volume. This publishes an image, not a hosted service: putting the control
plane on a network is a different piece of work with a different threat model.

## Tags

`ghcr.io/koukeneko/jingclaw`

| tag | what it is |
|---|---|
| `latest` | the default branch |
| `main` | the same, named |
| `1.2.3`, `1.2` | a release |
| `sha-<commit>` | exactly one commit |

Built for `linux/amd64` and `linux/arm64`.
