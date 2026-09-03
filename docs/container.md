# Running JingClaw in a container

The image is the program. A volume is the deployment.

```
docker volume create jingclaw
docker run -d --name jingclaw -v jingclaw:/var/lib/jingclaw \
  ghcr.io/koukeneko/jingclaw
```

The first run against an empty volume comes up, says which credential it
wants and where to put it, and stops. That message is the setup instructions.

## What lives where

Everything that makes a deployment *this* deployment is in the volume, mounted
at `/var/lib/jingclaw`, which is what `JINGCLAW_HOME` points at:

| | |
|---|---|
| `config.toml` | settings; optional, defaults work |
| `data/` | the event log, which is the source of truth |
| `workspace/` | what the agent may read and write |
| `run/daemon.json` | written at start, regenerated, never yours to edit |
| `*.token`, `*.key` | credentials, mode 600 |

The image carries a binary and the things a command needs to run. It carries
no settings, no database, no workspace and no credentials. There is nothing in
it that identifies whose deployment it is, which is what makes it safe to
publish.

`config.example.toml` is at `/usr/share/jingclaw/config.example.toml`. Copy it
into the volume to start from:

```
docker run --rm -v jingclaw:/var/lib/jingclaw --entrypoint sh \
  ghcr.io/koukeneko/jingclaw \
  -c 'cp /usr/share/jingclaw/config.example.toml /var/lib/jingclaw/config.toml'
```

There is deliberately no `config.toml` in the image. One shipped inside
becomes the operator's by accident, and then quietly stops being read the day
they write their own.

## Credentials

Two routes, both already supported, and they are not equally private.

**A file in the volume** — the documented default:

```
docker run --rm -v jingclaw:/var/lib/jingclaw --entrypoint sh \
  ghcr.io/koukeneko/jingclaw \
  -c 'umask 077 && cat > /var/lib/jingclaw/discord.token'
```

The names come from the configuration: `token_file` for a gateway,
`api_key_file` for a provider. Mode 600 is expected and checked.

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
carries none. Until it has been done, the daemon says so by name and stops:

```
error: provider ollama does not serve model "gemma4"; run --list-models to see the options
```

A `:cloud` model needs `ollama signin`, which wants a browser, so those are
easier on the host than in a container.

An NVIDIA host can hand the GPU to the Ollama service; `compose.yaml` has the
block to uncomment. A machine without one fails to start with it present,
which is why it is not on by default.

## What the image cannot do: read the web

`web.enabled = true` with the default backend drives a real browser, and the
image carries neither python3 nor the `cloakbrowser` package. The daemon says
so and stops rather than coming up with the tool missing:

```
error: web.backend is "browser": web: python3 is not on PATH
Reading pages drives a real browser, which needs python3 with the cloakbrowser package. Set web.enabled = false to run without it.
```

Stopping is deliberate. A deployment that quietly dropped page reading looks
from the room like a model that will not use the tool, and that is a long
afternoon.

Two ways forward. **Leave it off** — `web.enabled = false`, the default — and
the agent works on the workspace and the conversation, which is most of what
it is for. Or **derive an image** that has the interpreter, the package and a
browser for it to drive:

```dockerfile
FROM ghcr.io/koukeneko/jingclaw
USER root
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-pip \
	&& rm -rf /var/lib/apt/lists/*
# Then the cloakbrowser package and the browser it drives, per its own
# instructions — it is a separate project with its own licence, which is why
# it is not in the image above.
USER jingclaw
```

That recipe is a starting point rather than a tested one: what `cloakbrowser`
needs from a base image is its business, and browsers in containers want fonts
and shared libraries that are worth reading its documentation for.

Either way, `web_read` is not a way past serious bot detection. Cloudflare and
DataDome-class protection refuse a browser in a container as readily as
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

**`JINGCLAW_CONFIG` carries the whole configuration file.** Individual
settings do not survive the trip through an environment: the variables reach
`provider.backend` and stop, because a name like `api_key_env` makes any
deeper underscore ambiguous, and a list of tables like the channel bindings
has no spelling as a variable at all. So the file goes in whole:

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

It is written to `$JINGCLAW_HOME/config.toml` on first start, with the same
permissions a file written by hand would get, and **only if there is not one
already**. The variable arrives again on every restart; overwriting would
discard whatever was edited in the volume, on the restart after somebody
edited it. To change the configuration, change the variable and delete the
file, or edit the file and leave the variable alone.

TOML that will not parse is refused and nothing is written — not the
configuration and not the other two files either. They are created once and
never replaced, so a run that wrote one and then refused the next would leave
a deployment holding a file that fixing the variable can no longer change.

The startup line says which happened:

```
Config:  /var/lib/jingclaw/config.toml (created from JINGCLAW_CONFIG)
Config:  /var/lib/jingclaw/config.toml (created, all defaults)
Config:  /var/lib/jingclaw/config.toml
```

**`JINGCLAW_PERSONA` and `JINGCLAW_AGENTS` carry the other two files.** The
configuration is not the only thing a deployment has to bring. `PERSONA.md`
says who the agent is and `AGENTS.md` says how the project works, and both are
read from the same directory, so on a platform whose only inputs are a volume
and some variables they are as unreachable as the configuration was. Every
start writes them as empty headings when they are absent, so a container
without these variables comes up with two files to edit rather than none — but
editing a file inside a container means finding a way in, and the variables
are the way in that does not need one.

```
JINGCLAW_PERSONA=# Who you are

Answer briefly. Say when you are unsure.
```

They follow the same rule as `JINGCLAW_CONFIG`: written on first start, never
written over one that is already there, `0600`. Nothing validates their
contents, because a persona has no shape to be wrong.

| Variable | Becomes | Checked as |
|---|---|---|
| `JINGCLAW_CONFIG` | `config.toml` | TOML |
| `JINGCLAW_PERSONA` | `PERSONA.md` | — |
| `JINGCLAW_AGENTS` | `AGENTS.md` | — |

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
and the daemon does not start — the alternative is a persona file full of
line noise that nobody notices for a week. All three variables take the
prefix, and `JINGCLAW_CONFIG` is checked as TOML after decoding.

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
file's variable. A box that does take newlines needs none of this — the
prefix is what says a value is encoded, so an unencoded one is simply used as
it reads.

Then check what the first run made of it:

```bash
docker logs jingclaw | head -12
```

```
Config:  /var/lib/jingclaw/config.toml (created from JINGCLAW_CONFIG)
```

`(created, all defaults)` on that line means the variable did not arrive, and
the deployment is running on an empty example. `(created from …)` appears
once, on the run that wrote the file; every later start shows the path alone,
because by then the file is whatever is in the volume.

**Credentials stay variables.** `DISCORD_BOT_TOKEN`, `GEMINI_API_KEY` and the
rest are read from the environment by name, so they need not go in the file at
all — which is the better arrangement here, because the file is in the volume
and the volume is a backup away from somewhere else.

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

### Two settings on the platform, not in the file

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

## The sandbox

Off by default, in the image as everywhere else, and left off here.

Turning it on (`[sandbox] enabled = true`) needs Linux 5.13 for the filesystem
rules and 6.7 for the network ones, and a container runtime that does not
block the Landlock syscalls. Where it cannot be enforced the daemon refuses to
run the command rather than running it unconfined — a sandbox somebody
believes in and does not have is worse than none — so turning it on in a
container that cannot enforce it stops the agent from doing anything.

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
