# Deferred tool servers

## Context

Every registered tool's name, description and JSON schema is sent to the
model on every request (`toolDeclarations()` → `provider.Request.Tools`). One
MCP server can carry dozens of tools — on the author's own machine,
ssh-manager exposes 37, playwright 24, pdf-viewer 9 — so a handful of servers
puts a hundred schemas in front of the model each turn. Three things follow:
the prompt prefix a provider is paid to cache grows by the whole catalogue,
every turn pays for schemas it will not use, and a model choosing among a
hundred tools chooses worse.

Configuration scales fine — one more `[[mcp.servers]]` block. The tool list is
what does not.

## Design

The pattern already exists twice: Claude Code defers most MCP tools and loads
a schema through `ToolSearch` when one is needed, and JingClaw's skills put a
catalogue in the prompt and hand the instructions over through `skill_load`.
Tool servers get the same shape.

**Registered but not declared.** A server marked `defer = true` still
registers every tool — lookup, validation and execution are unchanged — but
its specs are *deferred*: left out of `Registry.Specs()` and so out of the
declarations sent to the model. `Registry.DeferredSpecs()` lists them.

**A catalogue, one line per server.** The prompt names each deferred server
once — name, how many tools, what they are for — with the instruction to
search and load. The prompt is assembled at startup, which is when servers
connect, so nothing about the stable prefix changes turn to turn.

**Two internal tools.** `tool_search(query)` returns the deferred tools whose
name or description match, with descriptions only. `tool_load(name)` returns
a tool's full schema and, from that turn on, that tool is declared. Both are
`internal`: they read a registry and change nothing on the machine.

**Loaded is read from the log, not held in memory.** The conversation is
rebuilt from the event log on every request, so what has been loaded must be
too. `declarationsFor(run)` scans the session for `tool_load` calls that
completed without error — the `ToolCallRequested` carries the arguments, the
`ToolCallCompleted` says it succeeded — and declares those deferred tools in
addition to the ordinary set. No new event kind: the tool's own call is the
record. A daemon restarted mid-session reaches the same set.

Workers keep their read-only allowlist; a loaded deferred tool is not on it
and does not reach a worker.

**Per server, off by default.** `defer` is a setting on each `[[mcp.servers]]`
block. A deployment with one small server changes nothing; an operator with
several large ones turns it on for those. Deferral is about cost, and the
operator is the one paying it.

## Step-by-step

1. `tool.Spec.Deferred`; `Registry.Specs()` excludes deferred, `DeferredSpecs()`
   lists them. `config.MCPServer.Defer` → `mcp.ServerConfig.Defer` → set on
   the adapted spec. Additive; no behaviour changes with the flag off.
2. `declarationsFor(run)` adds deferred tools the session has loaded, derived
   from the log.
3. `tool_search` and `tool_load` in `builtin`, registered by the daemon.
4. The prompt catalogue: `prompt.Environment.DeferredServers`, one line each.
5. Documentation: `[mcp]` in `configuration.md`, and STATUS.

## Verification

- Registry: a deferred tool is absent from `Specs()`, present in
  `DeferredSpecs()`, and `Execute` still runs it.
- Runtime: with a deferred server, `declarationsFor` omits its tools; after a
  successful `tool_load` in the session log it includes exactly that one; a
  `tool_load` that errored loads nothing; a worker never gets it. Measured by
  `estimateRequestOverhead` before and after: the figure drops by the deferred
  schemas and rises by one when one is loaded.
- End to end: the real helper MCP server, deferred; the fake provider scripts
  `tool_search` → `tool_load` → the tool; the tool runs, and the request that
  ran it carried one deferred schema, not all of them.
- Mutation on each: declare everything anyway; ignore the log; count an
  errored load; leak to a worker.
