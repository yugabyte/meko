# Meko backend: the Meko Cloud link

Meko keeps one link to Meko Cloud, or to another Meko server: the MCP URL, the web app and REST API
URLs, and the API key. While it's linked, this Meko is the way to Meko Cloud for everything on this
computer: agents and extensions still connect here, `/mcp` sends each tool call to Meko Cloud unless
it names a datapack this Meko has, and `/api/v1/cloud/…` sends REST requests there, with the key, which never leaves Meko. AgentK keeps each
project's datapack there and shares projects through it; with no link, everything uses this Meko's
own datapacks. Meko owns the link: it's set in one place, checked before it's saved, and read by
every extension the same way (`internal/cloudlink`).

## Decisions

| Decision | Why |
|---|---|
| **Meko owns the link, and extensions read whether it's on** | One place to link Meko Cloud, whatever extensions are installed, rather than a key in each extension's own settings. AgentK, which used to keep the key itself, hands its key to Meko once. |
| **While linked, `/mcp` offers Meko Cloud's tools and routes each call by its datapack id** ([below](#mcp-while-linked)): a call that names a datapack this Meko has runs here, and every other call (a blank id, an id this Meko doesn't have, a tool only Meko Cloud has) goes to Meko Cloud. The listings hold both sides, and nothing tells a client which side a datapack is on | Moving the datapacks already here into Meko Cloud takes time, needs an import API Meko Cloud doesn't have yet, and may meet the account's datapack limit, so many stay here for a while; they must keep working for the agents and projects that use them. Agents see one Meko of names and ids: the id is enough to route by, since both sides use UUIDs and no datapack is made here while linked. Serving only Meko Cloud's while linked, with this Meko's datapacks out of agents' view until they're moved, was the alternative; so was a second MCP server or separate tool names for this Meko's, which would make every agent pick a side. |
| **An id on both sides is this Meko's, until it's moved** (`servedHere`) | An export keeps ids, so Meko Cloud can hold a datapack of the same id. Serving this Meko's copy loses nothing the user can't see; the move to Meko Cloud, when it's built, keeps ids too, so it must stop serving the copy here, the one predicate's to change. |
| **No datapack is made here while linked**, and a blank `datapack_id` is Meko Cloud's default: MCP's `datapack_create`, REST's `POST /datapacks`, the web UI's **+** and `meko-cli create_datapack` make it in Meko Cloud | New work goes where the agents work. A blank id meaning this Meko's default would quietly split one session across two Mekos. |
| **Extensions reach Meko Cloud through this Meko**: `meko token --json` says whether it's linked and its web app, never the key, and `/api/v1/cloud/…` passes any REST request on with the key ([below](#apiv1cloud)) | The key stays in Meko's secret store: no extension, agent or agent config holds it. A prefix of its own keeps Meko Cloud's routes apart from this Meko's same-shaped ones. It passes every path, as the owner chose: what the key reaches through it is what Meko Cloud lets the key reach (its datapack routes and a share link's redeem today, not its account's API keys), and that's Meko Cloud's to decide, routes it adds later included. Its replies lose the credentials no caller of a client-token route should get (below). |
| **The URLs go in `cloud.toml` beside `config.toml` (0600), and the key in the secret store** as `meko_cloud_key` ([install.md](install.md#secrets)) | `cloud.toml` is a file Meko writes itself, as `web.toml` is, so `config.toml` stays the user's. The key never sits in a file of its own outside the secret store, and never reaches a log or a reply. The CLI reads both without the server, so `meko token --json` works while Meko is stopped. `cloud.toml` existing means the link was set, linked (`linked = true`) or unlinked since. |
| **Set in the web UI (Settings › Meko Cloud, `/settings/meko-cloud`) or with `meko cloud_link`** | The page is for a person at a browser; `meko cloud_link` is for a computer without one (with local access, a headless server's web UI can't be reached from another computer) and for an extension handing over a key. |
| **A change is checked before it's saved**: a new server or key with an MCP `initialize` handshake carrying the key (10 s, `CheckMCP`); a web app or API URL that was typed, when it moves the API URL, by asking its `/datapacks/incoming-shares` without the key, which must answer in Meko's `{data}` or `{error: {code}}` envelope (`CheckAPI`) | A link that doesn't work isn't saved, and a mistyped API host never gets the key. No ping follows the handshake: a stateless server on the MCP Go SDK, as Meko's own is, refuses a ping that carries no protocol version. |
| **A saved key goes only where it already goes, or to Meko Cloud** (`Service.Link`): a change of the MCP, web app or API URL to another origin without the key typed again is refused, before anything is sent | Otherwise anyone who reaches the web UI could point the link at their own server and read the key off the check, or off an extension's next call. |
| **Meko Cloud's MCP URL is saved as empty**, and its web app and API URLs are worked out (`https://cloud.mekodata.ai`, plus `/api/v1`); another server's web app must be set | The same rule AgentK's `mekoWebUrl` had. A server whose web app isn't known has no datapack links or sharing until it's set, which the page says. |
| **Unlink keeps the URLs and removes the key** | Linking again takes only the key. Unlinked, `/mcp` and the extensions use this Meko's datapacks again. `meko destroy` removes the key from the secret store with the client token, so none is left in the Keychain. |
| **Extensions read the link from `meko token --json`**, and are told when it changes (below) | Extensions already run it for this Meko's own MCP URL and token; one command gives them both. |
| **The server reads the link once**, at its first use, and keeps it as changes save it | Every tool call asks whether Meko Cloud is linked, and on macOS each read of the key runs a program. A key changed behind Meko's back (in the Keychain, or the secrets file) is read at the next start; `meko cloud_link` goes through the running server. |
| **Meko lists the linked Meko Cloud's datapacks itself** (`GET /api/v1/datapacks`, with this Meko's; `Service.Datapacks`), asking the link's REST API for `GET /datapacks` with the key | The web UI shows them beside this Meko's, and the key never leaves Meko. It's the REST API and key AgentK's knowledge uploads and Presenter's decks already use with Meko Cloud. A link to another server with no web app or API URL has no API to ask, so it says to set one. |
| **Any client token can list them**, unlike the link's own routes | An agent or extension may want to know which datapacks the linked account has. The reply holds names, descriptions and grants, never the key, and the client token still can't read or change the link. |
| **Only the fields Meko shows are copied** from Meko Cloud's rows, and `/api/v1/cloud/…` takes the credentials out of every reply it passes on | Cloud's rows also carry each datapack's database user and connection string (and a create's reply an MCP key), which no caller of a route that takes the client token should get. |
| **This Meko's own `/api/v1` routes act on the linked Meko Cloud's datapacks** ([below](#meko-clouds-datapacks-on-this-mekos-routes)): a datapack id this Meko doesn't have goes to Meko Cloud, a listing holds both sides, and a create goes to Meko Cloud while it's linked. No route takes or says a location (one sent is ignored), but Context Gaps' ([api.md](api.md#context-gaps)), whose list says which side a datapack's questions are of so AgentK links the page that shows them. The server makes the call with the link's key | One API for the web UI and `meko-cli`, with Meko Cloud's paths and shapes, and the key never leaves the server. The id is enough to tell the sides apart: both are UUIDs. A client never has to know where a datapack lives; each listing row's `web_url` says where to open it. Meko Cloud's REST API serves what it has a route for; it has none that searches a datapack or adds a memory with an API key, so those two use its MCP tools. A saved key goes only where the link sends it: the routes take no URL, and another Meko Cloud is set with `meko cloud_link --url --api-url`. |
| **These routes act on Meko Cloud for the owner or the admin token**, not an agent's client token; listing takes any | They're the web UI's and `meko-cli`'s. A client reaches Meko Cloud through `/mcp` and `/api/v1/cloud/…`, which take its client token. |
| **Through `/mcp` and `/api/v1/cloud/…`, any credential may do anything on Meko Cloud**, deletes included, and `auth = "none"` too | The owner chose to link, and agents held the key itself before they stayed here, which could already do all of it. Per-client keys, which could scope it, aren't planned. |
| **Agents stay on this Meko** ([below](#agents-stay-on-this-meko)): linked or not, they connect to this Meko's `/mcp` with the client token, and a link change moves back the ones a Meko from before connected to Meko Cloud | Every agent, AgentK and its capture use the same Meko, which sends their calls to Meko Cloud, and no agent config holds the key. |
| **When Meko Cloud fails, the listings fail**: MCP's `datapack_list` and `datapack_search` and REST's `GET /datapacks` answer with its failure, not this Meko's datapacks alone, while calls that name one of this Meko's datapacks keep working | A list that silently lacks Meko Cloud's datapacks would look whole. |
| **Forwarded calls are in this Meko's call log, marked as Meko Cloud's** (`location`, [api.md](api.md#the-call-log)) | What agents did through this Meko is on record; this Meko's metrics, usage and usage stats read only its own calls, so no Meko Cloud call counts toward a datapack here, even one of the same id. Context Gaps keeps the questions of the searches it passed on, under their datapack's id ([api.md](api.md#context-gaps)), but they aren't in the usage stats either. |

## Routes and command

- `GET /api/v1/settings/meko-cloud` reads the link: `{set, linked, url, web_url, api_url,
  effective_url, effective_web_url, effective_api_url, key_hint, default_url, default_web_url}`.
  `key_hint` is the key's last four characters ("" for a key under 12, null with none), never the
  key. `PUT` takes `{url?, web_url?, api_url?, api_key?}` (a field left out is unchanged; an empty
  `api_key` keeps the saved one) and answers the same shape, or 400 `VALIDATION_ERROR` with why it
  was refused. `DELETE` unlinks.
- On the web listener they take the owner: its browsers, with local or remote access, from Meko's
  own origin. On the API listener they take only the admin token (`adminOnly`), for `meko
  cloud_link`; an agent's client token gets 403, so it can't read the link's state or redirect
  the key.
- `meko cloud_link [--url U] [--web-url W] [--api-url A]` reads the key from stdin, or asks for it
  on a terminal with echo off (Enter keeps a saved one), and links; `--off` unlinks. With a server
  running it goes through the API listener (admin token), so the running extensions are told;
  otherwise it saves the link itself, and they read it when they next ask. `meko status` shows
  the link when there is one (`✓ Meko Cloud      linked to <url> (key …abcd)`), and
  `meko status --verbose` says so either way (`Meko Cloud:    linked to …`, or not linked).
- A saved change (`PUT` or `DELETE`) moves back the agents a Meko from before connected to Meko
  Cloud ([below](#agents-stay-on-this-meko)), and its reply adds `agents: {moved: [{name, title}],
  left: [{name, title, url}], failed: [{name, title, error}]}`. `meko cloud_link` prints a line when
  it moved any (`Moved Claude Code and Kiro CLI back to Meko on this computer; their sessions
  already running need a restart`) and one for each it couldn't (`Couldn't move Kiro CLI: <why>; run
  meko agent connect kiro`).

## /mcp while linked

`internal/mcp`'s `cloud.go`, over `cloudlink.Service` (`CloudTools`, `Forward`; `remote.go`):

- **Which server:** each request's server is picked by the link's state: this Meko's tools while
  unlinked, Meko Cloud's tool list while linked, each call routed as below. A link that can't be read (a secret store that refused) counts
  as linked: a call that names one of this Meko's datapacks still runs here, and the others fail
  with `cloud_call_failed` and the reason in the log, rather than land in this Meko's default
  datapack or listings. A session already running keeps the tool list it read at
  `initialize`; a stateless server can't tell it of a change, so agents' sessions see a link
  change once restarted. `meko cloud_link` says so only for the agents it moved back.
- **The tools** are Meko Cloud's own, as its `tools/list` gives them (names, schemas,
  annotations), and so are the `initialize` instructions. They're read at start, after each link
  change, and again once 15 minutes old; a request before the first read waits for it up to 10 s.
  Until a read works, the tools this Meko shares with Meko Cloud stand in (not `datapack_metrics`,
  which reads this Meko's call log) with instructions of Meko's own.
- **A call that names a datapack this Meko has runs here** (`routed`, `byDatapack`): a tool this
  Meko has that takes `datapack_id`, whose trimmed id is one its store has (`servedHere`), runs this
  Meko's tool of that name, recorded as this Meko's call, exactly as while unlinked. Meko Cloud's
  schemas fit: this Meko's tools take Cloud's arguments and ignore ones they don't know. When the
  store can't say, the call fails with the tool's own failure code (the reason in the log), never
  sent to Meko Cloud on a guess.
- **Every other call goes to Meko Cloud as it came**, its arguments untouched, and Meko Cloud's
  result comes back as it is, its `{"error", "detail"}` replies included: a blank `datapack_id`
  (Meko Cloud's default datapack), an id this Meko doesn't have, `datapack_create`, and a tool only
  Meko Cloud has (`amp_*`), whatever datapack it names.
- **The listings add this Meko's datapacks** to Meko Cloud's reply, in the same row shape, one row
  per id (Meko Cloud's row of an id this Meko has is left out), with no word of the side:
  - `datapack_list`: Meko Cloud's rows, then this Meko's matching `query`, newest first, the whole
    cut to `limit`.
  - `datapack_search`: on the first page (no `cursor`), this Meko's matches (`query`, `name`,
    `sort`; none for `grant: "shared"`) come first, every one of them, so the page can be longer
    than `limit`. A page asked for with a cursor adds none: the cursor is Meko Cloud's.
    `meko-cli` finds a datapack by name this way.
  - Every page leaves out Meko Cloud's row of an id this Meko lists on it; one whose copy here
    doesn't match the query keeps Meko Cloud's row. A `total` Meko Cloud gave counts this Meko's
    matches too, less the rows that page left out (an id on both sides on another page is counted
    twice); a null `total` stays null.
  - This Meko's rows carry an empty `user_id`: the account's id is Meko Cloud's, and AgentK takes it
    from an owner row's `user_id`.
  - An error reply or failure from Meko Cloud passes as it is: no partial list.
- **`knowledgebase_upload_file`** stays this Meko's, since only it can read a path on this
  computer: it checks and reads the file as for this Meko's datapacks (the client or admin token
  only), asks Meko Cloud's `knowledgebase_upload_document` for an upload link, posts the file there,
  and answers with Meko Cloud's `knowledgebase_document_status` reply plus `already_uploaded`.
  Meko Cloud's own tool of that name, if it had one, isn't offered.
- **One session is kept** with Meko Cloud's MCP server, with the key, and shared by every call.
  It's dropped only when it's gone (Meko Cloud says it lacks it, or its connection closed), never for
  one call's timeout or cancel, which would fail the others on it. A call is made once more only when
  Meko Cloud said it lacks the session, so it never ran; any other failure is reported, since the call
  may have run. A link change drops the session and the tool list at once, and a call or a list read
  that started under the old link can't connect it again or keep its result (each carries the link's
  generation). A call takes at most 2 minutes. The REST routes' search and memories for a Meko Cloud
  datapack use the same session.
- **A call that doesn't reach Meko Cloud** replies `cloud_unreachable`, `cloud_key_refused`,
  `cloud_not_linked` or `cloud_call_failed`, with what to do (`Link it again in Settings › Meko
  Cloud, or with meko cloud_link`). Nothing falls back to this Meko's datapacks.
- **The call log** gets a row for each call sent to Meko Cloud, `location = 'cloud'`, with its tool, datapack, agent,
  conversation, credential, outcome (Meko Cloud's error code, when it replied with one) and time;
  no counts or steps, since Meko Cloud's reply isn't read.

## /api/v1/cloud

`/api/v1/cloud/<path>` (`internal/rest`'s `passToCloud`, over `cloudlink.Service.Pass`) sends the
request to the link's REST API at `<path>`, with its method, query, body and content type, and the
key as a Bearer token, within 30 s, and answers with Meko Cloud's status, content type and body; no
other header is passed either way. It takes any credential the data routes take: it's how extensions
(AgentK's sharing, invites and uploads, Presenter's decks) reach Meko Cloud's REST API without its
key. It passes every path, so what it reaches is what Meko Cloud accepts the key on.

- **Credentials never pass:** a JSON reply loses, at any depth, every field whose name holds
  `password`, `conn_str`, `connection_string`, `ysql`, `db_access`, `db_user` or `api_key`: a
  datapack's database user, password and connection string, and an MCP server's key. Numbers and the
  rest keep their values; a reply that isn't JSON passes as it came.
- **Failures:** unlinked it's a 409 `CLOUD_NOT_LINKED`; a 401 from Meko Cloud is a 502
  `CLOUD_KEY_REFUSED` (the key is Meko's, not the caller's); a link with no API URL is a 502
  `CLOUD_NO_API_URL`, one that doesn't reach Meko Cloud a 502 `CLOUD_UNREACHABLE`, and a reply over
  8 MiB a 502 `CLOUD_CALL_FAILED` rather than a body cut short, each in Meko's `{data, error: {code,
  message}}` envelope.

## Agents stay on this Meko

The agents on this computer connect to this Meko's MCP URL with the client token, linked or not
([api.md](api.md#agents)). Before agents stayed here, a link moved them to Meko Cloud with its key; a
link change now moves those back. After a change is saved (a link, a new key or URL, an unlink),
`Service.Link` and `Unlink` call their `Moved` hook, still under the change's lock so moves happen
in the changes' order and before the extensions are told. It moves back every installed agent whose
`meko` server is at the link before the change with that link's key (or on this computer, at an old
token), the way `meko agent connect` would. One at Meko Cloud with a key of its own, or anywhere
else, is the user's: it's left, and reported. The server's routes do it in `serve`; `meko
cloud_link` with the server stopped does it itself. Both hold the agents' lock (`run/agents.lock`),
and a move has `agents.MoveTimeout` (the lock's wait, then up to three runs of each agent's CLI),
which `meko cloud_link` waits out, with the checks and a minute to spare.

- **A failed move never undoes the link.** The agent is reported as failed, with why (its CLI's
  error, the key blanked), and shows as elsewhere until it's connected again.
- **A Meko linked while agents followed the link** moves them back once, at its first start where
  they stay here (`agents_on_this_meko` in Meko's settings records it, linked or not), in the
  background, under the changes' lock (`Service.MoveAgents`); a stop during it leaves it for the
  next start.
- **It edits the user's agent configs without asking**, only their `meko` server and only one Meko
  put there. Running sessions keep the old server until they restart.
- **`serve` may not see what the user's shell sees** (a service's thin PATH, or no
  `CLAUDE_CONFIG_DIR`): Meko also looks in each agent's usual folders and on the user's login
  shell's PATH, and a move it can't make is reported, never a reason to refuse the link.

## Meko Cloud's datapacks on this Meko's routes

The web UI's and `meko-cli`'s ways to Meko Cloud on this Meko's own routes (a client's are `/mcp` and
[`/api/v1/cloud`](#apiv1cloud)): `internal/rest`'s `cloud.go`, over `cloudlink.Service`
(`Datapacks`, `API` for its REST API, `Tool` for its MCP tools on the kept session, each within 30 s
but the listing's 10 s):

| Route | On the linked Meko Cloud |
|---|---|
| `GET /datapacks` | This Meko's rows and its datapacks (below) as one list, sorted by name (ignoring case), one row per id: Meko Cloud's row of an id this Meko has is left out. Each row's `web_url` opens it: `/datapacks/<id>`, this web UI's page, for this Meko's; Meko Cloud's web app for its own. Only this Meko's while unlinked, and a 502 when Meko Cloud fails. Any credential |
| `POST /datapacks {name, description?}` | `POST /datapacks` while linked; replies 201 `{datapack_id, agent_id, name, web_url}`, nothing else of Cloud's reply (`web_url`, as a listing row's, is Meko Cloud's web app, and `/datapacks/<id>` for one made here), so the web UI opens a new datapack where the server made it. Unlinked it's made here. A client token's create while linked acts on Meko Cloud and gets 403: it creates through `/mcp` or `/api/v1/cloud` |
| `DELETE /datapacks/:id` | `DELETE /datapacks/:id` (only the account's own: Cloud refuses the others) |
| `POST /datapacks/:id/knowledge-bases/upload-url {filename, size, content_hash, client?}` | The same route. The file then goes from the caller straight to the presigned link it gives, with no key; it answers 204 |
| `GET /datapacks/:id/knowledge-bases/document-status?document_key=` | The same route: `state` is `awaiting_upload`, `indexing`, `completed`, `failed`, `abandoned` or `oversized` |
| `POST /datapacks/:id/search {query, limit?, force_source?, agent_id?, conversation_id?}` | MCP `context_search` (Meko's own route: Cloud's REST API has none) |
| `POST /datapacks/:id/memories {text, agent_id?, conversation_id?}` | MCP `memory_add` (Meko's own route, likewise) |

- **Which side:** the id decides (`inCloud`). A route that takes `:id` serves this Meko's datapack,
  even when Meko Cloud has one of the same id. A UUID this Meko doesn't have goes to Meko Cloud for
  the owner or the admin token while it's linked. For a client token it stays this Meko's 404, so
  linking Meko Cloud doesn't change what a missing datapack looks like to an agent.
- **No location:** no route takes one or says one, but Context Gaps' ([api.md](api.md#context-gaps)). A `?location=` or a body's `location`, which an
  older `meko-cli` or web UI tab sends, is ignored.
- **Search and memories on this Meko's datapacks:** they run the tools in-process as `/mcp` does
  (`mcp.Call`), recorded in the call log the same way. Their reply is the tool's.
- **A tool's error reply** (`{"error", "detail"}`) maps by its code, on either side:
  - a missing datapack (`datapack_access_denied`, `not_found`) is a 404 `NOT_FOUND`;
  - the tool's own failure (a code ending in `_failed`) is a 500, or a 502 `CLOUD_CALL_FAILED`
    from Meko Cloud;
  - `memory_engine_unavailable` is a 503;
  - the rest are 400s with the tool's code.
- **The arguments sent to Meko Cloud's tools:** only those listed, since Meko Cloud refuses an
  argument it doesn't know. `conversation_id` defaults to a fixed id derived from `meko-rest`, since
  Cloud's tools require one.
- **Credential:**
  - Acting on Meko Cloud takes the admin token (`meko-cli`) or the owner on the web listener; an
    agent's client token gets 403.
  - The listing takes the credential every data route does: the client token too, or nothing with
    `[server] auth = "none"`.
- **Failures:** they keep the reply off 401, which the web UI reads as Meko refusing the browser.
  - Meko Cloud's own 4xx keeps its status, code and message (`NOT_FOUND`, a 409 `CONFLICT`, a 403).
  - The rest are 502s with a code:
    - `CLOUD_NO_API_URL`: the link has no API URL;
    - `CLOUD_UNREACHABLE`: Meko Cloud couldn't be reached, or its whole reply didn't arrive within
      the time limit (a refused connection, DNS, TLS, a timeout, a reply cut short);
    - `CLOUD_KEY_REFUSED`: a 401, or a 403 on the listing;
    - `CLOUD_LIST_FAILED`: any other error on the listing, with Meko Cloud's message;
    - `CLOUD_CALL_FAILED`: any other error on the other routes, with Meko Cloud's message.
  - A link that can't be read is the usual 500.

### The listing

`Service.Datapacks` asks the link's REST API (`effective_api_url`) for `GET /datapacks` with the key
as a Bearer token, within 10 s. It sends no query: Meko Cloud answers that with every datapack the
account can see, owned and shared, in one list (a `limit` would switch it to pages).

Each row keeps:
- `datapack_id`, `datapack_name` and `description`;
- `grant`: `owner` for the account's own, while any other (such as `maintainer`, `contributor` or
  `viewer`) means it was shared with it;
- `created_at` and `updated_at`.

It gains `web_url`, the datapack in the link's web app (`<web_url>/datapacks/<id>`, null when the
web app isn't known). `GET /datapacks` sorts them by name with this Meko's.

### meko-cli

`meko-cli`'s tool commands are MCP calls to this Meko's `/mcp` with the admin token
([api.md](api.md#cli)), so while Meko Cloud is linked they work on its datapacks and this Meko's, as
an agent's do: `datapacks` lists both, a name is looked up among both (`datapack_search`'s first
page), `create_datapack` makes one in Meko Cloud, and `upload` goes to the knowledge base of the
datapack's side. Their messages never say which side a datapack is on (`Using datapack team-notes`). `meko-cli` never
reads the key. `export` works only on this Meko's datapacks, linked or not, since its route is
Meko's own: it passes the name or id to the route, which looks it up among this Meko's alone, so an
export never asks Meko Cloud and works while it's down or refuses the key. One this Meko doesn't
have is refused (`"team-notes" isn't one of this computer's datapacks: only those can be
exported`). While linked it takes no default (`/mcp`'s is Meko Cloud's), so the datapack must be
named; it asks the link's state (`GET /api/v1/settings/meko-cloud`) only for that, and for the
note `delete_datapack` adds about the default datapack, which it leaves out while linked.

## What extensions see

`meko token --json` carries `cloud`: whether this Meko is linked, its web app and which account,
never the key.

```json
"cloud": {"set": true, "linked": true, "web_url": "https://cloud.mekodata.ai", "account": "3f1c09a4be7d5e21"}
```

- `account` names the linked account without giving its key away: the first 16 hex digits of the
  SHA-256 of the MCP URL and the key. A new key or server is a new account, which an extension
  tells apart even when nothing it connects to changes (AgentK forgets the old account's invites).

- `linked` true: this Meko's `/mcp` (the reply's `url` and `token`) works on Meko Cloud's datapacks
  and the ones already on this Meko,
  and `/api/v1/cloud/…` on this Meko's origin reaches Meko Cloud's REST API; link to a datapack at
  `<web_url>/datapacks/<id>` (`web_url` is "" when the web app isn't known). Both need this Meko's
  server running.
- `set` false: the link was never set. An extension may hand Meko a key it kept itself, once,
  with `meko cloud_link` (AgentK does).
- `set` true and `linked` false: unlinked; use this Meko's datapacks.
- `error` set (and nothing else): the link couldn't be read (a malformed `cloud.toml`, a secret
  store that refused). The rest of the reply still answers; the extension keeps the link it last
  read rather than treat it as unlinked.
- No `cloud` at all: a Meko from before the link; the extension keeps doing what it did. A Meko from
  before the key stayed in Meko sends `url`, `api_key` and `api_url` too, which an extension may still
  use.

After every change, once it's saved and the change's lock is released (so a slow extension holds
up neither the reply nor the next change), Meko posts `{"what": "cloud"}` to `/_meko/changed` on each running
extension's socket, with no Origin, best effort with a 5 s timeout ([web.md](web.md)); the
extension reads `meko token --json` again. A failure to reach one is logged, never the link.
