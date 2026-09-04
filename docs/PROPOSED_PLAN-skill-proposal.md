# Agent-proposed, human-approved skill install

**Status: done.** All six steps landed; see the commits and `scripts/verify-skill-proposal.sh`.

## Context

Today a skill is installed only by an operator at the CLI:
`jingclaw skills install git:<url>#<commit>[:path]`. The agent can read an
installed skill (`skill_load`) but cannot install one. The ask: let the agent
propose installing a skill from a repo and let a human approve it through the
same approval rail (a button in Discord/console) instead of dropping to the
CLI.

The tension, and the thing that shaped the design (CloakGPT research,
2026-09-03): an approval button shows the *action*; a skill install approves a
*payload* — hundreds of lines of fetched instructions that then steer every
future session and every user of the deployment. A one-tap "allow" on a
description the author wrote is integrity theatre, not an informed decision.

**The real privilege level is blast radius — staging fetched bytes versus
activating standing instructions — not the interface.** That is the principle
the whole design follows.

## High-level design

Two tools, because there are two genuinely different acts:

1. **`skill_stage(source)`** — fetch the pinned commit, verify it is a real
   skill, land it in `skills/.staged/<name>/`. Nothing is put in front of the
   model. The approval for this shows the repo and the exact commit — facts,
   not the author's prose. Low blast radius: bytes on disk that steer nothing.

2. **`skill_activate(name)`** — move a staged skill into `skills/<name>/` and
   record it in the lockfile. `remember`, not `high_impact`: high_impact is
   Deny in every profile (CLI-only), while remember is the level every attended
   profile stops for — and it is the honest fit, since a skill is standing
   instructions believed across sessions like a memory. The approval's preview is built
   from the *staged bytes already on disk* — no network in the render path —
   and shows the whole honest surface: name, repo, exact commit, a digest of
   the entire installed tree, size, the description line, and a prominent
   blast-radius warning. This is where a human decides to trust standing
   instructions.

This maps onto the existing runtime, where approval precedes tool execution:
each approval's preview is computed from data already in hand (call arguments
for stage; staged disk contents for activate). Neither needs the network while
rendering.

The CLI keeps its one-shot `Install` (Stage then Activate) for the operator
who has the source in front of them.

### The load-bearing invariant

A skill can never grant or lower a capability. It is text in the prompt; the
permission engine is Go outside the prompt. A skill body that says "shell
commands are pre-approved for this skill" must change nothing. This is
structurally true today and must stay true — a test locks it.

### What the activate approval must show

Minimum honest surface (CloakGPT: description + hash alone is theatre — the
hash proves nobody swapped what you approved, not what you approved):

- skill name (parsed from the fetched SKILL.md, not the agent's claim)
- source repository and subpath
- exact 40-char commit
- digest of the **entire installed tree**, not only SKILL.md
- payload size (lines / bytes)
- the description line
- a prominent "installs persistent instructions, affects future sessions"
  warning
- a path to review the exact SKILL.md bytes (console `show`, or the file in
  the volume)

### Threat model and what an in-chat approval can and cannot do

- Branch/tag repointed → mitigated by the exact-commit pin (already enforced).
- Staged content swapped before activate → mitigated by the tree digest in the
  activate approval and the lockfile.
- Prompt injection in the skill body → **not** mitigated by approval; mitigated
  by the invariant above (a skill cannot escalate) and by every later approval
  still showing the exact action.
- Repo already compromised, or a malicious exact commit → not mitigated by the
  button; needs a human reading the source. This is why the full surface
  includes a review path and why deployment-wide trust is a deliberate act.
- Name collision / typosquatting / silent replacement → refuse silent
  replacement; re-approve on any new commit and show a diff.

## Step-by-step implementation

1. **skill package: split Stage / Activate** (backend, no gateway). `Stage`
   fetches+verifies to `.staged/<name>/` and returns a `Staged` describing the
   real metadata. `Activate` moves staged → installed and locks. `Install`
   becomes `Stage` then `Activate`. **← this increment.**
2. **Whole-tree digest.** Add a digest over the installed directory, not only
   SKILL.md, and carry it in `Staged`/`Locked`.
3. **Tools.** `skill_stage` (network_read) and `skill_activate` (remember).
   Staged skills are never offered to the model.
4. **Rich activate preview.** `previewOf` for `skill_activate` reads the staged
   bytes and renders the surface above with the blast-radius warning.
5. **Invariant test.** An activated skill's text cannot change a permission
   decision.
6. **Console/Discord.** The activate approval shows the rich preview; a console
   verb lists and inspects staged skills.

## Verification strategy

- Unit + mutation tests for Stage/Activate and the tree digest (increment 1).
- A test that a staged skill is not in the catalogue and not offered until
  activated.
- The invariant test (step 5).
- `scripts/verify-skill-proposal.sh`: an end-to-end run where the agent stages
  from a repository served over `git://` (a local path is refused on purpose),
  the staged skill steers nothing, activation stops for an approval, and only
  answering it installs the skill and puts it in the catalogue.

## Open decision

Whether chat may activate at all, or may only stage while activation stays an
operator act (CLI / console). Default taken here: chat may activate, but only
through the full-surface `high_impact` approval; deployment-wide trust is the
deliberate, most-privileged step.
