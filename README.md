<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="src/meko/assets/meko-logo-white.svg">
    <img src="src/meko/assets/meko-logo.svg" width="220" alt="Meko">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/yugabyte/meko/releases/latest"><img src="https://img.shields.io/github/v/release/yugabyte/meko?filter=meko-v*&label=Release" alt="Meko release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS-lightgrey" alt="Platform: Linux | macOS">
  <a href="https://docs.mekodata.ai/"><img src="https://img.shields.io/badge/docs-orange" alt="Docs"></a>
  <a href="https://mekodata.ai"><img src="https://img.shields.io/badge/Meko%20Cloud-purple" alt="Meko Cloud"></a>
  <a href="https://mekodata.ai/discord/"><img src="https://img.shields.io/badge/Discord-5865F2?logo=discord&logoColor=white" alt="Discord"></a>
</p>

**The Context Engine for Agentic Applications**

Universal, self-learning context for every agent, tool, and team: collective memory, shared
knowledge, and decision traces in governed datapacks. Run Meko locally on your machine with `meko`,
a single binary that has the command line, the server, the MCP tools and the REST API.

## Start here

| I want to… | Go to |
|---|---|
| Run Meko on my machine | [Install](#install) |
| Give agents like Claude, Codex, or any MCP client, Meko's context | [Connect an agent](#connect-an-agent) |
| Link to Meko Cloud | [Link to Meko Cloud](#link-to-meko-cloud) |
| See every command | [CLI](src/meko/docs/cli.md) ⋅ [API](src/meko/docs/api.md#rest-at-apiv1) ⋅ [MCP](src/meko/docs/api.md#mcp-at-mcp)|
| Run many coding sessions that share and learn from each other | [Kode](src/extensions/agentk/README.md) |
| Contribute to Meko | [Contributing](#docs-and-contributing) |

## Install

On any Apple Silicon Mac (M1, M2, M3, M4 or later) with macOS 14 or newer, or Linux on x64 or arm64.
Windows and older Intel-based Macs aren't supported.

```sh
curl -fsSL https://mekodata.ai/install.sh | sh
```

Add a memory and search:
```sh
meko-cli add_memory "Elementary, my dear Watson"
meko-cli add_memory "The quick brown fox jumps over the lazy dog"
meko-cli search "What color was the fox?"
```

Open web ui:
```sh
meko ui
```

## Connect an agent

Give agents like Claude, Codex, or any MCP client, Meko's context. `meko agent connect` adds Meko
to your agent, for all your projects:

```sh
meko agent list                # the agents on this computer, and whether each reaches Meko
meko agent connect claude      # or codex, kiro, opencode, cursor
meko agent connect             # the MCP URL and token, for any other MCP client
```

The REST API takes the same token:

```sh
curl -s http://127.0.0.1:7777/api/v1/datapacks -H "Authorization: Bearer $(meko token)"
```

## Link to Meko Cloud

Link Meko to [Meko Cloud](https://cloud.mekodata.ai/) by signing in with your browser, or with an
API key you make there:

```sh
meko cloud-link          # sign in with your browser, or enter a key; reads a piped key; --off unlinks
```

Or use the web UI's Settings › Meko Cloud.
[127.0.0.1:7777/settings/meko-cloud](http://127.0.0.1:7777/settings/meko-cloud)

Once linked, Meko works on Meko Cloud's datapacks and keeps serving the ones already on your
computer until they're moved: the agents stay connected to Meko on your computer, which sends each
call to the side that has its datapack, `meko-cli` works the same way, and new datapacks are made in
Meko Cloud.

## Commands

`meko` runs Meko, and `meko-cli` works with your context. The ones you'll use first:

| Command | What it does |
|---|---|
| `meko start` / `stop` / `status` | Start or stop the server, or see whether it's running and its links |
| `meko ui` | Open the web UI in your browser. It has no login; `meko ui stop` turns it off, and `meko ui start` on |
| `meko agent connect claude` | Add Meko to Claude Code (or `codex`, `kiro`, `opencode`), for all your projects |
| `meko memory-extractor use claude` | Extract memories from your conversations with your own Claude Code, every 5 minutes. It's off until you run this or turn it on in the web UI's Settings; `status`, `run` and `off` check, run or stop it. See [memory extraction](src/meko/docs/memory-extraction.md) |
| `meko answers use claude` | Have your own Claude Code write an answer from each search's results in the web UI, citing them. Off until you run this or turn it on in Settings; `status` and `off` check or stop it |
| `meko update` | Install Meko's newest release now, then every extension's |
| `meko-cli search "<question>"` | Search memories, the knowledge base and past conversations |
| `meko-cli add_memory "<text>"` | Save a memory, word for word |
| `meko-cli upload <file>...` | Add files to the knowledge base |

`meko --help`, `meko-cli --help` and `<command> --help` give the rest. Every command and flag is in
[`src/meko/docs/cli.md`](src/meko/docs/cli.md).

## Extensions

**[Kode](src/extensions/agentk/README.md)** is the agentic coder built on Meko. It runs many Claude
Code, OpenCode and Kiro CLI sessions from a browser or your phone, takes each task from design to
merged PR, and keeps each project's designs and learnings in a Meko datapack.

Install it with:
```sh
meko extension install kode       # -- --agent claude picks its coding agent
meko kode ui                      # opens Kode in your browser
```

## How it works

- **One folder.** Everything lives under `~/.meko`.
- **Datapacks.** Separate workspaces, each with its own memories, knowledge, conversations and
  artifacts.
- **Memory and knowledge.** Facts agents save, and files you or your agents upload (PDF, text,
  Markdown, JSON, CSV, XML, HTML).
- **Conversations, decision traces and artifacts.** Agents' turns with their reasoning and plan,
  what each turn looked up and saved, and files agents store and fetch back whole.
- **Under the hood.** Retrieval for RAG over hybrid search (meaning and keywords, with knowledge
  results reranked by a cross-encoder), memory extraction from conversations, and automatic text
  extraction, chunking and embedding of uploaded files. Your local datapacks embedding and reranking
  run locally.

## Usage stats

Meko sends anonymous usage stats to YugabyteDB, so we can see how it's used and fix what gets in
people's way. They never include your data: no datapack, memory, conversation or file, and no query,
path or token.

To turn them off, use Settings › Anonymous usage stats in Meko's web UI, or set `enabled = false`
under `[call_home]` in `~/.meko/config.toml`. Kode, if you use it, has its own checkbox in its
Settings › General.

## Build from source

You need Go 1.26 (an older `go` downloads it on first use), a C compiler, and Node 22.13 or newer
for the context engine and the web UI.

```sh
cd src/meko
make build context_engine web_ui  # bin/meko (and bin/meko-cli, a link to it), the context engine and the web UI
./bin/meko start                  # runs in the background on 127.0.0.1:7777: MCP, the API and the web UI
```

The first start downloads the embedding model (23 MB), and `meko-cli` calls fail with
`CONTEXT_ENGINE_NOT_READY` until `meko status` shows the context engine ready. On RHEL 8, set
`CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc` before building.

## Docs and contributing

This repo is agent-first. To work on it, point your coding agent (Claude Code, Codex, OpenCode,
Kiro, Cursor or another) at the repo and say what you want: it starts from
[`AGENTS.md`](AGENTS.md), which maps the repo, sets its rules for commits and PRs, and leads to each
part's own guide and docs.

PRs need the Contributor License Agreement signed: CLA Assistant's `license/cla` check on your
first PR links to a page where you sign it once with your GitHub account.

- Meko's docs: [`src/meko/docs/`](src/meko/docs)
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md)
- Kode docs: [`src/extensions/agentk/docs`](src/extensions/agentk/docs)
- Questions and ideas: [Meko's Discord](https://mekodata.ai/discord/) or
  [Slack](https://inviter.co/yugabytedb).

## Badges

Is your context stored in Meko? Show it in your README.

<a href="https://mekodata.ai"><img src="assets/badges/meko-context.svg" width="112" height="30" alt="Meko context"></a>
<a href="https://mekodata.ai"><img src="assets/badges/meko-context-dark.svg" width="112" height="30" alt="Meko context"></a>

The light badge:

```md
[![Meko context](https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context.svg)](https://mekodata.ai)
```

```html
<a href="https://mekodata.ai">
  <img width="112" height="30" src="https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context.svg" alt="Meko context">
</a>
```

The dark badge:

```md
[![Meko context](https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context-dark.svg)](https://mekodata.ai)
```

```html
<a href="https://mekodata.ai">
  <img width="112" height="30" src="https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context-dark.svg" alt="Meko context">
</a>
```

Or one that follows the reader's light or dark theme:

```html
<a href="https://mekodata.ai">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context-dark.svg">
    <img width="112" height="30" src="https://raw.githubusercontent.com/yugabyte/meko/main/assets/badges/meko-context.svg" alt="Meko context">
  </picture>
</a>
```

The snippets load the badges from this repo's `main` branch on raw.githubusercontent.com.

## License

[Apache-2.0](LICENSE)

The Meko logo in `src/meko/assets/` is Meko's mark, from Meko Cloud's web app, and the badges in
`assets/badges/` are made from it.

<!-- Scarf pixel meko-github-readme: counts README views. -->
<img referrerpolicy="no-referrer-when-downgrade" src="https://static.scarf.sh/a.png?x-pxid=437e0be7-bfef-40e8-83ca-dd7d4a7ea7da" alt="" width="1" height="1" />
