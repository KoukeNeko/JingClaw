# How JingClaw is put together

The shape of the thing and the rules it was built by. Read this when you want to change it, not to use it.

## Layout

```
proto/    the contract
core/     everything: daemon, gateway, CLI
scripts/  the checks
docs/     plans and research
```

Inside `core`:

```
cmd/jingclaw/   the one executable
internal/cli/   its subcommands: daemon, gateway, client, supervise, service
internal/       the runtime, the store, the tools, the adapters
gen/            generated from proto, and committed
```

Generated code is committed, so a build never needs `buf` installed. CI
regenerates and fails on any drift.

## What is one conversation

A Discord **thread** is a session. Outside a thread, the **channel** is: two
people mentioning the bot in `#agent` are continuing one conversation, and
opening a thread is how somebody says they want a separate one.

This is worth stating because getting it wrong is invisible from the code and
obvious from the channel. Keying on the arriving message's id — which is what
this did until it was noticed — gives every mention a session of its own, and
what that looks like is an agent with no memory: ask it something, say "go
ahead", and it has never heard of you.

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
