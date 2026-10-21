# Meko backend: MCP and REST

Status: approved; being built. Milestone 3 of the backend (issue #169), on the foundation in
[design.md](design.md) and the memory engine in [memory-engine.md](memory-engine.md). Built in
stacked PRs; each section below names the PR that adds it, so the doc matches the code at every
step:

| PR | Adds |
|---|---|
| #197 | The rename from Meko Local to Meko (merged) |
| 1 | The client token, the `[server] auth` setting, the call log; this doc |
| 2 | MCP at `/mcp`, the search slots, and `meko search`, `add` and `datapacks` |
| 3 | Learnings (keep and promote), artifacts, provenance, and the replay of AgentK's Meko calls |
| 4 | The `/api/v1` REST subset meko_ui calls |

Milestone 3 puts milestone 2's service layer (`internal/memory`) on the wire. Agents (Claude Code,
AgentK) reach Meko over MCP, and meko_ui (from milestone 6) and the CLI over `/api/v1`, with Meko
Cloud's tool names, arguments and reply shapes, written here from scratch.

## Auth (PR 1)

| Route | Admin token | Client token | No credential |
|---|---|---|---|
| `/healthz` | yes | yes | yes |
| `/api/v1/status`, `/api/v1/version` | yes | 403 | 401 |
| Every other route (`/mcp`, the data routes) | yes | yes | 401, or yes with `auth = "none"` |

- **The admin token** is the CLI's, as in the foundation: new at each start, in `run/admin.token`.
- **The client token** is what MCP and REST clients send as `Authorization: Bearer <token>`:
  32 random bytes, hex, in `client.token` (0600) at the data folder's root. `serve` (or
  `meko token`, if it runs first) makes it once, and it's kept across restarts, because Claude
  Code's MCP config and AgentK store it once. It lives outside `run/`, which `serve` clears at
  each start. Making it is atomic (a hard link of a complete temp file), so two processes making it
  at once agree on one token. A token file other users have any access to, that another user
  owns, that isn't a regular file, or that holds no token is refused. Delete the file and restart
  to replace it; every client then needs the new one. While the server runs, `meko token` only
  reads the file, and says to restart when it's gone: the server keeps the token it loaded.
- `meko token` prints it, whether or not the server runs.
- **`[server] auth = "none"`** (or `MEKO_AUTH=none`) lets `/mcp` and the data routes take requests
  with no credential; any `Authorization` header is then ignored unless it's the client or admin
  token. The default is `"token"`. An unknown value is refused at start, so a typo never turns auth
  off. Everything else stays: status and version still need the admin token, the server still
  listens on 127.0.0.1 only, and the Host and Origin checks still apply. Each start logs a warning,
  and `meko status` shows `Auth: none` with what it means.
- The credential a request got in with (`admin`, `client` or `none`) is recorded with each call,
  never the token. Every 401 says `WWW-Authenticate: Bearer`.
- Per-client API keys, which the PRD asks for, are #195.

## The call log (PR 1)

`call_log` (`0004_call_log.sql`) gets one row per MCP tool call, which replaces Langfuse's traces
for metrics. Each row records:

- the time, tool, datapack, agent, conversation id and credential;
- whether it worked (with its error code) and how long it took;
- the result counts per source (memory, knowledge base, conversation);
- the results by match quality, as Meko Cloud bands them: 0.75 and up, 0.5 and up, below;
- `context_search`'s token estimate (sent and saved), and what `track_token_usage` reports;
- its steps as JSON (`[{step, ms, n}]`), enough for milestone 6's trace drawer to show timings.

It never holds arguments, text or tokens. It has no foreign key to datapacks, so a deleted
datapack's history stays until it's pruned. `serve` prunes rows older than 90 days at start and
then daily (`memory.RunCallPruner`). The rows don't count toward the 200 MB content target.
