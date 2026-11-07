# Meko backend: the commands

Every `meko` and `meko-cli` command, with its flags. The README lists the few most people need; this
is the full list. How the CLI is built is in [design.md](design.md#process-model-and-cli) and
[api.md](api.md#cli), and installing and running Meko in [install.md](install.md).

Meko has two commands, one binary under two names, as PostgreSQL has `postgres` and `psql`: `meko`
runs Meko, and `meko-cli` works with what it keeps. `meko --help` and `meko-cli --help` list them, and
`<command> --help` gives each one's examples and flags. `meko` alone (or `meko --help`) ends with
what to type next, after the commands, in bold on a terminal, for the state Meko is in: `meko install`
on a computer without it, `meko start` when it's set up but stopped, and when it's running a `Next:`
list of `meko ui` and the steps not set up yet. `install`, `start`, `stop` and `status` print a
short summary: a row per step marked ✓, ! or ✗, a `● Meko <version> is running` headline, plain
link rows for people (`Web UI`) and for agents (`Agents`: MCP and the REST API), both on Meko's one port (7777), and
the Meko Cloud it's linked to (`Cloud`: its web app, or "not linked"), and what to run next, the
first command as the call to action. A first install from the installer ends, before that, by
offering the extensions to add. What to run next (`meko install`, `meko start`, `meko --help`, `meko status`, `meko cloud-link`
and `meko agent connect <agent>`) offers only what isn't set up yet,
and leaves out a step whose state it can't read. In order: `meko ui` (`meko ui start` while the web
UI is off), using Meko in an agent (`meko agent connect claude`, or the first agent installed when
Claude Code isn't), learning from your conversations (`meko memory-extractor use claude`, when
Claude Code is installed), sharing through Meko Cloud (`meko cloud-link`), and AgentK (`meko
extension install agentk`, or `meko kode ui` once it's there, `meko kode --data-dir <dir> ui` for a
data folder other than the default one; an extension the install's picker just added comes first),
and `install` and `start` end with `meko status` (`install`'s naming `meko uninstall` too). `meko
agent connect <agent>` opens with starting a
new session of that agent (after `meko start` while Meko isn't running), in place of `meko ui` and
the connect step.

| `meko` | What it does |
|---|---|
| `meko install [--no-service] [--access local\|remote] [--no-ui]` | Set this meko up: the `meko` and `meko-cli` commands (offering, at a terminal, to put `~/.local/bin` on the PATH in the shell's profile), the login service (once Meko's port is known to be free: another program's moves Meko to the next free port, set in `config.toml`, unless someone chose it), and how browsers reach the web UI (local: this computer only; remote: every address, over HTTPS; no login either way). `--no-ui` leaves the web UI off until `meko ui start`. A failure is a `✗` row with what to do; once something was written, running the installer again finishes the setup ([install.md](install.md#install)) |
| `meko start` / `stop` / `restart` | Start the server (through its service once installed), and stop or restart it. `start` and `stop` also start and stop the extensions, each printing its own row; `stop --keep-extensions` doesn't |
| `meko status [--verbose] [--json]` | A row each for the context engine, service, updates and extensions (and the Meko Cloud link when it can't be read), with a Storage row from 90% of the storage limit, then a `● Meko <version> is running · pid …, up …` headline, the link rows (`Web UI`, `Agents`, `Cloud`), and what to run next; `--verbose` adds everything (models, certificate, auth, storage used by kind, children), `--json` prints the server's status reply, usage stats included |
| `meko ui` | Open Meko's web UI in the browser, and print its link; with no browser here (over SSH, or on Linux without `DISPLAY` or `WAYLAND_DISPLAY`) it only prints the link. The extensions' pages are in its sidebar. While the web UI is off it says to run `meko ui start` |
| `meko ui start` / `stop` | Turn the web UI on or off, and keep it so across restarts (`[web] enabled` in `web.toml`). Off closes its port, and AgentK's page with it. A running Meko starts or stops it at once (`Started the web UI at <url>`, `Stopped the web UI`); a stopped one reads it at its start. When `config.toml` sets `[web] enabled`, they refuse and say to change it there |
| `meko logs [-f] [-n N]` | The server's and the context engine's logs, merged by time; `-f` follows them; `--server`, `--context-engine` or `--service` shows one, `--json` the raw lines |
| `meko answers use claude [--model haiku]` | Have your own Claude Code, run headless with no tools, write an answer from each of the web UI's Search results; `off` turns it off, `status` shows it (`--json` for raw JSON). It makes one test call first. See [memory-extraction.md](memory-extraction.md#search-answers) |
| `meko memory-extractor use claude [--every 5m] [--model haiku]` | Extract memories from conversation turns with your own Claude Code, run headless with no tools, every 5 minutes (said in words: `every 5 minutes`); `off` turns it off, `status` shows how it's doing, `run` extracts now, and `datapack <name> [--profile general\|coding] [--instructions "..."] [--off\|--on]` sets a datapack's own; `--json` prints raw JSON. See [memory-extraction.md](memory-extraction.md) |
| `meko update [--check]` | Install Meko's newest release now (under the service it updates itself), then every extension's; `--check` only looks |
| `meko version` (or `--version`) | Meko's version, and Go's, SQLite's and sqlite-vec's |
| `meko agent` | Alone, list its commands: `list`, `connect` and `disconnect` (exits 2). |
| `meko agent list [--json]` | The agents installed on this computer (Claude Code, Codex, Kiro CLI, OpenCode, Cursor: their command on PATH or in its usual folders; Cursor's editor command also inside the macOS app, or its `cursor-agent` CLI), whether each one's `meko` MCP server reaches Meko (connected, needs reconnecting, elsewhere with its URL, or not connected, with the command that connects it), and where agents connect: this Meko, which sends their calls to Meko Cloud while it's linked. `--json` prints what `GET /api/v1/agents` answers. It needs no running server |
| `meko agent connect [claude\|codex\|kiro\|opencode\|cursor]` | Add Meko's MCP server, named `meko`, to Claude Code (at user scope), Codex, Kiro CLI, OpenCode or Cursor, for all your projects, replacing one it has: at this Meko with its token, linked to Meko Cloud or not. Alone, it prints what any MCP client needs (Streamable HTTP, this Meko's URL, `Authorization: Bearer <token>`) and the agents installed. How it adds itself to each is in [api.md](api.md#agents) |
| `meko agent disconnect claude\|codex\|kiro\|opencode\|cursor` | Remove the agent's `meko` MCP server when it's this computer's; one pointing elsewhere (Meko Cloud included) is left as it is |
| `meko token [--json]` | The token MCP and REST clients send; `--json` the MCP URL, the web UI's URL, the token, and whether Meko Cloud is linked (never its key), with `datapack_routing` for older extensions ([cloud.md](cloud.md#what-extensions-see)) |
| `meko cloud-link [--url U] [--web-url W] [--api-url A] [--no-browser] [--off]` | Link Meko Cloud (or another Meko server). On a terminal it asks how, picked with ↑ and ↓ and Enter, or 1 or 2 at once (an Enter typed just after the number is dropped, so the key prompt doesn't take it; Ctrl+C cancels; a terminal whose settings can't be changed to read a key at a time is asked for a line, `1` or `2`, instead): sign in with the browser (the default; it opens the browser, or with `--no-browser` prints the address, and takes the address the browser ended on, pasted, when the browser is on another computer), or enter an API key. Piped, it reads the key from stdin. It checks the key and saves it, then says `✓ Linked to <url>` and what to run next; `--off` unlinks. While linked, Meko's `/mcp`, the extensions and `meko-cli` work on Meko Cloud's datapacks, and on the ones already on this Meko until they're moved; agents stay connected to this Meko. The web UI's Settings › Meko Cloud does the same. A change moves back the agents an older Meko connected to Meko Cloud, with a line saying which moved and which couldn't |
| `meko extension` | Alone, list its commands: `list`, `install`, `update` and `delete` (exits 2). |
| `meko extension list [--json]` | A row for each extension Meko knows, installed or not: its state (running, not running, didn't answer in 3s, damaged, not installed), the release Meko installed, and whether Meko installs its updates on its own and a newer one waits. It needs no running server. A damaged extension is registered, but its command is missing and can't be found again; `--json` prints the rows with what the Extensions page shows of their updates |
| `meko extension install agentk [--version V] [-- <options>]` | Install AgentK's newest release (or V) pointed at this Meko; `meko kode` then runs its commands, `meko start` and `stop` run it too, and the web UI's port serves it at `/ext/agentk/`. What follows `--` goes to AgentK's install: `--agent claude\|opencode\|kiro`, `--install-deps`, `--no-install-deps` |
| `meko extension install present` | The same for Presenter, at `/ext/present/`, run with `meko present`. An extension's program already on PATH (a build from source, `npm link`) is used as it is. A release install puts no command on PATH: Meko runs its `app/current/bin/<program>` (`me_kode`, `me_present`), and removes the `~/.local/bin` links an older Meko made into this data folder's releases, by any of the program's names. An AgentK installed under an old name (`me_koder`, `agentk`, `agent-k-local`) moves to `me_kode` when Meko starts |
| `meko extension update <name>\|--all [--check] [--auto=on\|off]` | Install an extension's newest release now and restart it (AgentK's sessions keep running); one that fails to start is rolled back. Meko also installs updates on its own after its checks, unless `--auto=off` (or the Extensions page's Update automatically) turns that off for the extension |
| `meko extension delete <name> [--purge] [--force]` | Remove an extension: its own uninstall (`--non-interactive`) stops it and removes its services, ending AgentK's sessions; then the releases Meko installed and any `~/.local/bin` links an older Meko made into them go, and Meko forgets it. Its data (AgentK's home, sessions and worktrees) stays; `--purge` runs its uninstall with `--purge` and deletes its folder too. It asks first; `--force` doesn't. Meko leaves a program that isn't its release (a build from source), though the extension's own uninstall may remove its old links: AgentK's removes `~/.local/bin/me_kode`, `me_koder`, `agentk` and `agent-k-local`. A damaged extension has no uninstall to run: a plain delete only removes its releases and forgets it, and `--purge` is refused, since a service it installed may still use its folder. Releases something still runs from (a session host whose stop failed) are kept, with its registration, until it stops |
| `meko kode <command> [args]` | Run one of AgentK's commands (`meko kode ui`, `status`, `attach <session>`, `logs -f`, `--help`): Meko runs AgentK's registered program, `me_kode` (its release's `app/current/bin/me_kode`, or a build from source on PATH), with what follows, `MEKO_HOME` naming the data folder, on this terminal, and exits as it does. A leading `--data-dir <dir>` is Meko's, for another data folder; everything else, `--help` included, is AgentK's. Not installed, it says to run `meko extension install agentk`; a program gone and not found again, that AgentK is damaged |
| `meko present <command> [args]` | The same for Presenter's commands (`meko present status`, `--help`), its program `me_present` |
| `meko destroy [--force] [--with-extensions]` | Stop Meko and remove its data folder for good. It asks first; `--force` doesn't. The extensions' data in it (AgentK's home) stays unless you say so when asked, or pass `--with-extensions`, which uninstalls them first |
| `meko uninstall [--purge [--force]]` | Remove the extensions (as `meko extension delete` does, without asking: nothing would run them), then the service, the installed releases, the commands and the PATH line `meko install` added, keeping the data, the extensions' too; its last line says how to install Meko again. `--purge` also destroys the data as `meko destroy` does, the extensions' data with it, asking first, once (`--force` doesn't), and the agents connected to this Meko are disconnected, since its token goes with the data |
| `meko serve` | Run the server in the foreground (what the service runs; not listed in `--help`) |

| `meko-cli` | What it does |
|---|---|
| `meko-cli` | List its commands. It has no prompt: it runs the one command it's given, so a script runs it once per command, with `--datapack` on each |
| `meko-cli datapacks` | List the datapacks by name, with their descriptions (while Meko Cloud is linked, its datapacks and this computer's); `--json` adds each one's id |
| `meko-cli create_datapack <name>` | Create a datapack (in the linked Meko Cloud while it's linked) |
| `meko-cli delete_datapack <name\|id> [--force]` | Delete a datapack and everything in it, for good. It asks first; `--force` doesn't |
| `meko-cli export [<name\|id>] [-o FILE] [--force]` | Write one of this computer's datapacks (default: the default one; name one while Meko Cloud is linked) to one zip: memories, collective memories, conversations, knowledge files and artifacts with their embeddings and file contents, and the call log. It won't replace a file unless `--force` |
| `meko-cli search "<question>" [--source S] [--limit N]` | Search memories, the knowledge base and past conversations (`context_search`); `--source memory`, `knowledge_base` or `conversation` searches one (default `auto`, all three), and `--limit` caps the results per source (default 8) |
| `meko-cli add_memory "<text>"` | Save a memory, word for word; `-` or no text reads stdin |
| `meko-cli upload <file>... [--no-wait]` | Add PDF, text, Markdown, JSON, CSV, XML or HTML files (up to 50 MiB) to the knowledge base, and wait until they're indexed |
| `meko-cli upload_status <key>` | How an uploaded file is doing |

`meko` runs none of `meko-cli`'s commands: given one (`meko search …`), it names the `meko-cli` command to
run instead. Run as `mekosh`, the binary runs nothing and names the `meko-cli` command (exit 2). Every command takes `--data-dir`. The default is `~/.meko` (or `MEKO_HOME`), but a home folder on a
network filesystem (NFS, say) moves it to `$XDG_STATE_HOME/meko` or `/var/tmp/<user>/meko`, since
SQLite's locking doesn't work over the network; `meko status --verbose` prints where it is (`Data:`). `start`,
`restart` and `serve` take `--port` (Meko's one port, for agents, the CLI and the web UI; default 7777)
and `--log-level` (`debug`, `info`, `warn` or `error`; default `info`); how browsers reach the web
UI is `[web] access` in `config.toml`. `search`, `add_memory`, `upload` and
`upload_status` take `--datapack NAME|ID`, and use
the default datapack without it. Meko makes the default datapack only on first start, so once
it's deleted, they need `--datapack`. While Meko Cloud is linked (`meko cloud-link`), they work on
its datapacks and on the ones already on this computer, as agents do: a name is looked up among
both, a datapack this computer has is used here, and the default is Meko Cloud's. `export` works
only on this computer's datapacks, linked or not, and never asks Meko Cloud, so it works while
Meko Cloud is down. `--json` prints the raw reply: `meko-cli`'s commands other than
`export`, and `meko status`, `token`, `logs`, `version`, `memory-extractor` and `answers`.
