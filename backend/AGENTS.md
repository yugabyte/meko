# AGENTS.md: Meko backend

Meko's Go backend: one binary, `meko`, that is both the CLI and the server, the way
yugabyted runs YugabyteDB, and its Node memory engine. Its design is [docs/design.md](docs/design.md)
(the process model, data folder, store, HTTP server and supervisor), [docs/memory-engine.md](docs/memory-engine.md) (the memory engine and
conversations), [docs/api.md](docs/api.md) (MCP and REST), [docs/knowledge.md](docs/knowledge.md)
(knowledge files), [docs/install.md](docs/install.md) (release builds, releases, the installer,
the service, extensions, updates and secrets), [docs/web.md](docs/web.md) (the web listener: access with no login, turning it off, TLS, extensions at `/ext/<name>/`, the handoff from AgentK), [docs/web-ui.md](docs/web-ui.md) (the web UI),
[docs/cloud.md](docs/cloud.md) (the Meko Cloud link extensions use), [docs/callhome.md](docs/callhome.md) (usage stats) and [docs/cli.md](docs/cli.md) (every command and its flags). The rules for the whole repo are in the root [AGENTS.md](../AGENTS.md).

## Map

| Path | What it is |
|---|---|
| `cmd/meko/` | The entry point: hands the program's name and arguments to `internal/cli` (`cli.Main`), which runs `meko`, or `meko-cli` when run by that name. |
| `internal/cli/` | The commands, `meko`'s and `meko-cli`'s, with each one's help in one table (`help.go`) and the summary `install`, `start`, `stop`, `restart` and `status` print (`report.go`: ✓/!/✗ rows, a box of links, the next commands; the extensions print their own rows with `MEKO_SUMMARY=1`). `meko`'s: `start`, `stop` and `restart` (through the login service once installed, else a detached `serve`; `start` and `stop` also run the extensions'), `status`, `logs`, `serve` (which also checks for updates, installs one the Status page asks for, and rolls back a failing one, and runs the web listener after the extensions' handoff: `web.go`, and the Extensions page's install, update, start and stop, Check now, Update automatically, and the automatic updates after a check: `extactions.go`), `version`, `token` (with `--json`, also whether Meko Cloud is linked, never its key, for extensions), `ui` (opens the web listener's URL, at a path when given), `ui start` and `ui stop` (turn the web listener on and off, in `web.toml`, and have a running server follow), `cloud_link` (links or unlinks Meko Cloud, through the running server when there is one, and moves back from Meko Cloud the agents an older Meko connected there itself when there isn't: `cloud.go`), `destroy` (stops the server and removes the data folder, keeping the extensions' data in it unless told otherwise, `--with-extensions`), `install`, `uninstall`, `extension` and its subcommands, each a two-word row of `help.go` (`meko extension` alone lists them): `extension list` (every known extension's state, release and updates, without a server: `extlist.go`), `extension install` (AgentK or Presenter from its release's tarball, `--version`, and the extension's own `install` with what follows `--`: `installRelease`), `extension update` (`--all`, `--auto=on\|off`: `extupdate.go`, over `extensions.Update`), `extension delete` (its own uninstall, then its releases and link, and the registry entry; `--purge` its data too: `extdelete.go`), `agent` and its subcommands, two-word rows as `extension`'s (`meko agent` alone lists them): `agent list` (the agents installed and whether each reaches Meko, `--json` the route's data), `agent connect` (adds Meko's MCP server to one, or alone prints what any MCP client needs) and `agent disconnect`, in-process over `internal/agents` with the server stopped too (`agent.go`), `update` (Meko, then the new meko's `extension update --all`), `_launch` (an extension launcher's long-running commands, with probation: `launch.go`); `status` is a checklist, with `--verbose` and `--json`, and given one of `meko-cli`'s commands `meko` names it instead of running it. `meko-cli`'s, each alone or one a line at its prompt (`shell.go`: `use <datapack>`, the shared stdin, a pipe as a script): `export` (a datapack's zip export, through the REST route), and `search`, `add_memory`, `datapacks`, `create_datapack`, `delete_datapack`, `upload` and `upload_status` (MCP calls to the running server, so while Meko Cloud is linked they work on its datapacks and this Meko's; `datapacks` as one list of names and descriptions fitted to the terminal: `table.go`). The data folder's `flock` says whether a server runs.; `meko memory-extractor` (`extractor.go`) sets up the memory extractor through its REST routes, and `meko answers` (`answers.go`) Search's answers through theirs |
| `internal/config/` | Settings: flags, then `MEKO_*` (and yugabyted's `YB_DISABLE_CALLHOME`), then `config.toml`, then defaults (the API listener on 7778, the web listener on 7777, local access); Meko never writes `config.toml`: what it chooses for the web listener goes in `web.toml` (`AddWeb`; `SetWebEnabled` for `[web] enabled`, which `meko ui start` and `stop` set), read under it, which an older release ignores. |
| `internal/paths/` | The data folder's layout and permissions, and moving it off a network filesystem (the cases in `testdata/resolve-cases.json`, which AgentK's and Presenter's tests read too). |
| `internal/store/` | The only door to SQLite: one writer and a read pool, embedded append-only migrations, typed queries. |
| `internal/server/` | The HTTP servers: the API listener on 127.0.0.1 (the Host and Origin checks, the per-route credential check: admin token, client token, or none with `[server] auth = "none"`; the knowledge-file links' ticket; `/healthz` and `/api/v1`, whose status includes the memory engine's state, the auth mode, the usage stats' and the web listener's) and, with `Options.Web`, the web listener (`web.go`, with no login: local access, `local.go`, or remote access's site check, `SameSite`; `/api/v1/auth/status`; the web UI's route; `/ext/<name>/*`; the extensions' list and the Extensions page's actions, run as jobs kept across the listener's restarts: `extjobs.go`; the Status page's update check and install: `update.go`); on both, the Meko Cloud link's routes (`cloud.go`: the owner on the web listener, the admin token only on the API listener) and the agents' (`agents.go`: `GET /api/v1/agents` for any credential, `POST /api/v1/agents/:name/connect\|disconnect` for the owner or the admin token, else 403 `OWNER_ONLY`). |
| `internal/agents/` | The agents Meko connects to itself (`Catalog`: Claude Code, Codex, Kiro CLI, OpenCode, Cursor): finding each one's command on PATH, in its usual folders or, under a service, on the user's login shell's PATH (`Find`; `shell.go`, which also gives a command found there that PATH when it runs), reading from its own config whether its `meko` MCP server reaches Meko's target (`Manager.List`: connected, stale, elsewhere, none), adding it each agent's own way (`Connect`: `claude mcp`, `opencode mcp add`, Kiro's and Cursor's `mcp.json` and Codex's `config.toml` as files) and removing one that's Meko's own (`Disconnect`: JSON and JSONC edited as text, `jsonedit.go`), and moving back from Meko Cloud, at a link change, the agents an older Meko connected there with the link's key (`Move`, `cloudlink.Service.Moved`), all under one lock across processes (`run/agents.lock`). No token or key in a reply or error. |
| `internal/extproxy/` | An extension's web UI at `/ext/<name>/`: the reverse proxy to its unix socket (prefix stripped, Host and Origin kept, no Cookie or forwarding headers), its WebSockets kept so the listener's shutdown closes them (`CloseAll`), whether it answers (`Answers`: a connection, not only the socket file), telling it of a change (`Notify`: `POST /_meko/changed`), and the page when it isn't answering. |
| `internal/tlscert/` | The web listener's certificate with remote access: `[tls]`'s files, or a self-signed pair in `certs/`, made again near its end, and its fingerprint. |
| `internal/mcp/` | MCP at `/mcp`: Meko Cloud's tools on the official MCP Go SDK (stateless, JSON replies, raw arguments read loosely), recording each call, and each search for Context Gaps (not AgentK's own, which send `X-Meko-Client: agentk`, nor the web UI's); `Call` runs one in-process the same way, for REST's search and memories routes. While Meko Cloud is linked, Meko Cloud's own tools and instructions, each call routed by its datapack id: one this Meko has runs here, any other is sent there as it came and recorded as Meko Cloud's (a search for Context Gaps too, judged from its reply); `datapack_list` and `datapack_search`'s first page add this Meko's datapacks to Meko Cloud's; and `knowledgebase_upload_file` reads a file here into the knowledge base of either side (`cloud.go`). |
| `internal/rest/` | The `/api/v1` routes (Meko Cloud's paths and shapes, and a few of Meko's own) the web UI calls: the local user, datapacks and metrics, artifacts, memories and learnings, conversations and their threads, knowledge files, the upload and download links, and the usage stats setting (`/settings/usage-stats`); Meko's own search and memories routes on a datapack; and the linked Meko Cloud's datapacks on the same routes (`cloud.go`: `GET /datapacks` lists both sides as one list by name, each row with the `web_url` that opens it, a create goes to Meko Cloud while linked, and on the `:id` routes Meko Cloud has too an id this Meko doesn't have is passed on to Meko Cloud with the key, for any token, a browser's staying here (`byID`, `pass`, which refuses a path parameter that could name another path there); the sharing routes only Meko Cloud has are passed on while linked (`cloudOnly`, a UUID datapack only); no route takes or says a location but Context Gaps'; there's no `/api/v1/cloud`). Context Gaps' routes (`questions.go`: `/datapacks/:id/questions`, on this Meko's datapacks and the linked Meko Cloud's). |
| `internal/knowledge/` | Knowledge files before the database: the types Meko takes and the check of a file's first bytes, text extraction (PDF, HTML, Markdown, CSV, text), chunking, and the `knowledgebase-files/` folder their bytes live in. No queries, no memory engine. |
| `internal/webui/` | Finding the built web UI and serving it: its files, `index.html` for the app's own routes, and the headers that keep other sites from framing it. |
| `internal/wire/` | Meko Cloud's reply shapes, which MCP and REST both build their replies with. |
| `internal/testenv/` | A whole server for tests: store, store API, the memory engine with the hashing embedder, the service and the HTTP server. |
| `internal/envelope/` | The `{data, error}` reply every API uses, and the error handler that keeps internal errors generic. |
| `internal/storeapi/` | The store API the memory engine calls on `run/store.sock`: mem0's VectorStore and HistoryManager over `internal/store`, and the memory engine's hello. Internal token only. |
| `internal/memoryengine/` | Finding Node and the built memory engine, its child spec, and the Go client of its API on `run/mem_eng.sock`. |
| `internal/memory/` | The service MCP and REST call: memories (through the memory engine), learnings (keep and promote), artifacts, conversations, conversation search, knowledge files (upload and download links, the indexer, hybrid search with the reranker), the sweep that embeds turns and knowledge chunks, the call log (recording, pruning, and the stats metrics read), Context Gaps (`questions.go`: judging a search's reply, recording the poor ones as questions, their list, marks and pruning, [docs/api.md](docs/api.md#context-gaps)), and the local user's profile.; the memory extractor (`extractor.go`, `extractprompt.go`: turns sent to the user's own agent CLI on an interval, [docs/memory-extraction.md](docs/memory-extraction.md)); Search's answers (`answer.go`: a question and its results sent to the same CLI, sharing `runClaude` and `checkHarnessCommand`) |
| `assets/` | The Meko logo, dark and white, as Meko Cloud's web app has it; the repo's README shows it. `badges/`: the "meko context" badge, light and dark, that other projects' READMEs load from `main` by URL (the root README's Badges section), so the two files never move or get renamed. Each is the logo's first path (the letters "meko", without "By yugabyteDB"), 13.5 px tall, then "context" outlined to paths from Inter Medium 12 px, on one baseline at y = 19.75, 10 px padding and a 6 px gap; 112×30, corners 3.5 px. Light: `#F1F3F5` fill, `#DCE0E5` border, `#121017` wordmark, `#4B5563` text. Dark: `#1C1B20`, `#34323A`, white, `#A1A1AA`. Remake them this way if the logo changes. |
| `memory_engine/` | The memory engine: TypeScript on Node 22, public `mem0ai` with Meko's providers (store, history, a transformers.js embedder in a worker thread, a refusing LLM), the knowledge base's cross-encoder in a worker thread of its own, its API, its tests. |
| `web_ui/` | The web UI: React, Vite and Tailwind, built into `web_ui/dist`, which the web listener serves at `/`. Its pages call only `/api/v1`, with local or remote access and no login. Its tests (`web_ui/test`, Vitest in happy-dom) render pages against a faked `fetch`. |
| `internal/auth/` | The admin token the CLI uses (new at each start), the client token MCP and REST clients send (`client.token`, kept across restarts). |
| `internal/service/` | The login service: a launchd LaunchAgent or a systemd user unit from embedded templates, run with launchctl or systemctl. |
| `internal/secrets/` | The secret store: the login Keychain through `/usr/bin/security` (macOS), or 0600 files in the data folder. |
| `internal/cloudlink/` | The Meko Cloud link ([docs/cloud.md](docs/cloud.md)): its URLs in `cloud.toml` and its key in the secret store (`Store`), working out the web app and API URLs (`WebURL`), the checks before a change is saved (`CheckMCP`, `CheckAPI`, and in `Service.Link` the rule that a saved key goes only where it already goes or to Meko Cloud), telling the extensions (`Service.Changed`), listing the linked Meko Cloud's datapacks (`Service.Datapacks`, `ListDatapacks`: only the fields Meko shows are copied from Cloud's rows), and calling it with the key (`calls.go`: `Service.API`, its REST API, `Service.Pass`, a REST request passed on as it came, and `Service.Tool`, its MCP tools; failures are a `CallError` with the code and status the route answers), on one kept MCP session for `/mcp`'s calls and its tool list (`remote.go`: `Service.Forward`, `Service.CloudTools`); the link is read once and kept as changes save it. |
| `internal/extensions/` | `extensions.json`: the extensions `meko start` and `stop` run (with `MEKO_HOME` set) and their Update automatically, the ones `meko extension install` knows (agentk, presenter) and how each differs (`Catalog`: `RestartArgs`, `CheckArgs`, `Probation`, `CleanExits`, `BusyArgs`), finding their command (and running an npm-installed one with its Node's folder first on PATH), their folder in the data folder (`Dir`), a `Managed` one's release app folder (`Catalog.App`) and the release its command runs (`Catalog.Release`), their status (`Entry.Status`, 15 s, for an update; `Registry.Statuses`, every one at once with `StatusTimeout` each, for `meko status`), updating one (`Update`, the only update path: CLI, Extensions page and automatic updates) and reading their newest releases from a check (`Check`), their uninstall (`RemoveArgs` keeps their data, `UninstallArgs` deletes it), a damaged one's message (`Damaged`), and for one with a web UI its handoff (`_handoff --json`: its socket and old web settings). |
| `internal/update/` | Release installs (`~/.local/share/meko/app/<version>`, `app/current`, outside the data folder), GitHub releases, staging and checking an update, probation and rollback (for the extensions too, through `meko _launch`), the in-use mark that keeps a running version from being pruned (`App.LockInUse`), the update lock every update holds (`Lock`), and the web UI's check and install (`Updater.Status`, `CheckNow`, `StartApply`), whose checks also find the extensions' releases (`Updater.Extensions`, `AfterCheck`). `App.Name` names the program, so a `Managed` extension's releases (in `<data>/extensions/<name>/app`) use the same code. |
| `internal/supervisor/` | Child processes (the memory engine): process groups, health checks, backoff, stop, and the pid records (`run/children/`) that let `stop` kill a hung server's children. |
| `internal/callhome/` | Usage stats to YugabyteDB's call home service (`diagnostics.yugabyte.com`, from release installs): the report (counts only, from `store.UsageTotals`, `CallDays` and `QuestionDays`), yugabyted's envelope, the schedule, and the on/off state the web UI and `status` show. |
| `internal/spin/` | The summary's ✓/!/✗ rows (their marks, colors and format, which `cli/report.go` uses) and a step's rotating dots on a terminal while it runs (`Start`, `Wait`): the release listing and download in `meko update`, `extension update` and `extension install`. Off a terminal a step prints what it's doing as a plain line when it starts (`Installing AgentK <version> (<tag>)`), then its finished row. |
| `internal/logging/` | slog to a rotated JSON file and the terminal. |
| `internal/version/` | Version and commit, set with `-ldflags`. |
| `scripts/` | `install.sh` (Meko's installer, published with each release), `package-release.sh` and `smoke-release.sh` (the release build and its check), and their tests. |
| `docs/` | The design docs, the command reference (`cli.md`), and `docs_test.go`, which checks they carry no status lines, milestones or PR numbers. |
| `memory_engine/scripts/fetch-model.mjs` | Downloads the pinned models (the embedder and the reranker) for a release to bundle, and checks they run offline. |

## Commands

Run them in `backend/`:

```sh
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc   # RHEL 8 only; on macOS the Xcode command line tools are enough
make build                                          # bin/meko, and bin/meko-cli (a link to it)
make memory_engine                                  # npm ci (when the lockfile changes) and build memory_engine/dist
make web_ui                                         # the same for the web UI, into web_ui/dist
make test                                           # builds the memory engine, then go test -race, the memory engine's tests and the web UI's
make lint                                           # go vet, gofmt, golangci-lint v2 (.golangci.yml), tsc on the memory engine and the web UI
make bench                                          # the search budget at 200 MB (minutes; not in CI)
go test ./scripts/                                  # the installer, against a fake gh and a fake curl
```

It needs Go 1.26 (an older `go` downloads it on first use; `go.mod`'s `go` line is a hard minimum,
and a dependency bump can raise it, as golang.org/x/sys 0.49 did)
and a C compiler, because cgo is on; the Makefile sets it and the build tags. The memory engine needs
Node 22.13+ and npm. The first memory engine test run downloads the embedding and reranking models (23 MB
each) into
`memory_engine/.models` (git-ignored; `MEKO_TEST_MODELS` moves it). SQLite and sqlite-vec
are compiled into the binary from the `yugabyte/go-sqlite3` fork (`go.mod`'s `replace`), with the
`sqlite_fts5` and `sqlite_vec` tags the Makefile and `.golangci.yml` set; the first build compiles
SQLite and takes about a minute. CI runs build, lint
and test on Linux and macOS when a PR changes `backend/` (the `backend` job in
`.github/workflows/ci.yml`; `scripts/ci-scope.sh` decides), and the required `CI gate` fails when any of them fails.
Markdown only people read, such as this file and `REVIEW.md`, doesn't run it; `docs/` does, since
`docs/docs_test.go` checks it.

## Rules

- Commits and PR titles name the `meko` component (`fix(meko/cli): ...`). Meko releases on its
  own train (`.github/workflows/meko-release.yml`, tags `meko-v<version>`): a `feat`, `fix`,
  `perf` or `deps` commit naming `meko` on `main` cuts a Meko release, and one naming only `meko`
  never cuts an AgentK one. A change that also ships in AgentK names both (`feat(agentk,meko): ...`) and releases
  both. Use `chore` for what doesn't change what ships ([install.md](docs/install.md)).
- Keep the docs in `docs/` and this file current in the same commit as the code: `docs/cli.md`
  when a command or its flags change, and the repo root's `README.md` too when a command it lists,
  the install or the build requirements change. Check the change against [REVIEW.md](REVIEW.md).
- The docs describe Meko as it is now, with the reasons for its design: no status lines, no
  milestones, PR or issue numbers, delivery plans or history of how it got here (that's the git
  log's). Work not built yet is described as such, without a tracking number. Nothing in them
  describes Meko Cloud's internals (its private code, services, hosts, data stores or tooling):
  state only the behavior Meko matches. `docs/docs_test.go` catches the commonest slips (a
  `Status:` line, a milestone, a `#123`), not all history.
- Every database call goes through `internal/store`, the only package that imports the SQLite
  driver. The memory engine has no database: it stores through the store API. Migrations are append-only: never
  edit one that has shipped.
- Memory engine and web UI dependencies are pinned exactly in their `package.json`; `memory_engine/.npmrc` keeps npm from
  installing mem0ai's peers. Meko Cloud's code is private: copy none of it and cite none of its
  paths (state the behavior instead).
- Dependabot (the repo root's `.github/dependabot.yml`) bumps the Go modules (`deps(meko): ...`),
  the memory engine's npm packages (`deps(meko/memory-engine): ...`, or
  `chore(meko/memory-engine): ...` for dev-only ones) and the web UI's (`deps(meko/web-ui): ...`
  for its dependencies, which ship in `web_ui/dist`, or `chore(meko/web-ui): ...` for its build
  and test tools) every Monday, with minor and patch bumps grouped, `mem0ai` in a PR of its own,
  and the web UI's react family and its Vite and Tailwind build each in one group. `mattn/go-sqlite3` is
  left out: it's replaced by the fork, so upgrade the fork's tag by hand.
- Neither log ever holds memory, turn or knowledge-file text: only ids, counts and timings. The
  memory engine never logs an error's message.
- Knowledge files' bytes live in `knowledgebase-files/`, only through `internal/knowledge`'s
  `Files`; a file and its `kb_documents` row change together under the service's `fileMu`.
- Usage stats (`internal/callhome`) carry counts, enums and versions only: never a name, text,
  path, host or token. A new field says what it counts in [docs/callhome.md](docs/callhome.md).
- Tests use temporary data folders (short `/tmp` paths: unix sockets have a length limit) and free
  ports, never `~/.meko`, port 7778 or 7777 (the CLI's tests set `MEKO_WEB_PORT` to a free port). They never touch the user's own service, Keychain or
  extensions: the CLI's tests set `HOME`, `MEKO_SECRETS=file`, a fake service manager and a PATH
  without the machine's `agentk` (or `agent-k-local`, its old name), and never read or change the
  machine's agents: `agents.SystemFolders` and `agents.Applications` are empty, `agents.LoginShell` is `""` (or the user's
  interactive login shell runs and finds their agents), `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `KIRO_HOME`
  and `XDG_CONFIG_HOME` are unset, and the agent commands are fakes on the test's PATH. Tests that need no model start the memory engine with the
  hashing embedder (`memoryengine.Options.TestEmbedder`; the CLI's tests set it through their own
  `MEKO_TEST_EMBEDDER`, which only the test binary reads).
- No third-party C code is copied here. SQLite and sqlite-vec come from a tag of the
  `yugabyte/go-sqlite3` fork (its `README.yugabyte.md` says how to sync sqlite-vec from
  `yugabyte/sqlite-vec` or rebase on mattn). To upgrade, cut a new `-yb.<n>` tag there (never move
  one), bump the `replace` in `go.mod`, and update the versions `TestLibVersions` expects.
- Lint is `.golangci.yml`, with its choices listed at its top. Fix findings
  rather than adding to its exclusions; a `nolint` names the rule and says why.
- The lines the CLI and `scripts/install.sh` print end without a period, like Go's error strings,
  so a version, path or command at the end isn't read with one (`Meko is running 0.20261022.1`).
  Periods between sentences stay. `TestOutputLinesEndWithoutPeriod` checks every line of the
  strings `internal/cli` prints or formats with `fmt` and of the strings its functions return.
