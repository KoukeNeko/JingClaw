<h1 align="center">JingClaw</h1>

<p align="center">
  <strong>An AI agent that keeps working when you close the window.</strong><br>
  It runs on your machine, answers in Discord, and remembers every session
  across restarts, crashes, and whichever client you attach from next.
</p>

<p align="center">
  <a href="https://github.com/KoukeNeko/JingClaw/actions/workflows/ci.yml"><img alt="CI status" src="https://img.shields.io/github/actions/workflow/status/KoukeNeko/JingClaw/ci.yml?branch=main&style=for-the-badge&logo=githubactions&logoColor=white&label=CI"></a>
  <a href="https://github.com/KoukeNeko/JingClaw/pkgs/container/jingclaw"><img alt="Container image" src="https://img.shields.io/badge/GHCR-amd64_%C2%B7_arm64-2496ED?style=for-the-badge&logo=docker&logoColor=white"></a>
  <img alt="Go 1.26" src="https://img.shields.io/badge/GO-1.26+-00ADD8?style=for-the-badge&logo=go&logoColor=white">
  <img alt="Preview" src="https://img.shields.io/badge/STATUS-PREVIEW-E8A33D?style=for-the-badge">
  <a href="LICENSE"><img alt="MIT licence" src="https://img.shields.io/badge/LICENSE-MIT-4CAF50?style=for-the-badge&logo=github"></a>
</p>

<p align="center">
  <strong>English</strong> · <a href="README.zh-TW.md">繁體中文</a>
</p>

<p align="center">
  <a href="#getting-started">Getting started</a>
  · <a href="docs/using-it.md">Using it</a>
  · <a href="docs/configuration.md">Configuration</a>
  · <a href="docs/container.md">Docker</a>
  · <a href="docs/architecture.md">Architecture</a>
  · <a href="docs/roadmap.md">Roadmap</a>
</p>

---

## Getting started

```bash
go install github.com/KoukeNeko/JingClaw/core/cmd/jingclaw@latest
jingclaw
```

That is the whole thing. It creates `~/.jingclaw` if there is none, starts the
daemon and the chat gateway together, and gives you a console to watch them
from. Run it again from another terminal and it says so rather than starting a
second one.

The first run has no model and no chat account yet, so it tells you what it
wants and where to put it. Set those in `~/.jingclaw/config.toml`, start it
again, and talk to it in Discord.

```bash
jingclaw status      # running, and where
jingclaw stop        # stop the one that is running
```

Prefer a container? `docs/container.md` — the image is on GHCR for amd64 and
arm64, and carries no settings, no database and no credentials of its own.

## What it does

**Work survives the client.** Sessions, runs and every event go to SQLite as
they happen. Close the terminal, restart the daemon, come back tomorrow from a
different machine: a run that was waiting for an approval is still waiting, and
answering it hours later works.

**A person decides what matters.** Reads run unattended. Anything that changes
the workspace, runs a command, or reaches the network stops and asks — in the
channel, with the command it is about to run written out. The pause is durable
too.

**The conversation lives in chat.** Discord is where you talk to it; the
terminal is where you watch it and answer it. It says what it is doing while it
does it — which file it opened, which page it read — and the answer arrives as
it is written.

**One binary, two processes.** The gateway holds a bot token and keeps a socket
open to the internet. The process that owns your shell, your workspace and the
event log does not go down with it.

```
Discord ──→ gateway ─┐
                     ├──→ daemon ──→ SQLite
CLI, console ────────┘
```

## Project status

**Preview.** It does what is described above, every day, on macOS and Linux,
and its tests pass on Windows too.

What that does not promise yet: configuration and storage formats may change
between versions; the daemon listens on loopback only, so there is no remote
access. Windows is newer than the rest and two things there are still
untested rather than working — the race detector and a real terminal — which
[`docs/STATUS.md`](docs/STATUS.md) sets out rather than rounding up.

`docs/STATUS.md` is the honest account — what works, what is missing, and the
defects found along the way. [`docs/roadmap.md`](docs/roadmap.md) has the
milestones.

## Documentation

| | |
|---|---|
| [Using it](docs/using-it.md) | approvals, memory, images, tool servers, long sessions |
| [Configuration](docs/configuration.md) | every setting, and why it is what it is |
| [In a chat channel](docs/discord.md) | what a conversation looks like from Discord |
| [Docker](docs/container.md) | the image, the volume, and where credentials go |
| [Architecture](docs/architecture.md) | the shape of it, and the rules it was built by |
| [Development](docs/development.md) | building it, and the checks a change has to pass |
| [Status](docs/STATUS.md) | what is done, what is not, and what went wrong |

## Requirements

Go 1.26+ to build. A provider — Gemini, Ollama, Anthropic, or anything that
speaks the OpenAI shape — and a Discord bot token if you want the chat half.
macOS, Linux and Windows — with the caveats in the status above.
