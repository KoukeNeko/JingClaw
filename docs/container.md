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

## A platform whose only inputs are a volume and some variables

Managed container platforms — Cloud Run, Fly, Zentring Run and the rest —
usually offer exactly two ways in: a persistent volume mounted somewhere of
their choosing, and a list of environment variables. There is no shell to copy
a file with.

Three things to set, and the first is the one that catches people.

**`JINGCLAW_HOME` has to match the mount path.** The image puts the
deployment at `/var/lib/jingclaw`. Mount the volume there and there is nothing
else to say. Where the platform picks its own path — `/data` is a common
default — set the variable to the same path:

| | |
|---|---|
| mount path | `/data` |
| `JINGCLAW_HOME` | `/data` |

Get these out of step and everything still appears to work: the daemon writes
its database into the container's own filesystem, and the volume sits empty
until the container is replaced and the sessions are gone.

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

TOML that will not parse is refused and nothing is written. The file is
created once and never replaced, so a broken one would be a deployment that
cannot start and cannot be corrected by fixing the variable.

The startup line says which happened:

```
Config:  /data/config.toml (created from JINGCLAW_CONFIG)
Config:  /data/config.toml (created, all defaults)
Config:  /data/config.toml
```

**Credentials stay variables.** `DISCORD_BOT_TOKEN`, `GEMINI_API_KEY` and the
rest are read from the environment by name, so they need not go in the file at
all — which is the better arrangement here, because the file is in the volume
and the volume is a backup away from somewhere else.

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
