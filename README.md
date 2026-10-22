<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="backend/assets/meko-logo-white.svg">
    <img src="backend/assets/meko-logo.svg" width="220" alt="Meko">
  </picture>
</p>

# Meko

Meko is the memory your AI agents share: facts, conversations and files that last across
sessions, kept in datapacks that every agent working on the same thing can search and add to.
This repo holds `meko`, which runs Meko on your own machine, and the extensions built on it.
[Meko Cloud](https://mekodata.ai) is the hosted service.

`meko` is one binary, both the command line and the server. It serves Meko Cloud's MCP tools at
`/mcp` (memories, conversations, datapacks, artifacts, `context_search`) and the `/api/v1` REST
API Meko's web app uses, with Cloud's tool names, arguments and reply shapes, so an agent set up
for Cloud works against it. Everything lives in one SQLite file under `~/.meko`. Search mixes
vector similarity (sqlite-vec, with an embedding model that runs locally) and keywords (FTS5).
The server listens only on `127.0.0.1`.

Meko is under construction. It has its storage, memory engine, conversations, MCP and REST.
Knowledge files ([#170](https://github.com/yugabyte/agent-k-local/issues/170)), an installer and
releases ([#171](https://github.com/yugabyte/agent-k-local/issues/171)), and its web UI
([#172](https://github.com/yugabyte/agent-k-local/issues/172)) come next, so for now you build it
from source.

## Build and run

You need Go 1.26 (an older `go` downloads it on first use), a C compiler, and Node 22.13 or newer
for the memory engine.

```sh
cd backend
make build engine       # bin/meko, and the memory engine
./bin/meko start        # runs in the background on 127.0.0.1:7778
./bin/meko status       # wait for "Engine: ready"
```

The first start downloads the embedding model (23 MB). On RHEL 8, set
`CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc` before building.

## Commands

| Command | What it does |
|---|---|
| `meko start` / `stop` | Start the server in the background, and stop it |
| `meko status` | Whether it's running, its URL, version, data folder, engine and auth mode |
| `meko serve` | Run the server in the foreground |
| `meko version` | Meko's version, and Go's, SQLite's and sqlite-vec's |
| `meko token [--mcp]` | The token MCP and REST clients send; `--mcp` prints the `claude mcp add` command for Claude Code |
| `meko destroy [--force]` | Stop Meko and remove its data folder, everything in it, for good. It asks first; `--force` doesn't |
| `meko search "<question>"` | Search memories and past conversations (`context_search`) |
| `meko add_memory "<text>"` | Save a memory, word for word; `-` or no text reads stdin |
| `meko datapacks` | List the datapacks |
| `meko create_datapack <name>` | Create a datapack |
| `meko delete_datapack <name\|id> --yes` | Delete a datapack and everything in it, for good |

Every command takes `--data-dir`. The default is `~/.meko` (or `MEKO_HOME`), but a home folder on a
network filesystem (NFS, say) moves it to `$XDG_STATE_HOME/meko` or `/var/tmp/<user>/meko`, since
SQLite's locking doesn't work over the network; `meko status` prints where it is. `start` and
`serve` take `--port` (default 7778). `search` and `add_memory` take `--datapack NAME|ID`, and use
the default datapack without it. Meko makes the default datapack only on first start, so once
it's deleted, they need `--datapack`. The data commands take `--json` for the raw reply.

## Connect an agent

Register Meko with Claude Code by running the command `meko token --mcp` prints. As printed, it
registers Meko only for the folder you run it in (Claude Code's default `local` scope); add
`--scope user` to it to use Meko in every repo. Meko's tools then show up as `mcp__meko__*`. Any MCP client works the same way: Streamable HTTP at
`http://127.0.0.1:7778/mcp`, with `Authorization: Bearer <token>`.

The REST API takes the same token:

```sh
curl -s localhost:7778/api/v1/datapacks -H "Authorization: Bearer $(./bin/meko token)"
```

## Extensions

**[AgentK Local](extensions/agentk/README.md)** (`extensions/agentk`) is the agentic coder built
on Meko. It runs many Claude Code and Kiro CLI sessions from a browser or your phone, takes each
task from design to merged PR, and keeps each project's designs and learnings in a Meko datapack.
Its README covers installing and using it.

## Develop

- [`AGENTS.md`](AGENTS.md): the repo's layout and its rules for commits and PRs.
- [`backend/AGENTS.md`](backend/AGENTS.md): building, testing and linting Meko.
- Meko's design: [`backend/docs/design.md`](backend/docs/design.md) (the server and storage),
  [`backend/docs/memory-engine.md`](backend/docs/memory-engine.md) (memories, conversations,
  search) and [`backend/docs/api.md`](backend/docs/api.md) (MCP, REST and the CLI).
- AgentK Local: [`extensions/agentk/CONTRIBUTING.md`](extensions/agentk/CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)

The Meko logo in `backend/assets/` is Meko's mark, from Meko Cloud's web app.
