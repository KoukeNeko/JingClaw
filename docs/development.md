# Working on JingClaw

Building it, and the checks that decide whether a change is finished.

## Build

Requires Go 1.26+ and [buf](https://buf.build) 1.72+ (only for changing the
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
