# REVIEW.md: Meko backend

What to check when reviewing a change to `backend/`, whether you're a person or an agent. Order
findings by severity: **blocker** (breaks a rule below, loses data, or opens a security hole),
**major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

- [ ] `make test` and `make lint` pass in `backend/`; new behavior has a test.
- [ ] Tests never reach this machine's own extensions (an installed `agentk`): PATH stays
      `/usr/bin:/bin` (`noRealExtensions`) while anything can run one, and cleanups that run Meko
      commands leave the extensions alone (`setup`'s `stop --keep-extensions`), since cleanups run
      last first and `t.Setenv` has restored PATH by then. A real `agentk stop` ends every session.
- [ ] Every database call goes through `internal/store`, the only package that imports the SQLite
      driver.
      Migrations are append-only (`internal/store/migrations/`), like AgentK's.
- [ ] Every route but `/healthz`, the knowledge-file links, the web UI's files (`/*`) and the UI
      code's redeem (`/api/v1/ui-code/redeem`) needs a credential, checked on the matched route
      (`c.Path()`), never the raw URL; catch-all `/api/*` and `/mcp/*` routes keep unknown API
      paths off the UI's ([docs/web-ui.md](docs/web-ui.md)). The links (`/api/v1/knowledge-uploads/:ticket`,
      `/api/v1/knowledge-downloads/:ticket`) take a one-time ticket instead: 256 random bits, 5 minutes,
      an upload's bound to its datapack, key, size and SHA-256 ([docs/knowledge.md](docs/knowledge.md)).
      Otherwise it's the admin token on `/api/v1/status`, `/api/v1/version` and `/api/v1/ui-code`, the admin or client
      token everywhere else, and nothing there only with `[server] auth = "none"`
      ([docs/api.md](docs/api.md)). Tokens and call-log rows never hold memory or turn text. The store API and the memory engine's API listen only
      on their unix sockets (0600) and need the internal token on every route, the memory engine's
      `/healthz` too (its reply can say why the memory engine failed). The server binds 127.0.0.1 only, checks `Host`,
      and refuses requests whose `Origin` isn't its own (`http://127.0.0.1:<port>` or
      `http://localhost:<port>`), so only the web UI it serves can call it from a browser.
      `knowledgebase_upload_file` reads a path as the server's user, so it refuses a call that came
      in with no token.
- [ ] Files in the data folder are owner-only (folders 0700, files 0600), and tokens and memory text
      never reach logs or error replies (internal errors get the generic message), in Go or in the
      memory engine (`logs/memory_engine.log`).
- [ ] REST routes keep Meko Cloud's paths and `{data, error}` shapes ([docs/api.md](docs/api.md)): a
      `datapack_id` that isn't a UUID is 400 `VALIDATION_ERROR`, one that doesn't exist is 404
      with code `NOT_FOUND`.
- [ ] MCP tools keep Meko Cloud's names, arguments and reply shapes ([docs/api.md](docs/api.md)),
      built in `internal/wire` when REST shares them; errors a caller can act on are replies
      (`{"error", "detail"}`, `isError: false`); every call is recorded in the call log without its
      arguments or text. A search takes a search slot; nothing holds the writer while it waits on
      the memory engine. A change to what AgentK's `meko.ts` sends or reads updates the replay in
      `internal/mcp/agentk_contract_test.go`.
- [ ] The memory engine stores only through the store API, its dependencies stay pinned, and nothing copies
      or cites Meko Cloud's private code.
- [ ] No third-party C code is copied into `backend/`. SQLite and sqlite-vec come from the
      `replace` of `yugabyte/go-sqlite3` in `go.mod`: a bump names a new `-yb.<n>` tag and updates
      `TestLibVersions`, and the build keeps the `sqlite_fts5` and `sqlite_vec` tags.
- [ ] Commits name the `meko` component, and a `feat`, `fix`, `perf` or `deps` subject means it
      changes what Meko ships (it cuts a Meko release).
- [ ] Secrets (the client token, later provider keys) live only in `internal/secrets` or the client
      token's file, and never reach a log, an error, or a command line other than an installer's
      own `--meko-key`. The Keychain is written through `security -i` on stdin.
- [ ] The service files keep their stop timeout above serve's own shutdown (35 s), exit 75 stays
      "don't restart" for systemd and a launchd-run serve waits for the folder instead, and the CLI
      uses a service only for the data folder it runs. Releases stay outside the data folder.
- [ ] An update is checked before it's switched to (checksum, then the staged `meko version --json`),
      and anything that changes the database's schema keeps rollback working: `Migrate` backs the
      file up, and `store.RestoreBackup` can put it back.
- [ ] A release keeps working offline: the tarball bundles the models, the embedder and the reranker
      (`MEKO_MODELS_OFFLINE`), and
      `scripts/smoke-release.sh` passes on each platform.
- [ ] Tests never touch the user's own service, Keychain, `~/.meko` or `agentk`.
- [ ] Knowledge files' bytes stay in `knowledgebase-files/` (0700 folders, 0600 files), written to a
      temporary name and renamed before their row commits, removed after it's deleted; their text
      never reaches a log or an error reply.
- [ ] The docs in `docs/` and `AGENTS.md` here match the code, and so does the repo root's
      `README.md` (Meko's commands, their flags and the build requirements). They describe Meko as
      it is, without status lines, milestones, PR or issue numbers or history, and say nothing of
      Meko Cloud's internals beyond the behavior Meko matches.
