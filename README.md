<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="backend/assets/meko-logo-white.svg">
    <img src="backend/assets/meko-logo.svg" width="220" alt="Meko">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/yugabyte/meko/releases/latest"><img src="https://img.shields.io/badge/Meko-release-blue" alt="Meko release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS-lightgrey" alt="Platform: Linux | macOS">
  <a href="https://docs.mekodata.ai/"><img src="https://img.shields.io/badge/docs-docs.mekodata.ai-purple" alt="Docs"></a>
  <a href="https://mekodata.ai"><img src="https://img.shields.io/badge/Meko%20Cloud-mekodata.ai-black" alt="Meko Cloud"></a>
  <a href="https://mekodata.ai/discord/"><img src="https://img.shields.io/discord/1463559014137991299?style=flat&logo=discord&logoColor=white&label=Meko&labelColor=5865F2" alt="Discord"></a>
</p>

**The Context Engine for Agentic Applications.**

Universal, self-learning context for every agent, tool, and team: collective memory, shared
knowledge, and decision traces in one governed datapack. This repo holds `meko`, which runs Meko on
your own machine: one binary, both the command line and the server, with
[Meko Cloud](https://mekodata.ai)'s MCP tools and REST API, so an agent set up for Meko Cloud works
against it. Meko is under construction.

## Start here

| I want to… | Go to |
|---|---|
| Run Meko on my machine | [Install](#install) |
| Give Claude Code, or any MCP client, Meko's memory | [Connect an agent](#connect-an-agent) |
| Run many coding sessions from a browser or my phone | [AgentK](extensions/agentk/README.md) |
| See every command | [backend/docs/cli.md](backend/docs/cli.md) |
| Work on Meko itself | [Build from source](#build-from-source) |

## Install

On Linux (x64 or arm64) or macOS 14 or newer on Apple Silicon. The repo is internal for now, so
the installer is fetched with a signed-in `gh` (or `GITHUB_TOKEN`):

```sh
gh release download --repo yugabyte/meko --pattern install.sh --output - | sh
meko install_extension agentk     # optional: AgentK, using this Meko (-- --agent kiro picks its coding agent)
```

It installs the newest release (checking its checksum) into `~/.local/share/meko/app`, links
`~/.local/bin/meko`, and runs
Meko as a login service (launchd or systemd) that restarts on failure and updates itself. The
release bundles its own Node and models, so the first start needs no network. It takes
`--version`, `--reinstall` and `--no-service` (pass them through `sh -s --`). How it all works:
[`backend/docs/install.md`](backend/docs/install.md).

## Connect an agent

`meko connect claude` adds Meko to Claude Code for all your projects (its `user` scope), and says
what it did. Meko's tools then show up as `mcp__meko__*`. The same works for other agents:

```sh
meko connect claude     # Claude Code
meko connect codex      # Codex
meko connect kiro       # Kiro CLI
meko connect opencode   # OpenCode
```

`meko connect` alone prints what any other MCP client needs: Streamable HTTP at
`http://127.0.0.1:7778/mcp`, with `Authorization: Bearer <token>`.

The REST API takes the same token:

```sh
curl -s localhost:7778/api/v1/datapacks -H "Authorization: Bearer $(meko token)"
```

## Commands

One binary under two names, as PostgreSQL has `postgres` and `psql`: `meko` runs Meko, and
`mekosh` works with what it keeps. The ones you'll use first:

| Command | What it does |
|---|---|
| `meko start` / `stop` / `status` | Start or stop the server, or see whether it's running and its links |
| `meko ui` | Open the web UI in your browser |
| `meko connect claude` | Connect Claude Code to Meko (or `codex`, `kiro`, `opencode`; alone, the URL and token for any MCP client) |
| `meko logs -f` | Follow the server's and the memory engine's logs |
| `meko update` | Install Meko's newest release now, then every extension's |
| `mekosh search "<question>"` | Search memories, the knowledge base and past conversations |
| `mekosh add_memory "<text>"` | Save a memory, word for word |
| `mekosh upload <file>...` | Add files to the knowledge base |

`meko --help`, `mekosh --help` and `<command> --help` give the rest. Every command and flag is in
[`backend/docs/cli.md`](backend/docs/cli.md).

## Extensions

**[AgentK](extensions/agentk/README.md)** is the agentic coder built on Meko. It runs many Claude
Code, OpenCode and Kiro CLI sessions from a browser or your phone, takes each task from design to
merged PR, and keeps each project's designs and learnings in a Meko datapack. Install it with
`meko install_extension agentk`; Meko then starts, stops and updates it, and serves it at
`/ext/agentk/`.

## How it works

- **One folder.** Everything lives under `~/.meko`: one SQLite file, and knowledge files' bytes
  beside it.
- **Hybrid search.** Vector similarity (sqlite-vec, with an embedding model that runs locally) and
  keywords (FTS5); knowledge-base results are reranked by Meko Cloud's cross-encoder, also run
  locally.
- **Two listeners.** Agents and the CLI reach Meko on `127.0.0.1:7778`. Browsers use port 7777,
  for the web UI and AgentK: from this computer with no login by default, or from your other
  devices over HTTPS with a password and optional two-step sign-in (`[web] access` in
  `config.toml`).
- **Usage stats.** Meko sends counts, versions and settings (never names or contents) to
  YugabyteDB once a day (every hour on an install's first day).
  Turn them off in the web UI's Settings, or with `[call_home] enabled = false` in `config.toml`
  ([what's sent](backend/docs/callhome.md)).

## Build from source

You need Go 1.26 (an older `go` downloads it on first use), a C compiler, and Node 22.13 or newer
for the memory engine and the web UI.

```sh
cd backend
make build memory_engine web_ui  # bin/meko (and bin/mekosh, a link to it), the memory engine and the web UI
./bin/meko start                 # runs in the background: 127.0.0.1:7778, and the web UI on :7777
./bin/meko status                # wait for "✓ Memory engine   ready"
./bin/meko ui                    # opens the web UI in your browser
```

The first start downloads the embedding model (23 MB). On RHEL 8, set
`CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc` before building.

## Docs and contributing

- [`AGENTS.md`](AGENTS.md): the repo's layout and its rules for commits and PRs.
- [`backend/AGENTS.md`](backend/AGENTS.md): building, testing and linting Meko.
- Meko's docs in [`backend/docs/`](backend/docs): [cli.md](backend/docs/cli.md) (every command),
  [design.md](backend/docs/design.md) (the server and storage),
  [memory-engine.md](backend/docs/memory-engine.md) (memories, conversations, search),
  [api.md](backend/docs/api.md) (MCP, REST and the CLI), [knowledge.md](backend/docs/knowledge.md)
  (knowledge files), [install.md](backend/docs/install.md) (releases, the installer, the service,
  extensions and updates) and [callhome.md](backend/docs/callhome.md) (usage stats).
- AgentK: [`extensions/agentk/CONTRIBUTING.md`](extensions/agentk/CONTRIBUTING.md).
- Questions and ideas: [Meko's Discord](https://mekodata.ai/discord/).

## License

[Apache-2.0](LICENSE)

The Meko logo in `backend/assets/` is Meko's mark, from Meko Cloud's web app.
