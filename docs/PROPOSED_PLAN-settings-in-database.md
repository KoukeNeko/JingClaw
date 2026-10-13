# Settings live in the database; the dashboard is the only editor

**Status: built, except what this plan lists as not done.** Decided with the
operator; each phase below ends with `go build ./... && go vet ./... && go test
-race ./...` green. What is left is decision 13 (a restart RPC, hot reload,
redaction of `mcp.servers` headers and environment values in `GetConfig`,
reaching the dashboard from inside a container) and the two lists under *What is
not done* in the SkillService section. The independent review and the
real-browser pass that end phase 4 are not claimed here. What is true of the
system now is in [`STATUS.md`](STATUS.md#done), [`container.md`](container.md)
and [`roadmap.md`](roadmap.md); where this plan and the code differ, the code is
right.

## What changes, in one paragraph

`config.toml`, `AGENTS.md` and `PERSONA.md` stop being files. They become three
versioned **documents** in the daemon's SQLite database, and the dashboard is
the only way to edit them. The *text* of each document is stored unchanged —
the comment-preserving patcher, `Preview`, `Candidate(bytes)`, unknown-key
preservation and hash-based optimistic concurrency all operate on bytes and keep
working. Credential files and skills stay files (they are secrets and fetched
directories, not settings) but become manageable from the dashboard.

## Decisions (already made — do not reopen)

| # | Decision |
|---|---|
| 1 | Three documents: `config`, `agents`, `persona`. Stored as UTF-8 text rows; last 20 versions kept per document (replaces the `.bak.1–3` ring) and can be restored from the dashboard. |
| 2 | `--config`, `config.EnsureFile`, `config.SeedFromEnvironment`'s file writes, `config.DefaultPath`, `config.Replace` and its backups, `writeInstructionFiles`, the stub `AGENTS.md`/`PERSONA.md`, `home.Dir.ConfigFile()` are **removed**. No flag or env var points the daemon at a settings file any more. |
| 3 | `server.data_dir` and `server.runtime_dir` say where the database and the discovery file are, so they cannot live in the database. They become **flag/env only** (`--data-dir`, `JINGCLAW_SERVER_DATA_DIR`, `JINGCLAW_SERVER_RUNTIME_DIR`, `JINGCLAW_RUNTIME_DIR`). Text containing them is refused at edit time, and ignored if one is found in stored text. |
| 4 | The three seed variables `JINGCLAW_CONFIG`, `JINGCLAW_PERSONA`, `JINGCLAW_AGENTS` keep their meaning (whole document, optional `base64:` prefix, all-or-nothing, **never overwrite an existing row**) but the target is the database row. |
| 5 | First start after upgrade imports an existing `config.toml`, `AGENTS.md`, `PERSONA.md` from the deployment directory, then renames each imported file to `<name>.imported` (never deletes). See *Ensure*. |
| 6 | Settings are still read once at start. The dashboard shows "saved, not running yet" for all three documents by comparing the stored hash with the hash the running process started with. The claim "instruction files are re-read on every run" is **wrong today** and is corrected everywhere. |
| 7 | **Repair mode**: if the settings cannot be used (unparsable, fail `Validate`) or the agent cannot be built, the daemon does not exit. It serves the dashboard with only the services needed to fix the problem, reports why, and runs no agent. There is no restart RPC; the operator runs `jingclaw stop` then `jingclaw`. |
| 8 | The gateway process stops reading a file. It asks the daemon for its slice of settings (`GatewayIngressService.GetGatewaySettings`, gateway-scoped). It still never reaches `ConfigService`. |
| 9 | Client CLI commands stop calling `config.Load`: they need only the runtime dir (`config.Candidate(nil)` = defaults + env). `jingclaw mcp list/login/logout` read the MCP servers from the running daemon (`GetConfig.text`). |
| 10 | Skills (`~/.jingclaw/skills/`) and credential files (`gemini.key`, `discord.token`, …) remain files. The dashboard manages both. Credentials are **write-only**: nothing ever returns a value, length, hash or prefix. |
| 11 | Database-held management (schedules create, memory list/forget, channel bindings) is added to the dashboard — RPCs already exist. |
| 12 | A gateway that cannot start for a *configuration* reason (missing credential, invalid gateway settings) exits with code 78 and the supervisor keeps the daemon and dashboard up ("chat is off: <reason>"). Any other gateway exit still ends the supervisor, as today. |
| 13 | Not done, and said so: restart RPC; hot reload; redaction of `mcp.servers[].headers/env` in `GetConfig` (pre-existing, documented limitation); reaching the dashboard from inside a container. |

## Storage

Migration `core/internal/storage/sqlite/migrations/0020_documents.sql`:

```sql
CREATE TABLE documents (
  name TEXT PRIMARY KEY CHECK (name IN ('config','agents','persona')),
  text TEXT NOT NULL, hash TEXT NOT NULL,          -- hash = '' iff text = ''
  version INTEGER NOT NULL, source TEXT NOT NULL,  -- file | env | default | web
  updated_at INTEGER NOT NULL) STRICT;
CREATE TABLE document_versions (
  name TEXT NOT NULL, version INTEGER NOT NULL, text TEXT NOT NULL,
  hash TEXT NOT NULL, source TEXT NOT NULL, saved_at INTEGER NOT NULL,
  PRIMARY KEY (name, version)) STRICT;
```

`storage.DocumentStore` (embedded in `storage.Store`; implemented by `sqlite` **and** `memory`; contract tests in
`storagetest/conformance.go` run against both):

```go
const DocumentVersionsKept = 20
var ErrDocumentChanged, ErrDocumentVersionNotFound error
type Document struct{ Name, Text, Hash, Source string; Version int64; UpdatedAt time.Time }
Document(ctx, name) (Document, error)                       // absent -> zero value with Name set, nil
PutDocument(ctx, name, text, source, expect string, at time.Time) (Document, error) // CAS on hash, one statement/tx
DocumentVersions(ctx, name) ([]Document, error)             // newest first, includes current
DocumentVersion(ctx, name, version) (Document, error)
SeedDocuments(ctx, seeds []DocumentSeed, at time.Time) ([]string, error) // one tx, never overwrites, returns names written
```

* `hash` = sha256 hex of the text, equal to `config.Hash`; **empty text hashes to `""`**, so an absent row and a
  cleared document are the same "started from nothing" token (`control.fileHash` parity). A cleared document is
  an empty *row*, never a deleted one, so an env seed does not resurrect it.
* `PutDocument(expect="")` succeeds when the row is absent or its hash is `""`; otherwise it needs `hash == expect`.
  Zero rows affected → `ErrDocumentChanged`. Single-statement CAS, correct even across processes.
* Add `fsperm.Restrict` on the database file (create it `0600` before opening so `-wal`/`-shm` inherit it). It now holds
  settings, including MCP header/env secrets. Applies to existing databases at open (idempotent).
* Add a read-only opener only if something needs it (`--print-prompt`, `--list-models` run the startup path and may
  simply do what the daemon does; decide by reading them). Do not make `sqlite.Open` skip migrations.

## Startup order (daemon `run`)

1. Parse flags. `--print-config` prints `config.Example`, unchanged, no DB. `--print-paths` stays side-effect free
   and DB-free (`TestAskingWhereThingsAreCreatesNothing`). `--init` creates the directories only.
2. Bootstrap pass: `config.Candidate(nil)` (defaults + env) plus `--data-dir` → database path and runtime dir. Nothing is created.
3. Open the database (`0600`).
4. **Ensure** (below), in one transaction.
5. Read the three documents in one read. Snapshot their hashes (`running`). `config.Candidate(text)`, force
   `Server.DataDir/RuntimeDir` to the bootstrap values, apply flag overrides, `Validate`.
6. Build the agent. On any failure in 5–6 that is about settings or the environment (not "database cannot open"),
   enter **repair mode** instead of `os.Exit(1)`. A database that will not open stays fatal.
7. Standing instructions come from the snapshot taken in step 5 and are passed into `buildPrompt` as parameters; the
   bytes hashed are the bytes the prompt was built from. `readStandingInstructions` and file reads disappear.

The logger is built after step 5 (bootstrap level `info` until then).

### Ensure (replaces `SeedFromEnvironment` + `EnsureFile` + `writeInstructionFiles`)

Per document, only if no row exists; first applicable source wins:
1. legacy file `<home>/{config.toml,AGENTS.md,PERSONA.md}` (source `file`) — wins over env, as the old file-existence check did;
2. the env var (source `env`), decoded as today (`base64:` prefix; undecodable aborts before anything is written; `JINGCLAW_CONFIG` must parse as TOML);
3. `config` only: `config.Example` (source `default`). `agents`/`persona`: **no row** (absent = empty; `prompt.Build` already skips empty text).

Rules:
* Everything is written by one `SeedDocuments` call. A crash before commit changes nothing.
* **Bootstrap guard:** if a legacy `config.toml` sets `server.data_dir` or `server.runtime_dir` to something that is not what this
  process is already using (env/flag/default), refuse to start with
  `set JINGCLAW_SERVER_DATA_DIR=<x>` (or runtime). Otherwise the first start after upgrade opens an empty default database —
  the "one deployment quietly becomes two" failure `verify-home.sh` guards against.
* Legacy text is imported **as-is, without Validate**: if it is broken the operator fixes it in repair mode. Env seeds still fail loudly when unparsable.
* After the commit, for each document seeded from a file whose current sha256 still equals the stored hash:
  `os.Rename(X, X+".imported")`; if the target exists or the rename fails (read-only mount) log WARN and continue.
  A file whose hash differs is left alone with a WARN. `*.bak.*` are never touched.
* Idempotent: with rows present nothing is read from disk; a crash between commit and rename is finished on the next start.
* Startup line becomes `Settings: database (created from JINGCLAW_CONFIG | imported from config.toml | created, all defaults | N documents)`.

## Repair mode

Trigger: step 5/6 failure above. Behaviour:
* The same listener, tokens, `AuthMiddleware`, `RequireLoopbackHost`, `/ui/`, `/ui/pair`, scope map and discovery file as normal mode
  (**no new auth path, no bypass flag**). The repair listener is forced to loopback regardless of `server.addr`.
* Mounted: `ConfigService`, `BrowserService`, and (when they exist) `CredentialService`. **Not** mounted/started: Session, GatewayIngress,
  Artifact, Memory, Channel, Skill services, provider, tools, MCP children, runtime, schedule watcher, gateway token. Repair mode executes nothing.
* The discovery file gains `mode` (`"repair"`; omitted in normal mode). It carries no gateway token. Written as soon as the repair mux is up.
* `ConfigService.GetAgentState` → `{mode, problems[], started_at}` (reuses `ConfigProblem`: key, value, why, fix; `source` = settings|startup).
  `GetConfig` must work on **unparsable** stored text: it returns `text` and `parse_error` with empty `file`.
  `ReplaceConfigText(text, hash)` replaces the document with raw text after the same check `CheckConfig` runs.
* Supervisor: after the daemon is ready it reads `mode`. In repair mode it does not start the gateway or console, prints
  `agent not running: <first problem>… run: jingclaw dashboard`, and keeps running until stopped. `jingclaw status` shows the mode.
* What the daemon mounts is one list (`cli/daemon/serve.go`). What each credential may call is another, `servicesForScope`
  (`control/auth.go`), written out on purpose so that a new service is reachable by nobody until someone decides.
  `TestTheTableOfWhatACredentialMayReachNamesEveryServiceTheProtosDefine` holds the table to the protos (a service nobody
  added, a mistyped name), and a test proves that, with a valid control token, repair mode answers 404 for SessionService.
* The dashboard asks `GetAgentState` first and shows a repair screen (problems + the settings editor + raw text) instead of polling session RPCs.

## Control plane

`config.proto` (all additions; `buf breaking` must pass, no field removed or renumbered):
* `GetConfigResponse`: keep 1–5 (`path` is now empty), add `text=6`, `parse_error=7`, `running_hash=8`, `pending_restart=9`, `version=10`.
* `GetInstructionFileResponse`: add `running_hash`, `pending_restart`, `version`. `PutInstructionFileResponse` and `WriteConfigResponse`: add `pending_restart`, `version`.
* New RPCs: `ReplaceConfigText`, `ListDocumentVersions`, `GetDocumentVersion` (enum `SettingsDocument`), `GetAgentState`.
* `edits` touching `server.data_dir`/`server.runtime_dir` → a `ConfigProblem` ("says where the settings are kept; give it as <env> or <flag>").
* `storage.ErrDocumentChanged` maps to `FAILED_PRECONDITION` next to `config.ErrChangedUnderneath` (same user-facing meaning).
* `GatewayIngressService.GetGatewaySettings` returns `platform`, `log_level`, discord `{account_id, token_env[], token_file, max_messages, max_attachment_bytes, tables_as_images}`,
  telegram `{account_id, token_env[], token_file, max_upload_bytes, api_base}`. Built from the daemon's *running* config. No channels, users or approvers. Gateway scope; `ConfigService` stays control-only (`auth_test.go` rows).

Comments in `config.proto` and `control/config.go` that say "the file stays the source of truth" are rewritten.

## Clients

* `client/main.go`: drop `--config` and the `config.Load` pre-run; runtime dir from `config.Candidate(nil)` (so `JINGCLAW_SERVER_RUNTIME_DIR` keeps working) and `JINGCLAW_RUNTIME_DIR`.
* `client/mcp.go`: servers from `ConfigService.GetConfig.text` → `config.Candidate`. Needs the daemon up; the error says so.
* `gateway/main.go`: dial first, fetch `GetGatewaySettings`, convert with `settingsFromProto` to a `config.Config` so `newAdapter`, `Selected()`, `LogLevel()` are unchanged; drop `--config`; keep `--platform`/`--account`. Exit 78 on configuration failure.
* `supervise`: treats gateway exit 78 as non-fatal (see decision 12), honours discovery `mode`.

## Credentials (web-managed files) and skills

Specified in the second half of this file by the phase that builds them (`CredentialService`, `SkillService`), with these fixed points:
* Credential slot enum, never a path from the caller; the file name comes from the stored settings (or the slot default) and must match
  `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, not collide with `config.toml`, `AGENTS.md`, `PERSONA.md`, `workspace`, `data`, `run`, `log`, `bin`, `skills`, `mcp-auth`, `*.bak.*`, `*.imported`; resolved path must stay directly inside the real deployment directory; leaf checked with `Lstat` (absent or regular non-symlink, small, one line); written `CreateTemp` + `fsperm.Restrict` before bytes + fsync + rename; **no backups**; status reports `ENVIRONMENT | FILE | NONE | BLOCKED` and modes, never content, length or hash; compare-and-set uses mtime, not a content hash.
* Skills: `Stage` → review (full SKILL.md, file list, tree digest, what it replaces) → digest-bound `Activate` → `Remove`; one package mutex around the mutating tail; remove refuses non-directories, `installed.json` and dot names; URL passwords rejected, credentials redacted in responses; git stderr truncated; `.replacing` renamed to a dot name; agent activation records the lock too; symlinks rejected after checkout.
* Both: control scope only; both pending-restart aware.

## Scripts, containers

* All `scripts/verify-*.sh` stop passing `--config` and stop relying on a settings file. Seed with `JINGCLAW_CONFIG` (and `JINGCLAW_PERSONA`/`JINGCLAW_AGENTS`) exported before the first start; mid-script rewrites become env overrides (`JINGCLAW_MEMORY_ENABLED=false`, `JINGCLAW_PROVIDER_BACKEND=gemini`). Drop the `runtime_dir`/`data_dir` lines (they equal the home defaults).
* `verify-dashboard.sh`, `verify-config.sh`, `verify-home.sh`, `verify-seed-from-environment.sh`, `verify-container.sh` are rewritten against the API/DB.
* `Dockerfile`: drop the `config.example.toml` COPY. `compose.yaml`: comments say "first start only". `docs/container.md` rewritten.

## Phases

1. **Settings core** — storage + sqlite perms; config package; ConfigServer on documents; protos; daemon bootstrap, Ensure, snapshot, repair mode; gateway/client/supervisor; scripts. *Gate:* build, vet, `go test -race ./...`, `verify-config/api/home/dashboard/seed-from-environment` pass.
2. **Credentials + skills** services and hardening.
3. **Dashboard** — repair screen, raw text + history/restore, pending banner for all three documents, copy corrections, skills area, credential controls, schedules create, memory, bindings.
4. **Docs and the rest of the scripts**, then independent review and a real-browser pass.

## CredentialService (built in phase 2)

`proto/jingclaw/control/v1/credential.proto`. Control scope only (`servicesForScope`), mounted in normal mode and in
repair mode from the one service list in `cli/daemon/serve.go`. `ListCredentials`, `PutCredential` (with optional
`expect_modified_at`), `DeleteCredential`, `RestrictCredentialFile`. Nothing it returns has a field for a value, a
size, a prefix, a hash or a mask; `TestNothingAReplyCanCarryIsAnythingButAStateOfTheSlot` holds the field list.

### Slots, as the code reads them

| slot | file setting (default name) | env setting (default names) | need | read by |
|---|---|---|---|---|
| `provider.gemini` | `provider.gemini.api_key_file` (`gemini.key`) | `api_key_env` (`GEMINI_API_KEY`, `GOOGLE_API_KEY`) | required if `provider.backend = gemini` | daemon |
| `provider.anthropic` | `provider.anthropic.api_key_file` (none) | `api_key_env` (none; see below) | required if `anthropic` | daemon |
| `provider.ollama` | `provider.ollama.api_key_file` (none) | `api_key_env` (none) | optional if `ollama` | daemon |
| `provider.openai_compat` | `provider.openai_compat.api_key_file` (none) | `api_key_env` (none) | optional if `openai_compat` | daemon |
| `memory.embedding` | `memory.embedding.api_key_file` (none) | `api_key_env` (none) | optional if `memory.enabled` and `memory.embedding.model` is set | daemon |
| `web.search` | `web.search.key_file` (`brave-search.token`) | `key_env` (`BRAVE_SEARCH_TOKEN`) | required if `web.enabled` and `web.search.backend = brave` | daemon |
| `gateway.discord` | `gateway.discord.token_file` (`discord.token`) | `token_env` (`DISCORD_BOT_TOKEN`) | required if `gateway.platform = discord` | gateway |
| `gateway.telegram` | `gateway.telegram.token_file` (`telegram.token`) | `token_env` (`TELEGRAM_BOT_TOKEN`) | required if `gateway.platform = telegram` | gateway |

There are no fixed variable names: every variable that can supply a credential is one a `*_env` setting lists. None of the
eight file settings can be given by a `JINGCLAW_*` variable (they have two dots). `provider.anthropic.api_key_env` has no
default although the example config shows `["ANTHROPIC_API_KEY"]` as one; a slot with no default file name answers
`PutCredential` with FAILED_PRECONDITION naming the setting and a name to give it.

### What is checked, and where

* The name rule is `config.CredentialFileProblems` (`config/credentials.go`), used by `Config.Validate` for exactly these eight
  settings and by the service. A config that fails it starts in repair mode with the problem's Why and Fix, like any other setting.
  A value that is not a plain name is never quoted back (`"(not shown)"`): it is as often a key pasted into the wrong place
  as a typing mistake, and a problem is printed, logged and returned by `GetAgentState`. `*.imported` is refused with the
  other kept names: those files are the only copy of the settings they held.
* `config.CredentialEnvProblems` (also in `Validate`) does the same for the eight `*_env` lists: an entry has to look like
  a variable name (`^[A-Za-z_][A-Za-z0-9_]*$`) and one that does not is not quoted back. A pasted key made only of letters,
  digits and underscores is a valid name and is not caught. `ListCredentials.env_names` lists, and the status looks up, only
  entries that look like variable names, because the service reads saved text that was never validated.
* Two settings naming one file is **not** a `Validate` failure: a provider and its embeddings on one endpoint can share a key
  file and have worked. `config.SharedCredentialFileProblems` is asked by the service only, which then refuses to write,
  delete or restrict either slot (its Fix says to give each its own name).
* The service gets the daemon's own `withFlags` (`NewCredentialServer(..., candidate, ...)`), so `need` and `restart_needed`
  follow `--provider` and `--dev-fake` as the daemon does, not the backend the saved text names.
* Containment, the leaf check, the value check and the write are `secret.Directory` (`secret/files.go`).
* Status is `secret.Inspect`, which is `secret.Load`'s own walk (`resolve`) with the value dropped. It reads the file to tell a
  whitespace-only file (EMPTY, read as absent) from a PRESENT one, and returns only the verdict and the modification time.
  The read is bounded: a file larger than `secret.MaxFileBytes` (16 KiB) is UNREADABLE with a detail, for `Load` as well, so
  the daemon refuses it at start and the status agrees with what `PutCredential` and `DeleteCredential` accept as a credential
  file. A file that is more than one line is still read by `Load` and still refused by `PutCredential` and `DeleteCredential`
  (not done: making `Load` refuse it would change what the daemon accepts).
* `restart_needed` is true when the next start would use something other than what this process started with and the slot is
  used: the file the credential is read from was modified after the process began, or this process wrote, deleted or restricted
  it. It is false when an environment variable decides, because the file is then not read. A `chmod` or `rm` done by hand leaves
  nothing to see and is not noticed. For a gateway slot the comparison is with when the daemon began.
* `env_in_use` describes the daemon's environment. A gateway is started with an environment of its own.
* `expect_modified_at` is compared at nanosecond precision, and a client has to send `modified_at` back exactly as it was
  returned: a JavaScript `Date` keeps milliseconds, and a save that went through one would be refused every time (phase 3).

## SkillService (built in phase 2)

`proto/jingclaw/control/v1/skill.proto`. Control scope only (`servicesForScope`), and mounted from the one service list in
`cli/daemon/serve.go` **only when there is an agent**: a repair-mode daemon has no agent for a skill to steer, and answers 404
for it like for every other service it does not serve. `ListSkills`, `StageSkill`, `GetStagedSkill`, `ActivateSkill`,
`DiscardStagedSkill`, `RemoveSkill`.

### The calls

* `StageSkill(source)` takes `git:<url>#<40 hex>[:path]` through `skill.ParseSource`, fetches that commit into
  `skills/.staged/<name>/` and returns what arrived. It installs nothing.
* `GetStagedSkill(name, file_path?)` returns the bytes of SKILL.md that the digest was made of, its line count, every file
  (`path`, `size`, `sha256`, `is_text`), `tree_matches` (the staged directory hashes to the recorded tree digest now), at
  most 1 MiB of one chosen file as text (cut at a character, `file_truncated` says so; the hash of all of the file is held to
  the one the listing gave it, FAILED_PRECONDITION if the file changed in between), all from one look: the listing, the
  tree check and SKILL.md are read by `skill.ReviewStaged` under the lock that Stage's last step, Activate and Discard take,
  so a skill staged again under the name cannot put one version's instructions under another's digest. SKILL.md of whatever is in the
  skills directory under that name (`installed_skill_md`, at most 1 MiB) so a page can diff. `file_path` must be a path from
  the listing; nothing read is built from the request.
* `ActivateSkill(name, tree_digest, replace)`: `tree_digest` is required and is the digest the person was shown. A
  `StagedSkill.tree_digest` and nothing else is accepted.
* `ListSkills` returns installed skills (`name`, `directory`, description, version, the digests on disk now and the ones
  recorded, `source` with the address redacted, `placed_by_hand`, `installed_at`, `drift`, `in_catalogue`), rejected ones
  (`directory`, `reason`), recorded ones that are gone (`missing`), staged ones (with `replaces`), and `restart_required`.
  `RemoveSkill` takes the `directory`, which is the name for anything installed through the service.
* Codes: `INVALID_ARGUMENT` for a source that is not the exact form (including an address with a credential in it), a name
  that could not be a directory, a missing digest, a `file_path` that is not a text file of the skill. `FAILED_PRECONDITION`
  for a source that could not be fetched or holds no usable skill, a fetch already under way, nothing staged, a digest that is
  not the staged skill's or a staged skill that changed since it was staged, a name already taken without `replace`, nothing
  installed under the name. Everything else is `INTERNAL`. The mapping is in `toConnectError`.

### `in_catalogue` and `restart_required`

`skillCatalogue()` returns the skills it listed in the prompt along with the text (`control.OfferedSkills`: name to the
description it was offered under), the agent keeps them, and `NewSkillServer` is given them. `in_catalogue` is "the disk's
skill has this name and this description in that snapshot". `restart_required` is "the snapshot is not equal to what
`skill.Installed` finds now": a skill added, removed or re-described since the start. It is true after removing a skill the
running agent was told about, and false again after a restart. A skill whose body or sibling files changed but whose name and
description did not is not a catalogue difference (`skill_load` reads the disk when asked); `drift` is where that shows.

### Hardening in `core/internal/skill`

* One package-level mutex covers Stage's last step (move into `.staged/` and write its record), `Activate`,
  `ActivateExpecting`, `Remove` and `Discard`, with `installed.json` read, changed and written inside the same critical
  section. A second, only-tried gate covers the whole of `Stage`: a concurrent Stage gets `ErrBusy` at once instead of waiting
  for a clone. There is no lock file: `TestARepositoryWithNoSkillLeavesNothing` needs the skills directory empty after a failed
  install. **The mutex is in one process.** `jingclaw skills install|remove` is another process and does not share it.
* `ActivateExpecting(name, treeDigest, replace)` compares the digest to the staged record and to the tree recomputed inside the
  mutex (`ErrStale`), refuses to put anything over a directory already at that name unless `replace` (`ErrCollision`), reads
  `installed.json` before moving anything and records the install after. `Activate(name)` (the agent's `skill_activate` and
  the CLI) keeps replacing without asking and checks no digest from outside, but now records the install too: a skill the
  agent installed used to look as if it had been put there by hand.
* `Remove` takes only a real directory (not a file, a link, `installed.json` or a dot name) and forgets the record in the same
  step. `Discard(name)` removes a staged skill. The staging record is written to a temporary file and renamed. The copy put
  aside while a replacement moves in is `.<name>.replacing`, which `Installed` does not read.
* `Drifts(lock, installed)` reports `NONE`, `EDITED`, `TREE_EDITED` (the recorded tree digest was written and never checked
  before) or `MISSING` per recorded skill; `Changed` words the same facts for `jingclaw skills list`.
* `ParseSource` refuses a password in any address and any user name in an `https` or `git` one (an `ssh://git@host` user name
  stays), a query string (where a token goes), and an address with no host after its scheme (`https:user:pw@host/x`, which
  git reads as something else); none of those refusals repeats the address. `Redact` takes the user-info out of every
  address in a text and is used for git's output, for errors, for `Source.String()` and for every source the service
  returns. That includes the login of an ssh address, which cannot be told from a token put in its place: the address
  recorded for the install has it, what a client is shown does not. It does not use `web.RedactAddress`, which finds only the first
  address in a text. git's output in an error is cut to its last 2 KiB. After a checkout, a link or anything that is not a
  plain file anywhere in the tree that lands is refused before SKILL.md is read. Git runs with `GIT_SSH_COMMAND=ssh -o
  BatchMode=yes -o ConnectTimeout=30` and with `cmd.WaitDelay`. What git prints is kept as its last 4 KiB while it runs
  (`tailWriter`), not in full, and an error carries the last 2 KiB.
* A skill name ending in `.json`, in any case, is refused: a staged skill's record is `<name>.json` beside its directory and
  installs are recorded in `installed.json`, so such a name would stand where a record is, and discarding it would delete
  one. (On a case-insensitive file system `INSTALLED.json` is the record, too.)
* A skill's directory is bounded before anything in it is read, in the walk the digest and the listing go through
  (`regularFiles`): 8 MiB a file, 32 MiB in all, 1000 files (`skill.MaxFileBytes`, `MaxTreeBytes`, `MaxFiles`), and a file
  whose name is not UTF-8 is refused like a link. SKILL.md is at most 1 MiB (`skill.MaxSkillMDBytes`), checked at Stage, so
  it is returned whole. A tree over the limits has no digest, as one with a link has none: an installed skill that grew past
  them is called edited.
* An ssh address is fetched with the keys, the configuration and the known hosts of the account the daemon runs as. OpenSSH
  takes the home directory from the account's entry, not from `$HOME`, so the empty `HOME` of the fetch does not isolate it.
  That is what an ssh remote is for, so the dashboard and the command line accept one. `skill_stage` needs no approval where
  an operator is present, so it refuses one (a person stages it).
* `skill_activate`'s approval preview says when it would replace a skill of the same name (`Staged.Replaces`).
* No refusal of private or loopback hosts. Internal git servers are legitimate and the agent's tools and the CLI already fetch
  from anywhere.

### What is not done

* What the clone writes to disk before the limits above are checked is not bounded: the whole repository, not only the path in
  it, lands in `.fetching-*`, which is removed when the fetch ends but not when the process is killed during it (nothing
  sweeps them). The number of staged skills is not bounded.
* Cancelling a fetch kills git and not the processes git started. `ConnectTimeout=30` ends an ssh that gets no banner; one that
  stalls later lives until its connection ends. No process group is used, because Windows would need a job object.
* `skill_activate` still has no digest in its arguments, so the agent path is bound only by the staged record, not by what its
  approval preview displayed.
