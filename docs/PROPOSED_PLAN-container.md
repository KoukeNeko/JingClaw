# A container image, built by CI, published to GHCR

## 1. What this is for

Running JingClaw somewhere that is not the machine it was built on, without
that machine needing Go, buf, or a checkout. The image is the deployment; a
volume is everything that makes it *this* deployment.

## 2. The settings question, which is the whole design

A deployment directory holds five things, and they are not alike:

| | what it is | in the image? |
|---|---|---|
| `config.toml` | settings, no secrets | **no** — the operator's |
| `data/` | the event log; the source of truth | **no** — must outlive the container |
| `workspace/` | what the agent may read and write | **no** — the operator's |
| `run/daemon.json` | 0600, holds live credentials | **no** — written at start, regenerated |
| `discord.token`, … | secrets | **no**, and not in a layer either |

So: **one volume, mounted at `JINGCLAW_HOME`, and nothing else.** The image
carries a binary and the things a command needs to run. It carries no
settings, no database, no workspace and no credentials, and there is nothing
in it that identifies whose deployment it is.

### Secrets

The config already has both routes, and they are already the right two:

- `api_key_env` / `token_env` — names of environment variables
  (`GEMINI_API_KEY`, `DISCORD_BOT_TOKEN`)
- `api_key_file` / `token_file` — a file under the deployment directory
  (`gemini.key`, `discord.token`), which the loader expects at 0600

Nothing new is needed. What the documentation has to say is which to use and
why: **a value passed with `-e` is in `docker inspect`, in the shell history
that started it, and in the process list of anything that can read `/proc`.**
A file in the volume is readable by whoever can read the volume, which is a
smaller set. Compose and Kubernetes both mount secrets as files for this
reason, so the file route is the documented default and `-e` is what a
one-off run uses.

### A first run with no configuration

`config.toml` is optional — the loader says so, and defaults work. So an image
started against an empty volume comes up and says what it is missing rather
than failing to parse something that is not there. The image ships
`config.example.toml` at a known path to copy from, and never a `config.toml`:
a default settings file in the image is one that silently becomes the
operator's and then silently stops being read the day they write their own.

## 3. Decisions that are not obvious

**D1 — Not `scratch`, and not `distroless`.** The binary is pure Go
(`modernc.org/sqlite`, no cgo) so either would work, and both would produce an
agent that cannot do anything: `exec_command` is most of what it is for, and
an image with no shell has nothing to run. Debian slim, plus `git` (the one
command the code invokes by name) and `ca-certificates` (without which every
provider call fails at TLS). This is a larger image on purpose.

**D2 — Not root.** The agent runs commands somebody approved, which is a
different thing from commands somebody wrote. A `jingclaw` user owns the
volume. The cost is that a bind mount from the host must be readable by that
uid, and that is documented rather than worked around by running as root.

**D3 — The sandbox is off by default, and stays off in the image.** Landlock
needs Linux 5.13 for paths and 6.7 for the network rules, and a container
runtime that does not block the syscalls. Turned on where it cannot be
enforced, the daemon refuses to run the command — deliberately, because a
sandbox somebody believes in and does not have is worse than none. So the
image does not turn it on; it documents what the host needs, and the check
below records which way it went on the runner rather than assuming.

**D4 — One container, both parts.** The supervisor already starts the daemon
and the gateway and already handles having no terminal to draw on. Splitting
them into two images would mean publishing the discovery file between
containers, which is a shared writable path for a file whose whole purpose is
to be local.

**D5 — `amd64` and `arm64`.** The machine this is developed on is arm64 and
most of what it would be deployed to is amd64.

## 4. Execution order

1. **`scripts/verify-container.sh`** — builds the image and runs it. Written
   first, fails, because there is no image.
2. **`Dockerfile`** — multi-stage: build with the Go image, run on slim.
3. **`docs/container.md`** — the two secret routes, the volume, the sandbox.
4. **`.github/workflows/ci.yml`** — a `container` job. Builds on every push
   and pull request so a broken Dockerfile is caught by the change that broke
   it; pushes to GHCR only from `main` and tags.

## 5. Verification

`scripts/verify-container.sh`, skipping itself with a reason where there is no
Docker, the way `verify-sandbox-linux.sh` does:

- it starts against an empty volume and the daemon answers
- `config.toml` in the volume is the one that is read — a setting changed
  there changes what `--print-config` reports
- a secret in a file never appears in `docker inspect`, and neither route
  appears in any image layer
- the database survives the container being replaced
- it runs as a non-root uid
- `git` and CA certificates are present, because an agent without them fails
  at the first tool call and the first model call

CI runs it on the `end-to-end` runner, which already has Docker.

## 6. Known costs

- The image is ~200MB rather than ~30MB. D1 says why.
- A bind mount needs the right uid. D2 says why.
- Nothing here makes the agent safe to expose to the internet: the daemon
  binds loopback and the credential is in the volume. This publishes an
  image, not a hosted service.
