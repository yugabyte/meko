# REVIEW.md: Meko backend

What to check when reviewing a change to `backend/`, whether you're a person or an agent. Order
findings by severity: **blocker** (breaks a rule below, loses data, or opens a security hole),
**major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

- [ ] `make test` and `make lint` pass in `backend/`; new behavior has a test.
- [ ] Every database call goes through `internal/store`. The only other package that imports the
      SQLite driver is `internal/sqlitevec`, which links SQLite's C code; only its test runs a query.
      Migrations are append-only (`internal/store/migrations/`), like AgentK's.
- [ ] Every route but `/healthz` needs a credential, checked on the matched route (`c.Path()`),
      never the raw URL: the admin token on `/api/v1/status` and `/api/v1/version`, the admin or
      client token everywhere else, and nothing there only with `[server] auth = "none"`
      ([docs/api.md](docs/api.md)). Tokens and call-log rows never hold memory or turn text. The store API and the engine's API listen only
      on their unix sockets (0600) and need the internal token on every route, the engine's
      `/healthz` too (its reply can say why the engine failed). The server binds 127.0.0.1 only, checks `Host`,
      and refuses requests with an `Origin` until a browser client exists.
- [ ] Files in the data folder are owner-only (folders 0700, files 0600), and tokens and memory text
      never reach logs or error replies (internal errors get the generic message), in Go or in the
      engine (`logs/engine.log`).
- [ ] MCP tools keep Meko Cloud's names, arguments and reply shapes ([docs/api.md](docs/api.md)),
      built in `internal/wire` when REST shares them; errors a caller can act on are replies
      (`{"error", "detail"}`, `isError: false`); every call is recorded in the call log without its
      arguments or text. A search takes a search slot; nothing holds the writer while it waits on
      the engine. A change to what AgentK's `meko.ts` sends or reads updates the replay in
      `internal/mcp/agentk_contract_test.go`.
- [ ] The engine stores only through the store API, its dependencies stay pinned, and nothing copies
      or cites prod Meko's private code.
- [ ] `third_party/` is vendored and unchanged; an upgrade replaces the files and updates `VERSION`.
- [ ] Commits are `chore(meko): ...` (or `chore(meko/<area>): ...`): nothing in `backend/` ships
      in AgentK Local's release yet.
- [ ] `docs/design.md`, `docs/memory-engine.md` and `AGENTS.md` here match the code.
