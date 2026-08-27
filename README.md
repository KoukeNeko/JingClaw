# JingClaw

A local-first, durable agent runtime you can attach to from anywhere.

`agentd` owns every piece of state — sessions, runs, the event log. The
control clients are projections of it, so closing one never stops work that is
already running, and reattaching from somewhere else picks up exactly where
you left off.

```
                    proto/jingclaw/control/v1
                              │
        ┌─────────────┬───────┴───────┬─────────────┐
        ↓             ↓               ↓             ↓
   SwiftUI        WinUI 3          Web UI        Go CLI
   (macOS)       (Windows)      (no GUI needed)
        └─────────────┴───────┬───────┴─────────────┘
                              ↓
                      Connect-RPC / loopback
                              ↓
                           agentd
```

## Status: M0 — walking skeleton

M0 proved the skeleton conducts: protocol, runtime, streaming event loop, and
the boundary between them. State now persists to SQLite and survives a
restart, including runs orphaned by a crash. There is still a deterministic
fake provider and no tools.

Deliberately absent until the rest of M1: real model providers, file and shell
tools, the permission engine, MCP, and every GUI.

| Milestone | Scope |
|---|---|
| M0 | walking skeleton — done |
| **M1a** | SQLite ✓, real providers, file/shell tools, permissions |
| M1b | `gatewayd` + Discord |
| M2 | SwiftUI, WinUI 3 and web clients at parity |
| M3 | subagents, replay, sandboxing, remote fleet |

## Layout

```
proto/    the contract, shared by every client
core/     Go daemon and CLI
macos/    SwiftUI client       (M2)
windows/  WinUI 3 client       (M2)
web/      embedded web UI      (M2)
```

Generated code lives in `core/gen/` and **is committed**, so Xcode, Visual
Studio and the web build never need `buf` installed. CI regenerates and fails
on any drift.

## Build

Requires Go 1.26+ and [buf](https://buf.build) 1.72+ (only for changing the
proto files).

```bash
cd core && go build -o bin/agentd ./cmd/agentd && go build -o bin/agent ./cmd/agent
```

## Try it

Three terminals. First, the daemon:

```bash
core/bin/agentd --dev-fake
```

It binds an ephemeral port on loopback and writes a `0600` discovery file
holding the address and a bearer token; the CLI finds it from there. State
lives in `jingclaw.db` under the user config directory, or wherever
`--data-dir` points.

Then create a session and follow it:

```bash
core/bin/agent session create
core/bin/agent attach <session-id>
```

And in a third, send a turn:

```bash
core/bin/agent send <session-id> "測試訊息"
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
core/bin/agent attach <session-id> --after 3
```

Only events 4, 5 and 6 arrive. Restarting the daemon changes nothing: the
session, its runs and the sequence all continue where they were.

## Design rules

These are load-bearing. Breaking one is a design change, not a refactor.

1. **The event log is the history.** Message lists in a UI are projections of
   it, never the source.
2. **Sessions and runs are separate.** A session is a conversation; a run is
   one execution within it.
3. **Clients are projections.** They may not decide tool permissions, judge
   whether a run has finished, or keep their own model catalog.
4. **The runtime never sees the wire format.** Translation lives only in
   `internal/control`.
5. **Not every caller is trusted.** `Run.origin` and `TrustLevel` exist from
   M0 so the gateway plane can arrive without a schema migration.
6. **Loopback is not authentication.** Any web page can reach `127.0.0.1`, and
   this API will grow a shell.
7. **A dead process must not strand a run.** Startup resolves runs left live by
   a crash and records the outcome as an event, so a reconnecting client learns
   it the same way it learns everything else.

`internal/architecture` asserts rules 3–4 mechanically: the CLI cannot import
the runtime, and the runtime cannot import generated protobuf.

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
