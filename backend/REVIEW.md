# REVIEW.md: Meko Local backend

What to check when reviewing a change to `backend/`, whether you're a person or an agent. Order
findings by severity: **blocker** (breaks a rule below, loses data, or opens a security hole),
**major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

- [ ] `make test` and `make lint` pass in `backend/`; new behavior has a test.
- [ ] Every database call goes through `internal/store`. The only other package that imports the
      SQLite driver is `internal/sqlitevec`, which links SQLite's C code; only its test runs a query.
      Migrations are append-only (`internal/store/migrations/`), like AgentK's.
- [ ] Every route but `/healthz` needs a credential (the admin token in this phase), checked on the
      matched route (`c.Path()`), never the raw URL. The server binds 127.0.0.1 only, checks `Host`,
      and refuses requests with an `Origin` until a browser client exists.
- [ ] Files in the data folder are owner-only (folders 0700, files 0600), and tokens and memory text
      never reach logs or error replies (internal errors get the generic message).
- [ ] `third_party/` is vendored and unchanged; an upgrade replaces the files and updates `VERSION`.
- [ ] Commits are `chore(backend): ...`: nothing in `backend/` ships in AgentK Local's release yet.
- [ ] `docs/design.md` and `AGENTS.md` here match the code.
