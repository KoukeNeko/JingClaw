# Working on JingClaw

Building it, and the checks that decide whether a change is finished.

## Build

Requires Go 1.27.3+ and [buf](https://buf.build) 1.72+ (only for changing the
proto files).

```bash
go install github.com/KoukeNeko/JingClaw/core/cmd/jingclaw@latest
```

Or from a checkout:

```bash
cd core && go install ./cmd/jingclaw
```

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
logged cleanly, and ignored every message; the client subcommands leaving their
read-the-configuration step on the shared root command, so the daemon ran it
too and refused to start anywhere without a default location. None were visible
to a unit test. All were visible within seconds of running the thing.

So each script starts a real daemon, drives it the way a person would, and
checks what came out. `verify-artifacts.sh` needs a provider credential —
only a model calls tools — and skips itself when there is none.

The settings are in the daemon's database, so there is no file for a script to
write. It gives its daemon settings the way a container does: it exports
`JINGCLAW_CONFIG` (and `JINGCLAW_PERSONA`, `JINGCLAW_AGENTS`) before the first
start, against a `JINGCLAW_HOME` of its own in a temporary directory. A seed is
applied only to a database that has no such document yet, so a script that
wants different settings on a second start sets the one setting by variable
(`JINGCLAW_PROVIDER_BACKEND=gemini`) or starts from a fresh home. Most scripts
also unset the `JINGCLAW_*` variables they inherited, with the loop at the top
of `verify-config.sh`, so that the deployment of whoever runs them cannot leak
in. One that wants to check what was saved asks the control plane, because
there is nothing on disk to read.

## Development

```bash
cd core
go test ./...
go test -race ./...
```

After editing anything in `proto/`, from the repository root:

```bash
buf format -w && buf lint && buf generate
buf breaking --against '.git#branch=main'
```

The last is what CI runs against the base branch of a pull request: a field
removed or renumbered fails it, and an addition does not. `buf generate` fetches
its plugins from buf.build, so it needs the network. Without buf installed,
`go run github.com/bufbuild/buf/cmd/buf@v1.72.0` takes the same arguments.
