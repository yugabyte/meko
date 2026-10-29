# Meko backend: the commands

Every `meko` and `meko-cli` command, with its flags. The README lists the few most people need; this
is the full list. How the CLI is built is in [design.md](design.md#process-model-and-cli) and
[api.md](api.md#cli), and installing and running Meko in [install.md](install.md).

Meko has two commands, one binary under two names, as PostgreSQL has `postgres` and `psql`: `meko`
runs Meko, and `meko-cli` works with what it keeps. `meko --help` and `meko-cli --help` list them, and
`<command> --help` gives each one's examples and flags. `meko` alone (or `meko --help`) opens with
what to type next, in bold on a terminal, for the state Meko is in: `meko install` on a computer
without it, `meko start` when it's set up but stopped, `meko ui` and `meko agent connect` when it's
running. `install`, `start`, `stop` and `status` print a
short summary: a row per step marked ✓, ! or ✗, the links for people (the web UI, on 7777) and for
agents (MCP and the REST API, on 7778), and what to run next.

| `meko` | What it does |
|---|---|
| `meko install [--no-service] [--access local\|remote]` | Set this meko up: the `meko` and `meko-cli` commands, the login service, and how browsers reach the web UI |
| `meko start` / `stop` / `restart` | Start the server (through its service once installed), and stop or restart it. `start` and `stop` also start and stop the extensions, each printing its own row; `stop --keep-extensions` doesn't |
| `meko status [--verbose] [--json]` | Whether it's running, a row each for the memory engine, service, updates, Meko Cloud link and extensions, and the links; `--verbose` adds everything (models, certificate, auth, usage stats, children), `--json` prints the server's status reply |
| `meko ui [path] [--no-open]` | Open the web UI in the browser (`meko ui /ext/agentk/` opens AgentK's); `--no-open` only prints its link |
| `meko logs [-f] [-n N]` | The server's and the memory engine's logs, merged by time; `-f` follows them; `--server`, `--memory-engine` or `--service` shows one, `--json` the raw lines |
| `meko memory-extractor use claude [--every 5m] [--model haiku]` | Extract memories from conversation turns with your own Claude Code, run headless with no tools, every 5 minutes; `off` turns it off, `status` shows how it's doing, `run` extracts now, and `datapack <name> [--profile general\|coding] [--instructions "..."] [--off\|--on]` sets a datapack's own; `--json` prints raw JSON. See [memory-extraction.md](memory-extraction.md) |
| `meko update [--check]` | Install Meko's newest release now (under the service it updates itself), then every extension's; `--check` only looks |
| `meko version` (or `--version`) | Meko's version, and Go's, SQLite's and sqlite-vec's |
| `meko agent` | Alone, list its commands: `list`, `connect` and `disconnect` (exits 2). |
| `meko agent list [--json]` | The agents installed on this computer (Claude Code, Codex, Kiro CLI, OpenCode: their command on PATH, in its usual folders or on your login shell's PATH), whether each one's `meko` MCP server reaches Meko (connected, needs reconnecting, elsewhere with its URL, or not connected, with the command that connects it), and where agents connect: this Meko, or the linked Meko Cloud. `--json` prints what `GET /api/v1/agents` answers. It needs no running server |
| `meko agent connect [claude\|codex\|kiro\|opencode]` | Add Meko's MCP server, named `meko`, to Claude Code (at user scope), Codex, Kiro CLI or OpenCode, for all your projects, replacing one it has: at this Meko with its token, or while Meko Cloud is linked at Meko Cloud with its key. Alone, it prints what any MCP client needs (Streamable HTTP, the URL, `Authorization: Bearer <token>`; while linked, Meko Cloud's URL and where its key is, never the key) and the agents installed. How it adds itself to each is in [api.md](api.md#agents) |
| `meko agent disconnect claude\|codex\|kiro\|opencode` | Remove the agent's `meko` MCP server when it's Meko's (this computer's, or the linked Meko Cloud's); one pointing elsewhere is left as it is |
| `meko token [--json]` | The token MCP and REST clients send; `--json` the MCP URL, the web UI's URL, the token and the Meko Cloud link |
| `meko cloud_link [--url U] [--web-url W] [--api-url A] [--off]` | Link Meko Cloud (or another Meko server) for the extensions, such as AgentK: reads the API key from stdin, or asks for it on a terminal, checks it and saves it; `--off` unlinks. The web UI's Settings › Meko Cloud does the same. Linking, a new key and unlinking move the agents connected to Meko to the new target, with a line saying which moved and which couldn't |
| `meko setup_link` | Print a one-time link that creates the web UI's login for remote access, or resets a lost one |
| `meko extension` | Alone, list its commands: `list`, `install`, `update` and `delete` (exits 2). |
| `meko extension list [--json]` | A row for each extension Meko knows, installed or not: its state (running, not running, didn't answer in 3s, damaged, not installed), the release Meko installed, and whether Meko installs its updates on its own and a newer one waits. It needs no running server. A damaged extension is registered, but its command is missing and can't be found again; `--json` prints the rows with what the Extensions page shows of their updates |
| `meko extension install agentk [--version V] [-- <options>]` | Install AgentK's newest release (or V) pointed at this Meko; `meko start` and `stop` then run it too, and the web UI's port serves it at `/ext/agentk/`. What follows `--` goes to AgentK's install: `--agent claude\|opencode\|kiro`, `--install-deps`, `--no-install-deps` |
| `meko extension install presenter` | The same for Presenter, at `/ext/presenter/`, as the command `meko_presenter`. An extension's command already on PATH (a build from source) is used as it is |
| `meko extension update <name>\|--all [--check] [--auto=on\|off]` | Install an extension's newest release now and restart it (AgentK's sessions keep running); one that fails to start is rolled back. Meko also installs updates on its own after its checks, unless `--auto=off` (or the Extensions page's Update automatically) turns that off for the extension |
| `meko extension delete <name> [--purge] [--force]` | Remove an extension: its own uninstall (`--non-interactive`) stops it and removes its services, ending AgentK's sessions; then the releases Meko installed and the `~/.local/bin` link Meko made go, and Meko forgets it. Its data (AgentK's home, sessions and worktrees) stays; `--purge` runs its uninstall with `--purge` and deletes its folder too. It asks first; `--force` doesn't. Meko leaves a command that isn't its release (a build from source), though the extension's own uninstall may remove its links: AgentK's removes `~/.local/bin/agentk` and `agent-k-local`. A damaged extension has no uninstall to run: a plain delete only removes its releases and forgets it, and `--purge` is refused, since a service it installed may still use its folder. Releases something still runs from (a session host whose stop failed) are kept, with its registration, until it stops |
| `meko destroy [--force] [--with-extensions]` | Stop Meko and remove its data folder for good. It asks first; `--force` doesn't. The extensions' data in it (AgentK's home) stays unless you say so when asked, or pass `--with-extensions`, which uninstalls them first |
| `meko uninstall [--purge [--force] [--with-extensions]]` | Remove the service, the installed releases and the commands, keeping the data; `--purge` also destroys the data as `meko destroy` does, asking first |
| `meko serve` | Run the server in the foreground (what the service runs; not listed in `--help`) |

| `meko-cli` | What it does |
|---|---|
| `meko-cli` | A prompt (`meko-cli default>`) that takes the commands below, one a line, plus `use <datapack>` (later `search`, `add_memory`, `upload` and `upload_status` lines use it unless they name another; a `delete_datapack` of it goes back to the default), `help` and `exit`. There's no history or line editing yet, and Ctrl-C leaves. Given a file or pipe instead of a terminal, it runs the lines as a script and exits with the first failing line's code |
| `meko-cli datapacks [--local]` | List the datapacks and the linked Meko Cloud's by name, with their descriptions; `--local` leaves Meko Cloud out, and `--json` adds each one's id and where it lives |
| `meko-cli create_datapack <name> [--cloud]` | Create a datapack, here or with `--cloud` in the linked Meko Cloud |
| `meko-cli delete_datapack <name\|id> [--force]` | Delete a datapack and everything in it, for good. It asks first; `--force` doesn't |
| `meko-cli export [<name\|id>] [-o FILE] [--force]` | Write a datapack (default: the default one) to one zip: memories, collective memories, conversations, knowledge files and artifacts with their embeddings and file contents, and the call log. It won't replace a file unless `--force` |
| `meko-cli search "<question>" [--source S] [--limit N]` | Search memories, the knowledge base and past conversations (`context_search`); `--source memory`, `knowledge_base` or `conversation` searches one (default `auto`, all three), and `--limit` caps the results per source (default 8) |
| `meko-cli add_memory "<text>"` | Save a memory, word for word; `-` or no text reads stdin (not at the prompt) |
| `meko-cli upload <file>... [--no-wait]` | Add PDF, text, Markdown, JSON, CSV, XML or HTML files (up to 50 MiB) to the knowledge base, and wait until they're indexed |
| `meko-cli upload_status <key>` | How an uploaded file is doing |

`meko` runs none of `meko-cli`'s commands: given one (`meko search …`), it names the `meko-cli` command to
run instead. Run as `mekosh`, the binary runs nothing and names the `meko-cli` command (exit 2). Every command takes `--data-dir`. The default is `~/.meko` (or `MEKO_HOME`), but a home folder on a
network filesystem (NFS, say) moves it to `$XDG_STATE_HOME/meko` or `/var/tmp/<user>/meko`, since
SQLite's locking doesn't work over the network; `meko status` prints where it is. `start`,
`restart` and `serve` take `--port` (the agents' listener's, default 7778) and `--log-level` (`debug`, `info`,
`warn` or `error`; default `info`); the web UI's port and access are
`[web] port` (default 7777) and `access` in `config.toml`. `search`, `add_memory`, `upload` and
`upload_status` take `--datapack NAME|ID`, and use
the default datapack without it. Meko makes the default datapack only on first start, so once
it's deleted, they need `--datapack`. While Meko Cloud is linked (`meko cloud_link`), they work on
its datapacks too: a name or id this Meko doesn't have is looked up there, and `cloud:` or `local:`
before it picks the side (`--datapack cloud:team-notes`; the same for `delete_datapack` and the
prompt's `use`). `export` works only on this Meko's. `--json` prints the raw reply: `meko-cli`'s commands other than
`export`, and `meko status`, `token`, `logs`, `version` and `memory-extractor`.
