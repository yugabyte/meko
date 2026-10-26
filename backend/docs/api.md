# Meko backend: MCP and REST

Agents (Claude Code, AgentK, and the CLI's tool commands) reach Meko over MCP at `/mcp` on the API
listener, and the web UI ([web-ui.md](web-ui.md)) over `/api/v1` on the web listener ([web.md](web.md)), with Meko Cloud's tool names, arguments, paths and
reply shapes, written here from scratch. Both call the service layer in `internal/memory`
([memory-engine.md](memory-engine.md)); the knowledge-file tools and routes are designed in
[knowledge.md](knowledge.md) and listed here beside the others.

## Auth

On the API listener (`127.0.0.1:7778`):

| Route | Admin token | Client token | No credential |
|---|---|---|---|
| `/healthz` | yes | yes | yes |
| `/api/v1/status`, `/api/v1/version`, `/api/v1/auth/setup-link` (`meko setup_link`) | yes | 403 | 401 |
| `/api/v1/server` (the web UI's status page: `/api/v1/status`'s reply) | yes | yes | 401, or yes with `auth = "none"` |
| Every other route (`/mcp`, the data routes) | yes | yes | 401, or yes with `auth = "none"` |
| The knowledge-file links, `/api/v1/knowledge-uploads/<ticket>` and `/api/v1/knowledge-downloads/<ticket>` | yes | yes | yes: the ticket is the credential |

The web listener ([web.md](web.md)) serves the same `/mcp`, data routes, `/api/v1/server` and
knowledge-file links to a browser with local access or the owner's session, never with a token, and
has no admin-only routes. Its own routes:

| Route | Credential | What |
|---|---|---|
| `GET /api/v1/auth/status` | none | `{access, local_access, signed_in, setup_required, reset, username, totp_enabled}`: `setup_required` while remote access has no login (or a setup link waits and this browser isn't signed in), `reset` when that link replaces a login |
| `POST /api/v1/auth/setup` `{token, username, password}` | none: the setup link's token | Sets the login, signs every browser out, and signs this one in (the cookie). 400 `VALIDATION_ERROR`, 403 `FORBIDDEN` (a link that's wrong or used), 429 `RATE_LIMITED` |
| `POST /api/v1/auth/login` `{username, password, totp?}` | none | Signs in (the cookie). 401 `BAD_LOGIN`, 401 `TOTP_REQUIRED` (no code, or a wrong one: the message says which), 429 `RATE_LIMITED` |
| `POST /api/v1/auth/logout` | none | Ends this browser's session and the WebSockets it opened |
| `POST /api/v1/auth/password` `{current, next}` | session or local | Signs every other browser out. 400 `VALIDATION_ERROR`, 409 `CONFLICT` (no login yet) |
| `POST /api/v1/auth/totp/enroll`, `confirm {code}`, `disable {code}` | session or local | Enroll gives `{secret, otpauth_url}`; confirm turns two-step sign-in on with a code from it; disable turns it off with a current code |
| `GET /api/v1/extensions` | session or local | `[{name, title, description, installed, web_ui, path, available, job?}]`: every extension Meko knows, sorted by name. `installed` is registered in `extensions.json`; `path` is `/ext/<name>/` while it's served, else `""`; `available` is whether it accepts a connection on its socket (it's running; a socket file left by one that died doesn't count); `job` is the last action the Extensions page ran on it since the server started, `{action, state, started_at, finished_at?, error?, output}`, `state` `running`, `done` or `failed`, `output` its last 16 KiB with the client token blanked |
| `POST /api/v1/extensions/<name>/<action>` | session or local | `install`, `start` or `stop`, run in the background as the extension's job: 202 with its row. An install or a start of one with a web UI is done once it answers on its socket (30 s at most). 404 `NOT_FOUND` (not a known extension or action), 409 `CONFLICT` (start or stop on one that isn't installed, or a job running for it) |
| `/ext/<name>/*` | session or local | The extension's web UI ([web.md](web.md)); a page without a session goes to `/signin?next=` |
| The web UI's files: `/` and every GET no other route takes | none | Static |

- **The admin token** is the CLI's ([design.md](design.md#auth)): new at each start, in `run/admin.token`.
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
  off. Everything else stays: status and version still need the admin token, the API listener still
  listens on 127.0.0.1 only, and the Host and Origin checks still apply. It doesn't touch the web
  listener, which never takes a token. Each start logs a warning,
  and `meko status` shows `Auth: none` with what it means.
- The credential a request got in with (`admin`, `client` or `none`, and on the web listener
  `session` or `local`) is recorded with each call, never the token. `knowledgebase_upload_file`,
  which reads a path as Meko's user, takes only `admin` or `client`. Every 401 on the API listener
  says `WWW-Authenticate: Bearer`; the web listener's don't, since it takes no Bearer token.
- Per-client API keys aren't built yet: every client shares the client token.
- **The knowledge-file links** take no token, as S3's presigned URLs need none: a
  browser form post, a new download tab and an agent posting to an upload link can't send one.
  Their ticket is 256 random bits, kept in memory for 5 minutes and good once; an upload's is
  bound to its datapack, key, exact size and SHA-256. The Host and Origin checks still apply. The access log
  records them like any request; the call log holds MCP calls only.

## The call log

`call_log` (`0004_call_log.sql`) gets one row per MCP tool call, which metrics, usage and the
web UI's decision trace are computed from. Each row records:

- the time, tool, datapack, agent, conversation id and credential;
- whether it worked (with its error code) and how long it took;
- the result counts per source (memory, knowledge base, conversation);
- the results by match quality, as Meko Cloud bands them: 0.75 and up, 0.5 and up, below, by cosine
  similarity (a memory's `raw_similarity_score`, else its `score`; 1 − `distance` for the others),
  counted when the call is recorded;
- `context_search`'s token estimate (sent and saved), and what `track_token_usage` reports;
- its steps as JSON (`[{step, ms, n, ids}]`), enough for the web UI to show timings. A memory search's step also lists the ids of the memories it returned (`ids`), so a turn's trace can show what the agent saw; calls recorded before that have none.

It never holds arguments, text or tokens. It has no foreign key to datapacks, so a deleted
datapack's history stays until it's pruned. `serve` prunes rows older than 90 days at start and
then daily (`memory.RunCallPruner`). The rows don't count toward the 200 MB content target.

## MCP at /mcp

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
  - With the memory engine down: `memory_engine_unavailable`, saying why.
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
| `memory_get_all` | `{criteria, memories: [{id, data, agent_id, conversation_id, user_id, created_at, is_mine}], top_k, total, page, page_size, total_pages}` | The 20 newest; `total` counts them all. There's no `page` argument, as in Cloud. `promoted: true` lists promoted memories instead: `{criteria, promoted_memories: [{id, conversation_id, agent_id, data, created_at, promoted_to_kb_at}], total, page, page_size, total_pages}`. |
| `memory_get_by_id` | The memory, without a score. | |
| `memory_update`, `memory_delete_by_id` | `{"message": "Memory updated successfully!"}` / `"... deleted ..."` | The writer's agent only. A promoted id gets `memory_is_promoted` (with `memory_id`, `promoted_row_id`, `operation`). |
| `memory_promote` | `{inserted_ids, updated_ids, not_found_ids}` | `memory_ids_required`, `invalid_memory_id`. A kept memory, or one that isn't the local user's, is not found. |
| `memory_delete_all` | `{"message": "Memories deleted successfully!"}` | One agent's memories; `run_id` must be a 32-hex conversation id (`invalid_run_id`). Only an absent, null or empty `run_id` deletes them all; one that's blank after normalizing (`-`, spaces) is refused, as in Cloud. |
| `conversation_create` | `{id, agent_id, user_id, app_id: null, run_id, title, metadata, created_at, updated_at}` | The same `session_id` is the same conversation. `invalid_title`, `invalid_metadata`. |
| `conversation_add_message` | `{status: "accepted", message_id, conversation_id[, conversation_search_skipped]}` | `not_found`, `agent_id_mismatch`. |
| `conversation_get` | The create shape plus `message_count`, and `messages` with `include_messages`. | The owner agent only. |
| `conversation_list` | `{datapack_id, datapack_name, langfuse_project_id: null, limit, offset, has_more, conversations}` | Most recent first, 200 at most. |
| `conversation_update` / `_delete` | The create shape / `{status: "deleted", conversation_id}` | `no_fields`. |
| `datapack_list` | A bare array of `{datapack_id, datapack_name, description, grant: "owner", user_id, created_at, updated_at}` | Newest first; `query`, `limit`. |
| `datapack_search` | `{datapacks, has_more, next_cursor, total}` | `next_cursor` is an offset. |
| `datapack_create` | `{datapack_id, agent_id, name}` | 3 to 60 characters, no HTML: `VALIDATION_ERROR`; a taken name: `CONFLICT`. |
| `datapack_describe` | The row plus `memory_count, learnings_count, collective_memory_count, knowledge_count, knowledge_chunk_count, knowledge_base_file_count, shared_knowledge_count, conversation_count, conversation_search_opt_out` | As Cloud counts: `knowledge_chunk_count` (and its old name `knowledge_count`) is every knowledge-base chunk, promoted memories' too; `knowledge_base_file_count` is 1 when the datapack has a knowledge base, else 0 (Cloud counts sources, not files); `shared_knowledge_count` is promoted memories plus that. |
| `datapack_update` / `_delete` | `{"message": "datapack updated"}` / `"... deleted"` | Update also sets the conversation-search opt-out; nothing to change replies `{"result": "nothing_to_update"}`. |
| `track_token_usage` | `{"recorded": true}` | Token counts go to the call log. A missing `conversation_id`, a negative count or a bad datapack replies `{"recorded": false, "error"}`. |
| `artifact_put` | `{artifact_id, content_hash, filename, content_type, size_bytes, stored_in: "db", created_at}` | 5 MiB at most (`artifact_too_large`); `invalid_base64`. The same bytes are kept once. |
| `artifact_get` | The same, plus `content_base64` | Always inline, never `local_path`. `ARTIFACT_NOT_FOUND`. |

The knowledge-file tools ([knowledge.md](knowledge.md)):

| Tool | Reply | Notes |
|---|---|---|
| `knowledgebase_search` | `{results: [{id, chunk_text, document_id, metadata_filters, distance, match_type, rerank_score, rank?}], count}` | `query`, `conversation_id`, `datapack_id` (blank is the default), `limit` (widens the candidates; it doesn't cap the reply, as in Cloud), `skip_conversation` (ignored). A promoted memory's `metadata_filters` is `{source: "memory", agent_id, memory_id}` and its `document_id` its conversation. No filename, as in Cloud. `rerank_score` is absent when the reranker can't run. |
| `knowledgebase_upload_document` | `{datapack_id, url, fields, key, already_uploaded}`, or `{datapack_id, url: "", key, already_uploaded: true, existing_filename}` | `filename`, `size`, `content_hash` (SHA-256 hex), `content_head_base64` (the first 3072 bytes). POST the file to `url` as multipart, `fields` first, the file as `file`. `VALIDATION_ERROR` (type, size, hash, a head that doesn't fit the type), `content_head_base64_required`. |
| `knowledgebase_upload_file` | `{datapack_id, already_uploaded, ...}` with `knowledgebase_document_status`'s reply | Not in Cloud: `path`, a file on this machine, which Meko reads itself, so an agent uploads in one call without encoding it. An absolute path to a regular file of a type Meko takes; anything else is `VALIDATION_ERROR`. It reads as the server's user, so it needs the admin or client token even with `auth = "none"` (`token_required`): a caller with none could be another local user. |
| `knowledgebase_document_status` | `document-status`'s reply (below) | `document_key`. `NOT_FOUND`. |
| `knowledgebase_delete_document` | `{document_id, chunks_deleted, s3_deleted: true}` | `document_id` (UUID: `invalid_document_id`). `{error: "NOT_FOUND", detail, status: 404}`, and `DOCUMENT_PROCESSING` (409) while the file is queued or indexing. |

### context_search

- Its sources run at the same time (`memory.ContextSearch`), so it takes as long as the slowest:
  memory search (the memory engine's hybrid search, Meko Cloud's 0-1 `score` and the cosine as
  `raw_similarity_score`), knowledge-base search ([knowledge.md](knowledge.md#search), keeping results within
  cosine distance 0.6, and reranked keyword or hybrid matches past it), and conversation search
  ([memory-engine.md](memory-engine.md#conversations-internalmemory), which leaves out the caller's
  own conversation).
- A source is `null` when it wasn't asked for (`force_source`) or the memory engine couldn't take the
  query; empty when it found nothing. A source that fails otherwise is logged and empty, as in
  Cloud.
- No score floor on `auto`; with `force_source: "memory"`, memory_search's 0.2 cosine floor.
- `token_savings` is Cloud's estimate: 1000 tokens per knowledge-base result, 200 per memory and
  1000 for any conversation hit, against a baseline of the knowledge-base results' tokens when there
  are any, else the whole knowledge base's (1000 a chunk), else 10000. Each source is there only
  when it returned something.

## Responsiveness

A slow search must never make other requests wait.

- `context_search` runs its sources concurrently.
- The store's read pool has 8 connections. Vector, keyword and conversation-chunk scans each take
  one of 4 search slots first (`store.searchSlot`), so at least 4 connections stay free for lists,
  gets and counts. A search whose context ends while it waits for a slot gives up.
- No write transaction is open while Go waits on the memory engine: the memory engine calls back into the store
  API and needs the single writer.

## CLI

`meko search "<question>"`, `meko add_memory "<text>"`, `meko datapacks`, `meko create_datapack
<name>` and `meko delete_datapack <name|id>` are MCP clients of the running server: they call
`context_search`, `memory_add`, `datapack_list`, `datapack_create` and `datapack_delete` on `/mcp`
with the admin token, so each shows exactly what an agent would get. `meko datapacks --cloud` also
lists the linked Meko Cloud's datapacks after this Meko's, from `GET /api/v1/meko-cloud/datapacks`,
each row marked `local` or `cloud` (and `shared`); `--json` gives `{local, cloud}`. While unlinked it
says so on stderr and exits 0; when Meko Cloud fails it still prints the local rows, then exits 1. `search` and `add_memory`
call as agent `meko_cli`; the datapack tools take no agent. Their calls carry a fixed conversation
id derived from `meko-cli` (Cloud's tools require one). `--datapack` takes a name or an id,
`--json` prints the raw reply, and they exit 3 when no server runs. `delete_datapack` deletes the
datapack and everything in it, so it asks `Delete it? [y/N]` first (as `meko destroy` does: it
reads stdin, end of input means no, and a no exits 1), unless `--force`. It says so when that's the default
datapack: Meko makes the default only on first start, and once it's gone `search` and
`add_memory` need `--datapack`, which they say instead of the tool's `datapack_id_required`. `meko upload <file>...` calls `knowledgebase_upload_document`, posts each
file to its link and follows `knowledgebase_document_status` until it's indexed, a file already uploaded too
(`--no-wait` returns once it's uploaded; a file that fails makes the command exit 1), and `meko upload_status
<key>`. `meko list mcp-tools` prints each tool with its description's first sentence (`--json` adds titles),
from the same definitions `/mcp` serves, so it needs no server. `meko ui [path]` opens the web listener's URL ([web.md](web.md)), at a path when given
(`/ext/agentk/` for AgentK); the link carries no credential, and `--no-open` only prints it. With remote
access and no login yet, it prints a setup link instead; `meko setup_link` prints one any time. `meko token
--mcp` prints the `claude mcp add` command that registers Meko, named `meko`: AgentK recognizes
Meko's tools by that name (`mcp__meko__*`), and `meko token --json` prints the MCP URL, the web
listener's (`web_url`, where AgentK finds its own address), the token,
whether a server runs and the Meko Cloud link with its key (`cloud`, [cloud.md](cloud.md)), for
extensions (AgentK reads it). `meko cloud_link` links Meko Cloud ([cloud.md](cloud.md)). `meko logs` and the commands that
install and run Meko are in [install.md](install.md).

## Learnings, promotion and artifacts

`0005_learnings_artifacts.sql` adds them.

- **Learnings**: `memories.promotion_action` is `''` for a learning (every memory starts as one,
  as in Meko Cloud) and `keep_as_memory` once the user keeps it (`POST /keep-as-memory`).
  The memory list's `scope=learnings` is the learnings; `scope=memories` (the default) is learnings
  and kept memories.
- **Promotion** (`memory_promote`, and `POST /promote-memories`) moves learnings into the
  datapack's collective memory, as Cloud does. Each is copied into `promoted_memories` with every
  column of its row but the lemmas (id, text, provenance, role, actor, hash, both dates), the copy
  commits, and then the memory is deleted through the memory engine (which also unlinks mem0's entities),
  so memory search no longer finds it. An id given twice is promoted once. A kept memory isn't a
  learning, so it's not found, as in Cloud. A separate table hides promoted memories from search
  without a filter on every scan. Updating or deleting a promoted id gets `memory_is_promoted`.
- **When the memory engine fails a delete**, the copy is removed only while the memory is still there: a
  request cut off after the memory engine got it may still have deleted it, and then the copy is all
  that's left (it's reported inserted). With the memory engine down or starting, the call stops with
  `memory_engine_unavailable` so the caller retries; any other failure leaves that one a learning, in
  `not_found_ids`. An error after some ids were done carries `inserted_ids` and `not_found_ids`
  beside `error` and `detail`. A memory left copied but not deleted (the undo failed too) is
  finished by promoting it again.
- **Counts** (`datapack_describe`): `memory_count` is learnings and kept, `learnings_count` the
  learnings, `collective_memory_count` and `shared_knowledge_count` the promoted ones.
- Promotion also puts the memory into the knowledge base as a chunk with the
  memory's own vector, so `knowledgebase_search` finds it at once and `promoted_to_kb_at` is set
  ([knowledge.md](knowledge.md)).
- **Provenance**: `memories.connector_id` (`''` for none) beside `agent_id`, `conversation_id`
  and `source`, so every memory records where it came from from day one. Nothing sets it until
  connectors exist, and replies don't show it yet.
- **Artifacts**: `artifacts` holds the bytes in the database, once per datapack and SHA-256, up to
  5 MiB (Cloud's limit). `artifact_get` returns them inline as `content_base64`, which AgentK reads
  as well as Cloud's `local_path`, and records when the file was last read. It's annotated
  read-only as in Cloud: that time is bookkeeping, not a change to the artifact. The bytes go
  with the datapack's export, and count toward the 200 MB content target.

## AgentK's calls

`internal/mcp/agentk_contract_test.go` replays every call AgentK's Meko client makes
(`extensions/agentk/src/server/meko.ts`), with the same tool names and arguments:

- `check` (a ping);
- `ensureConversation`, including the service conversation with a null datapack;
- `listDatapacks`, `createDatapack`, `setDatapackDescription`, `describeDatapack`;
- `retitleConversation`;
- `addMemory`, `similar`, `updateMemory` (as the writer and with a null agent);
- `search`;
- `putArtifact`, `getArtifact`.

It checks each reply has every field MekoClient reads. When a method in `meko.ts` changes a call
or reads a new field, change its step there. Once AgentK's own tests run against a real Meko,
this replay can go.

## REST at /api/v1

`internal/rest` serves the subset of Meko Cloud's REST routes that the web UI, AgentK and
`meko export` use, plus a few of Meko's own (marked below), with Cloud's paths and shapes in the
`{data, error}` envelope, mounted on the server's `/api/v1` group (1 MB body limit), behind the
same credential check as `/mcp`. Shapes MCP shares come from `internal/wire`; REST-only ones are in
the package. Its browser client is the web UI ([web-ui.md](web-ui.md)), the only page whose origin
the server takes. (The CLI calls `/api/v1/status`, which `internal/server` serves, and `meko export` calls
`/datapacks/:id/export`.)

- A `datapack_id` that isn't a UUID gets 400 `VALIDATION_ERROR`, and one that doesn't exist
  gets 404 `NOT_FOUND`. Any form of UUID Cloud's uuid column takes (upper case, no hyphens,
  braces) is read as the id. A route that isn't served gets 404 `NOT_FOUND` too, so a reply
  doesn't tell the two apart, and a path that only matches a route's pattern, such as
  `/datapacks/incoming-shares` (sharing isn't served), gets that route's 400.
- A body that isn't JSON gets 400 `INVALID_JSON`.

| Area | Routes | Notes |
|---|---|---|
| Local user | `GET /auth/config`, `GET /auth/me`, `PATCH /auth/me`, `PATCH /auth/me/profile`, `GET /auth/me/usage` | `{provider: "none"}`. `/auth/me` gives the install's user as `account_id`, the profile kept in `settings` (Local User until changed), `email_verified: true`, `user_type: "pro"`; onboarding starts done (the web UI has no onboarding). Usage counts datapacks, conversations, searches, adds and tracked tokens. |
| Datapacks | `GET`, `POST /datapacks`; `GET`, `PATCH`, `DELETE /datapacks/:id`; `GET /datapacks/:id/provision-status`, `/agents`, `/conversations/count` | The list is newest first, every row `grant: "owner"` with the local user as owner, counts left at 0 as in Cloud; describe counts. `POST` replies 201 with Cloud's blanked `mcp_server` and `db_access`. `provision-status` gives the MCP URL and the client token as `mcp_api_key`, for the Connect page, only to a request that sent a token or comes from the owner signed in on the web listener: with `auth = "none"`, or with the web listener's local access (which every user of the computer has), it's `""`, and the page says to run `meko token`. The token lets a caller do what those don't (`knowledgebase_upload_file` reads a path as Meko's user), and keeps its meaning once auth or access changes. Agents are those seen in memories, conversations and the call log. |
| Metrics | `GET /datapacks/:id/metrics`, `GET /datapacks/:id/token-savings` | From the call log (90 days): searches (`context_search` as hybrid, `memory_search` as memory, `knowledgebase_search` as kb), tokens sent and saved, score bands, agents, last access, 30 days of activity. Sizes come from the store, knowledge files' (`kb_file_count`, `kb_chunk_count`, `kb_bytes`) too. Extraction fields are 0 until extraction is built. `agent_count` leaves out the web UI's own agent, `meko_web_ui`: it's the user. `?days=N` (Meko's own) narrows the searches and the activity to the last N days (at most 90); without it they're Cloud's: 90 days of totals, 30 of activity. |
| Export (Meko's own) | `GET /datapacks/:id/export` | One datapack as a zip to download (`meko-<name>-<date>.zip`), streamed. JSON Lines per kind: `memories.jsonl` (`{id, text, hash, state (learning or kept), agent_id, conversation_id, role, actor_id, attributed_to, expiration_date, metadata, created_at, updated_at, history, embedding}`), `collective_memories.jsonl` (`{..., source, connector_id, promoted_at, promoted_by, embedding}`), `conversations.jsonl` (`{id, title, agent_id, session_id, run_id, metadata, created_at, updated_at, turns: [{id, input, output, reasoning, plan, metadata, created_at, chunks: [{index, text, embedding}]}]}`), `knowledge.jsonl` (`{id, filename, type, sha256, size_bytes, status, error, uploaded_at, completed_at, path, chunks: [{index, text, metadata, embedding}]}`), `artifacts.jsonl` (`{id, filename, content_type, sha256, size_bytes, agent_id, created_at, path}`) and `call_log.jsonl` (every call with its steps). The bytes are at each `path`: `knowledge/<sha256>.<ext>` and `artifacts/<sha256>.<ext>`. Knowledge files live on disk, outside the snapshot, so one deleted while the export runs goes out with `path: ""` and no bytes. `datapack.json`, written last, is `{format: "meko-datapack-export", version: 2, exported_at, meko_version, embedding: {model, dims}, left_out, counts, datapack}`. An `embedding` is null until the sweep has embedded its text. Ids and dates are kept, and every read goes through one snapshot of the database, so writes made while it runs neither drop nor repeat a row; a memory a promotion stopped partway on is exported once, as a collective memory. Keyword indexes and entities are left out (rebuilt from the text on import). Nothing imports it yet: it's the first half of moving a datapack to Meko Cloud. A failure before the body starts gets an error reply; one after it is logged and the connection is cut, so the download fails instead of ending cleanly. `meko export` calls it, checks the zip opens and holds `datapack.json`, and won't replace a file unless `--force`. |
| Memory map (Meko's own) | `GET /datapacks/:id/memory-map` | The datapack's newest 300 memories and up to 300 collective memories, laid out by meaning for the web UI's memory graph: `{points: [{id, text, state (learning, kept or collective), agent_id, conversation_id, created_at, x, y, similarity}], links: [{a, b, similarity}], query: [x, y]}`. `x` and `y` are the embeddings projected onto their two main directions (PCA by power iteration), scaled into [-1, 1]. Each memory is linked to its 3 nearest at a cosine of 0.35 or more (MiniLM scores short related facts 0.3 to 0.5); `a` and `b` index `points`. With `?q=`, the query is embedded and placed the same way (`query`), and each point gets its cosine to it (`similarity`). `?ids=` (comma-separated, up to 100) adds those memories even when they're older than the newest 300, as a turn's trace asks for what its searches returned; unknown or deleted ids are skipped. A memory whose promotion stopped partway is drawn once, as collective. A memory the sweep hasn't embedded yet is left out. |
| Usage (Meko's own) | `GET /datapacks/:id/usage` | From the call log (90 days), for the web UI's Metrics tab; Cloud has no such route. `{calls, errors, tools: [{tool, calls, errors, p50_ms, p95_ms}], steps: [{step, calls, p50_ms, p95_ms}], agents: [{agent_id, calls, searches, last_at}], recent: [{id, at, tool, agent_id, ok, error_code, ms, memory_n, kb_n, conversation_n, steps: [{step, ms, n, ids}]}] (the latest 25), input_tokens, output_tokens}`, over 90 days or `?days=N`. |
| Artifacts | `GET /datapacks/:id/artifacts`, `POST .../artifacts/:hash/download-url`, `GET .../artifacts/:hash` | Pages from 1 (25, at most 200), newest first, `q` on the filename. `download-url` has no `url` (the bytes are in the database), so a client reads them from the `GET`. A hash that isn't 64 hex: 400 `INVALID_PARAM`; missing: 404 `ARTIFACT_NOT_FOUND`. |
| Memories | `GET /memories`, `DELETE /memories/:id`, `GET /memories/:id/history` (Meko's own: `{memory_id, history: [{memory_id, action (ADD, UPDATE, DELETE), previous_value, new_value, created_at, updated_at, is_deleted}]}`, oldest first, as mem0 recorded them; only for a memory the local user can get, as `GET` on it would be: another user's, a deleted or a promoted memory is 404; the id is matched in any case), `POST /promote-memories`, `POST /keep-as-memory`, `GET /promoted-memories`, `DELETE /promoted-memories/:id` | The list takes `datapack_id`, `scope=learnings|memories`, `q` (any case) and pages of 20 (at most 100), newest first, `top_k: 0`, every row the local user's (`is_mine`). Promote and keep take `{datapack_id, memory_ids}` (UUIDs, any case). A promotion that stops partway (the memory engine went down) replies 500 `INTERNAL_ERROR` with `promoted <id>` in `details` for each one it did. |
| Knowledge files | Under `/datapacks/:id/knowledge-bases/`: `POST upload-url`, `upload-complete`, `create`, `download-url`; `GET document-status`, `status`, `documents`, `summary`, `pending-uploads`, `activity-log`; `DELETE pending-uploads?key=`, `documents/:document_id` | Cloud's shapes. `upload-url` `{filename, size, content_hash, client}` gives a link on Meko (`/api/v1/knowledge-uploads/<ticket>`) with `fields: {key}`, or `already_uploaded`; the file is registered and queued when it arrives (204), so `upload-complete` only checks the key and `create` only marks the datapack as having a knowledge base. `document-status` is `{document_key, filename, state (awaiting_upload, indexing, completed, failed), message, upload?, document?: {chunks_processed, uploaded_at, size_bytes, status}}` plus Cloud's top-level `index_name, source_uri, document_name, document_status, chunks_processed`. `documents` pages from 1 (50, at most 200) with `q`, `type_filter`, `status_filter`. `pending-uploads` is always empty (nothing is stored unregistered); its DELETE cancels a waiting link (409 `OBJECT_REFERENCED` for a stored file). `download-url` `{document_uri}` (`local://<key>`) gives a link (`/api/v1/knowledge-downloads/<ticket>`, `expires_in: 300`) that sends the file as an attachment, HTML as plain text. A key outside the datapack is 403 `FORBIDDEN`; deleting a file still indexing is 409 `DOCUMENT_PROCESSING`. |
| Usage stats | `GET /settings/usage-stats`, `PATCH /settings/usage-stats` | `{enabled, forced_off, by, sends, url}` ([callhome.md](callhome.md)); `url` is where they go while `sends`. `PATCH {enabled}` saves the web UI's setting (`call_home_enabled`); with usage stats forced off by `config.toml` or the environment it's saved, but they stay off. A missing `enabled` is 400 `VALIDATION_ERROR`. `/api/v1/status` (and `/api/v1/server`, which gives the same reply) carries the same state as `usage_stats`. |
| Meko Cloud link (Meko's own) | `GET`, `PUT`, `DELETE /settings/meko-cloud` | The link Meko keeps for its extensions ([cloud.md](cloud.md)): `{set, linked, url, web_url, api_url, effective_url, effective_web_url, effective_api_url, key_hint, default_url, default_web_url}`, never the key. `PUT {url?, web_url?, api_url?, api_key?}` checks a change before saving it; a refusal is 400 `VALIDATION_ERROR` with why. On the web listener for the owner; on the API listener for the admin token only (`meko cloud_link`), so a client token gets 403. |
| Meko Cloud datapacks (Meko's own) | `GET /meko-cloud/datapacks` | The linked Meko Cloud's datapacks, for the web UI's sidebar and `meko datapacks --cloud` ([cloud.md](cloud.md#listing-meko-clouds-datapacks)): `{linked, api_url, web_url, datapacks: [{datapack_id, datapack_name, description, grant, created_at, updated_at, web_url}]}`, newest first, each `web_url` the datapack in the link's web app (null when that isn't known); `{linked: false, datapacks: []}` while unlinked. Meko Cloud's failures are 502 with a code: `CLOUD_NO_API_URL`, `CLOUD_UNREACHABLE`, `CLOUD_KEY_REFUSED` or `CLOUD_LIST_FAILED`. It takes the credential every data route does, the client token included; the reply never holds the key. |
| Conversations | `GET /datapacks/:id/conversations`, `GET /datapacks/:id/conversations/:cid`, `PATCH /datapacks/:id/conversation-search-opt-out` | The list is most recently active first, 20 to a page (200 at most), with `first_message`, `messages`, `memories_added`; `q` matches the title or any turn. A conversation's thread is oldest first, 100 turns to a page (500 at most). Each turn also carries `calls` (Meko's own): its decision trace, the conversation's calls since the turn before (and since the conversation was created), shaped as `/usage`'s recent calls plus `read_only` (from the tool's definition), without `conversation_create` and `conversation_add_message`, and with the conversation's calls that failed before their datapack was known; matched by time as its memories are; with its `reasoning` and `plan`, they're what the UI's decision trace shows. Each turn carries the memories it produced, by time window (below). `llm_calls`, `latency_ms` and `total_tokens` are 0, and `can_index` false. |

### Which memories a turn produced

Until extraction records which turn made which memory, a turn's memories are those saved
in its conversation (a memory's `conversation_id`) after the turn before it and no later than it,
as Meko Cloud infers them. So a memory counts for a turn only when the agent records the turn
(`conversation_add_message`) after that turn's `memory_add`s; a turn recorded before them gives
them to the next turn. Turns come from the agents' own MCP calls (AgentK's Meko client records
none), so the attribution depends on their order. Promoted ones are listed apart
(`promoted_memory_ids`, `promoted_memory_texts`).

Meko has one local user and no billing or sharing: its `/auth` routes serve that user, and it serves
no billing summary, `/runtime-config.js`, message traces or sharing routes. `/me/meko-tokens` would
come with per-client keys.
