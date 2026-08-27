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

State persists to SQLite and survives a restart, including runs orphaned by a
crash. Real models answer through the Gemini provider, and the agent loop runs
tools: it searches a workspace, reads what it finds, acts on what a failed call
told it, and stops for a human before it changes anything.

Deliberately absent until the rest of M1: targeted edits and shell execution,
context compaction, MCP, and every GUI.

| Milestone | Scope |
|---|---|
| M0 | walking skeleton — done |
| **M1a** | SQLite ✓, providers ✓, read tools ✓, permissions ✓, edits, shell |
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

Or against a real model:

```bash
core/bin/agentd --provider=gemini --model=gemma-4-31b-it --workspace .
```

`--workspace` is the only directory tools can reach. Paths are resolved and
symlink-checked against it before any I/O, so neither traversal nor a symlink
pointing elsewhere gets out.

Reads run unattended; anything that modifies the workspace stops for a
decision:

```bash
core/bin/agent approvals <session-id>
core/bin/agent approve <approval-id>     # --session to allow that tool for the session
core/bin/agent deny <approval-id>
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
8. **Provider deltas are coalesced before they are persisted.** A model that
   streams a few characters at a time must not turn every keystroke into a
   database write — but interrupting a reply still flushes what was already
   generated, because losing it would be worse than never batching.
9. **A model's cooperation is not a security control.** The system prompt says
   what the agent may reach, but the workspace boundary is enforced in code and
   tested by forcing the calls a well-behaved model would decline to make.
10. **A failed tool is an observation, not an error.** Structured codes and a
   suggested next step go back to the model; "exit status 1" leaves it retrying
   the same call until the budget runs out.
11. **Permission and correctness are separate.** Approval answers "may you touch
   this"; the write rules answer "do you know what you are touching". A human
   saying yes does not let the agent overwrite a file it never read.
12. **A paused run is not an orphan.** Waiting on a human is a legitimate state,
   so startup leaves it alone instead of failing it.
13. **Credentials never reach a log.** They load from an environment variable or
   a mode-600 file into a type whose `String`, `Format` and `MarshalJSON` all
   redact.

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
