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

## MCP at /mcp (PR 2)

`internal/mcp` serves Meko Cloud's tools on the official `modelcontextprotocol/go-sdk` (v1.8.0),
mounted at `/mcp` behind the server's Host, Origin and credential checks, with an 8 MB body limit.

- **Transport**: `mcp.NewStreamableHTTPHandler` with `Stateless` and `JSONResponse`: no session
  ids, every POST answered with `application/json`. The SDK handles `initialize` and version
  negotiation, `ping`, notifications (202), `tools/list`, 405 for `GET` and `DELETE`, and the
  2026-07-28 protocol's sessionless requests and `server/discover`. An old-protocol client's
  `server/discover` (Claude Code probes with it) gets `-32601` with HTTP 200. POSTs must accept
  both `application/json` and `text/event-stream`, as the spec says; Claude Code and the
  TypeScript SDK do. `serverInfo` is `{name: "Meko", version}`, with our own `instructions`.
- **Tools** are added with the SDK's low-level `AddTool`, which hands the handler the raw
  arguments and validates nothing. They read them as loosely as Cloud does (`"10"` is 10, `"true"`
  is true, `metadata` and `messages` as JSON strings or values). An argument no tool knows is
  ignored and logged at debug level; Cloud rejects it. A value that can't be read as its type fails
  the call (`isError: true`), and so does an unknown tool (a JSON-RPC error).
- **Replies** are Cloud's: the reply's JSON as the text content, and `structuredContent` of
  `{"result": <that text>}` (`track_token_usage`'s is the object itself). Errors a caller can act
  on are replies too, `{"error": "<code>", "detail": "<text>"}` with `isError: false`, and a few
  of Cloud's legacy paths reply in plain text (`Error: metadata must be a valid JSON string.`). An
  unexpected failure is logged and replies with the tool's failure code (`search_failed`,
  `memory_add_failed`, ...) and a generic detail.
- **Shared rules**, as Cloud's:
  - An empty `conversation_id` gets `conversation_id_required`, with Cloud's exceptions.
    `conversation_create` and `conversation_list` don't need one. `memory_search` advertises it
    as required but takes a blank one. The tools that act on a conversation
    (`conversation_add_message`, `_get`, `_update`, `_delete`) look a blank id up like any other
    and reply `not_found`. `track_token_usage` replies `{"recorded": false, "error":
    "conversation_id_required"}`. Hyphens are stripped.
  - A blank `agent_id` is `meko_agent`.
  - A blank `datapack_id` is the default datapack. If the user has deleted it, a call naming
    none gets `datapack_id_required`, as Cloud answers a caller with no default. A datapack that
    doesn't exist gets `datapack_access_denied`, except on `datapack_update` and `datapack_delete`,
    which pass on Cloud's API code, `NOT_FOUND`.
  - Memory ids must be UUIDs (`invalid_memory_id`). One that's missing, someone else's, or
    (for update and delete) another agent's gets `not_found`.
  - With the engine down: `engine_unavailable`, saying why.
- Every call is recorded in the call log, with its counts and steps and the credential it came in
  with. A call that replied with an error, in-band or in plain text, is recorded as failed, with
  its code.
- Cloud's internal arguments (`conversation_add_message`'s `skip_search_embed`) are read but left
  out of `tools/list`.
- One SDK server per credential (admin, client, none) is made up front, so a tool knows the
  credential it records without reading the request.

| Tool | Reply | Notes |
|---|---|---|
| `context_search` | `{results: {knowledge_base, memory, conversation: {hits, thread}}, token_savings}` | See below. |
| `memory_add` | `{"results": [{id, memory, event: "ADD", actor_id, role}]}` | Verbatim. `messages_missing`. |
| `memory_search` | `{"results": [{id, memory, hash, score, raw_similarity_score, created_at, updated_at, user_id, agent_id, run_id, role, metadata}]}` | Every agent's memories. Cosine below 0.2 dropped. `run_id` narrows to a conversation. |
| `memory_get_all` | `{criteria, memories: [{id, data, agent_id, conversation_id, user_id, created_at, is_mine}], top_k, total, page, page_size, total_pages}` | The 20 newest; `total` counts them all. There's no `page` argument, as in Cloud. `promoted: true` lists promoted memories (PR 3). |
| `memory_get_by_id` | The memory, without a score. | |
| `memory_update`, `memory_delete_by_id` | `{"message": "Memory updated successfully!"}` / `"... deleted ..."` | The writer's agent only. |
| `memory_delete_all` | `{"message": "Memories deleted successfully!"}` | One agent's memories; `run_id` must be a 32-hex conversation id (`invalid_run_id`). Only an absent, null or empty `run_id` deletes them all; one that's blank after normalizing (`-`, spaces) is refused, as in Cloud. |
| `conversation_create` | `{id, agent_id, user_id, app_id: null, run_id, title, metadata, created_at, updated_at}` | The same `session_id` is the same conversation. `invalid_title`, `invalid_metadata`. |
| `conversation_add_message` | `{status: "accepted", message_id, conversation_id[, conversation_search_skipped]}` | `not_found`, `agent_id_mismatch`. |
| `conversation_get` | The create shape plus `message_count`, and `messages` with `include_messages`. | The owner agent only. |
| `conversation_list` | `{datapack_id, datapack_name, langfuse_project_id: null, limit, offset, has_more, conversations}` | Most recent first, 200 at most. |
| `conversation_update` / `_delete` | The create shape / `{status: "deleted", conversation_id}` | `no_fields`. |
| `datapack_list` | A bare array of `{datapack_id, datapack_name, description, grant: "owner", user_id, created_at, updated_at}` | Newest first; `query`, `limit`. |
| `datapack_search` | `{datapacks, has_more, next_cursor, total}` | `next_cursor` is an offset. |
| `datapack_create` | `{datapack_id, agent_id, name}` | 3 to 60 characters, no HTML: `VALIDATION_ERROR`; a taken name: `CONFLICT`. |
| `datapack_describe` | The row plus `memory_count, learnings_count, collective_memory_count, knowledge_count, knowledge_chunk_count, knowledge_base_file_count, shared_knowledge_count, conversation_count, conversation_search_opt_out` | Knowledge counts are 0 until milestone 4. |
| `datapack_update` / `_delete` | `{"message": "datapack updated"}` / `"... deleted"` | Update also sets the conversation-search opt-out; nothing to change replies `{"result": "nothing_to_update"}`. |
| `track_token_usage` | `{"recorded": true}` | Token counts go to the call log. A missing `conversation_id`, a negative count or a bad datapack replies `{"recorded": false, "error"}`. |

Milestone 4 (#170) adds `knowledgebase_search` and the knowledge-file tools; they aren't in
`tools/list` until then.

### context_search

- Its sources run at the same time (`memory.ContextSearch`), so it takes as long as the slower:
  memory search (the engine's hybrid search, Meko Cloud's 0-1 `score` and the cosine as
  `raw_similarity_score`), and conversation search (milestone 2's, which leaves out the caller's own
  conversation).
- A source is `null` when it wasn't asked for (`force_source`) or the engine couldn't take the
  query; empty when it found nothing. A source that fails otherwise is logged and empty, as in
  Cloud. `knowledge_base` is always `null` until milestone 4.
- No score floor on `auto`; with `force_source: "memory"`, memory_search's 0.2 cosine floor.
- `token_savings` is Cloud's estimate: 200 tokens per memory and 1000 for any conversation hit,
  against a baseline of 10000 (Cloud's for a datapack with no knowledge files). Each source is
  there only when it returned something.

## Responsiveness (PR 2)

A slow search must never make other requests wait (the PRD). The 500 ms p95 target at 200 MB is
#192's.

- `context_search` runs its sources concurrently.
- The store's read pool has 8 connections. Vector, keyword and conversation-chunk scans each take
  one of 4 search slots first (`store.searchSlot`), so at least 4 connections stay free for lists,
  gets and counts. A search whose context ends while it waits for a slot gives up.
- No write transaction is open while Go waits on the engine: the engine calls back into the store
  API and needs the single writer.

## CLI (PR 2)

`meko search "<question>"`, `meko add "<text>"` and `meko datapacks` are MCP clients of the
running server: they call `context_search`, `memory_add` and `datapack_list` on `/mcp` with the
admin token, as agent `meko_cli`, so each shows exactly what an agent would get. Their calls carry a
fixed conversation id derived from `meko-cli` (Cloud's tools require one). `--datapack` takes a
name or an id, `--json` prints the raw reply, and they exit 3 when no server runs. `meko token
--mcp` prints the `claude mcp add` command that registers Meko, named `meko`: AgentK recognizes
Meko's tools by that name (`mcp__meko__*`). `meko logs` comes with the services in #171.
