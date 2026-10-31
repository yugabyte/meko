# Meko backend: the Meko Cloud link

Meko keeps one link to Meko Cloud, or to another Meko server, for its extensions: the MCP URL,
the web app and REST API URLs, and the API key. AgentK keeps each project's datapack there and
shares projects through it; with no link, extensions use this Meko. Meko owns the link: it's set
in one place, checked before it's saved, and read by every extension the same way
(`internal/cloudlink`).

## Decisions

| Decision | Why |
|---|---|
| **Meko owns the link, and extensions read it** | One place to link Meko Cloud, whatever extensions are installed, rather than a key in each extension's own settings. AgentK, which used to keep the key itself, hands its key to Meko once and reads Meko's from then on. |
| **The URLs go in `cloud.toml` beside `config.toml` (0600), and the key in the secret store** as `meko_cloud_key` ([install.md](install.md#secrets)) | `cloud.toml` is a file Meko writes itself, as `web.toml` is, so `config.toml` stays the user's. The key never sits in a file of its own outside the secret store, and never reaches a log or a reply. The CLI reads both without the server, so `meko token --json` works while Meko is stopped. `cloud.toml` existing means the link was set, linked (`linked = true`) or unlinked since. |
| **Set in the web UI (Settings › Meko Cloud, `/settings/meko-cloud`) or with `meko cloud_link`** | The page is for a person at a browser; `meko cloud_link` is for a computer without one (with local access, a headless server's web UI can't be reached from another computer) and for an extension handing over a key. |
| **A change is checked before it's saved**: a new server or key with an MCP `initialize` handshake carrying the key (10 s, `CheckMCP`); a web app or API URL that was typed, when it moves the API URL, by asking its `/datapacks/incoming-shares` without the key, which must answer in Meko's `{data}` or `{error: {code}}` envelope (`CheckAPI`) | A link that doesn't work isn't saved, and a mistyped API host never gets the key. No ping follows the handshake: a stateless server on the MCP Go SDK, as Meko's own is, refuses a ping that carries no protocol version. |
| **A saved key goes only where it already goes, or to Meko Cloud** (`Service.Link`): a change of the MCP, web app or API URL to another origin without the key typed again is refused, before anything is sent | Otherwise anyone who reaches the web UI could point the link at their own server and read the key off the check, or off an extension's next call. |
| **Meko Cloud's MCP URL is saved as empty**, and its web app and API URLs are worked out (`https://cloud.mekodata.ai`, plus `/api/v1`); another server's web app must be set | The same rule AgentK's `mekoWebUrl` had. A server whose web app isn't known has no datapack links or sharing until it's set, which the page says. |
| **Unlink keeps the URLs and removes the key** | Linking again takes only the key. Unlinked, the extensions use this Meko. `meko destroy` removes the key from the secret store with the client token, so none is left in the Keychain. |
| **Extensions read the link from `meko token --json`**, and are told when it changes (below) | Extensions already run it for this Meko's own MCP URL and token; one command gives them both. |
| **Meko lists the linked Meko Cloud's datapacks itself** (`GET /api/v1/datapacks`, after this Meko's, or `?location=cloud`; `Service.Datapacks`), asking the link's REST API for `GET /datapacks` with the key | The web UI and `meko-cli datapacks` show them beside this Meko's, and the key never leaves Meko. It's the REST API and key AgentK's knowledge uploads and Presenter's decks already use with Meko Cloud. A link to another server with no web app or API URL has no API to ask, so it says to set one. |
| **Any client token can list them**, unlike the link's own routes | An agent or extension may want to know which datapacks the linked account has. The reply holds names, descriptions and grants, never the key, and the client token still can't read or change the link. |
| **Only the fields Meko shows are copied** from Meko Cloud's rows | Cloud's rows also carry each datapack's database user and connection string, which no caller of a route that takes the client token should get. |
| **This Meko's own `/api/v1` routes act on the linked Meko Cloud's datapacks** ([below](#meko-clouds-datapacks-on-this-mekos-routes)): a datapack id this Meko doesn't have goes to Meko Cloud, and where there's no id `location` picks the side: a listing names both by default, and a create goes to Meko Cloud while it's linked. The server makes the call with the link's key | One API for the web UI and `meko-cli`, with Meko Cloud's paths and shapes, and the key never leaves the server. The id is enough to tell the sides apart: both are UUIDs. Meko Cloud's REST API serves what it has a route for; it has none that searches a datapack or adds a memory with an API key, so those two use its MCP tools. A saved key goes only where the link sends it: the routes take no URL, and another Meko Cloud is set with `meko cloud_link --url --api-url`. |
| **Acting on Meko Cloud takes the owner or the admin token**, not an agent's client token; listing takes any | The key acts on the whole Meko Cloud account and can delete its datapacks. An agent that should use Meko Cloud gets the key in its own config, and only through the owner's connect (`meko agent connect`, the web UI, or the admin token) or a link change ([below](#agents-follow-the-link)): an agent's client token can't set either off. |
| **The agents connected to Meko follow the link** ([below](#agents-follow-the-link)): while Meko Cloud is linked they connect to it directly, with its key, and a link change moves the ones at the old target to the new one | Every agent, AgentK and its capture then use the same Meko. Unlinking moves them back, which takes the key out of their configs. |

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
- A saved change (`PUT` or `DELETE`) moves the agents ([below](#agents-follow-the-link)), and its
  reply adds `agents: {moved: [{name, title}], left: [{name, title, url}], failed: [{name, title,
  error}]}`. `meko cloud_link` prints a line when it moved any (`Moved Claude Code and Kiro CLI to
  Meko Cloud; their sessions already running need a restart`) and one for each it couldn't
  (`Couldn't move Kiro CLI: <why>; run meko agent connect kiro`).

## Agents follow the link

The agents on this computer connect to Meko's target ([api.md](api.md#agents)): the linked Meko
Cloud, with its MCP URL and key, else this Meko. After a change is saved (a link, a new key or URL,
an unlink), `Service.Link` and `Unlink` call their `Moved` hook, still under the change's lock so
moves happen in the changes' order and before the extensions are told. It moves every installed
agent whose `meko` server is one of Meko's own (this computer's, the old link's or the new one's)
and isn't already connected to the new target, the way `meko agent connect` would. One pointing
anywhere else is left, and reported. The server's routes do it in `serve`; `meko cloud_link` with
the server stopped does it itself. Both hold the agents' lock (`run/agents.lock`), and a move has
`agents.MoveTimeout` (the lock's wait, then up to three runs of each agent's CLI), which `meko
cloud_link` waits out, with the checks and a minute to spare.

- **A failed move never undoes the link.** The agent is reported as failed, with why (its CLI's
  error, the key blanked), and shows as stale until it's connected again.
- **Unlinking moves them back** to this Meko with the client token, so no agent's config keeps
  Meko Cloud's key.
- **A Meko linked before agents followed the link** moves its agents to Meko Cloud once, at its
  first start that does (`agents_followed_link` in Meko's settings records it, linked or not), in
  the background, under the changes' lock (`Service.MoveAgents`), so it never moves them to a Meko
  Cloud being unlinked; a stop during it leaves it for the next start.
- **It edits the user's agent configs without asking**, only their `meko` server and only for
  Meko's own: that's what following the link means. Running sessions keep the old server until
  they restart.
- **`serve` may not see what the user's shell sees** (a service's thin PATH, or no
  `CLAUDE_CONFIG_DIR`): Meko also looks in each agent's usual folders and on the user's login
  shell's PATH, and a move it can't make is reported, never a reason to refuse the link.

## Meko Cloud's datapacks on this Meko's routes

`internal/rest`'s `cloud.go`, over `cloudlink.Service` (`Datapacks`, `API` for its REST API, `Tool`
for its MCP tools, each within 30 s but the listing's 10 s):

| Route | On the linked Meko Cloud |
|---|---|
| `GET /datapacks` | This Meko's rows, then its datapacks (below), each with its `location` (`location=all`, the default; only this Meko's while unlinked, and a 502 when Meko Cloud fails). `location=cloud` is only Meko Cloud's (a 409 `CLOUD_NOT_LINKED` while unlinked), and `location=local` only this Meko's, as the web UI asks. Any credential |
| `POST /datapacks {name, description?, location?}` | `POST /datapacks` while linked, unless `location` is `"local"` (the web UI's **+** sends it); replies 201 `{datapack_id, agent_id, name, location}`, nothing else of Cloud's reply. Unlinked, or with `"local"`, it's made here; `"cloud"` while unlinked is a 409 `CLOUD_NOT_LINKED`, whatever the credential. While linked, a client token must send `"local"`: without it the create acts on Meko Cloud and gets 403 |
| `DELETE /datapacks/:id` | `DELETE /datapacks/:id` (only the account's own: Cloud refuses the others) |
| `POST /datapacks/:id/knowledge-bases/upload-url {filename, size, content_hash, client?}` | The same route. The file then goes from the caller straight to the presigned link it gives, with no key; it answers 204 |
| `GET /datapacks/:id/knowledge-bases/document-status?document_key=` | The same route: `state` is `awaiting_upload`, `indexing`, `completed`, `failed`, `abandoned` or `oversized` |
| `POST /datapacks/:id/search {query, limit?, force_source?, agent_id?, conversation_id?}` | MCP `context_search` (Meko's own route: Cloud's REST API has none) |
| `POST /datapacks/:id/memories {text, agent_id?, conversation_id?}` | MCP `memory_add` (Meko's own route, likewise) |

- **Which side:** a route that takes `:id` takes `?location=` too:
  - `cloud` sends the request to Meko Cloud, a 409 `CLOUD_NOT_LINKED` while unlinked. It reaches
    Cloud's datapack even when this Meko has one of the same id, since an export keeps ids.
    `meko-cli` always sends it for a Cloud datapack.
  - `local` serves this Meko's.
  - Without it, the route serves this Meko's datapack. A UUID this Meko doesn't have goes to Meko
    Cloud for the owner or the admin token while it's linked. For a client token it stays this
    Meko's 404, so linking Meko Cloud doesn't change what a missing datapack looks like to an
    agent.
- **A browser that names no location** (local or remote access) gets this Meko's
  datapacks from a listing or a create. The web UI always names one, so such a request is a tab still
  running a bundle from before these routes listed and made Meko Cloud's. That bundle opens and makes
  only this Meko's.
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
web app isn't known). The rows are newest first.

### meko-cli

While Meko Cloud is linked, `meko-cli`'s commands work on its datapacks as on this Meko's
([api.md](api.md#cli)), through the routes above, with its admin token. `meko-cli` never reads the
key, and a local command never asks about Meko Cloud.

- **Naming a datapack:**
  - A datapack is named by `--datapack NAME|ID`, `delete_datapack`'s and `export`'s argument, and
    the prompt's `use`.
  - It's looked up among this Meko's first, then among the linked Meko Cloud's
    (`?location=cloud`), so a name both have is this Meko's.
  - `cloud:` or `local:` before it looks on that side only.
  - `cloud:<id>` is taken as it is, and the routes get `location=cloud`. The prompt passes it on the
    lines after `use`, so they don't list Meko Cloud's datapacks each time. `use` shows the name.
  - A `delete_datapack` line at the prompt goes back to the default datapack only when the datapack it
    deletes is the one in use, on the same side.
  - Two Cloud datapacks of one name ask for the id.
- **The calls:** `search` and `add_memory` post to the datapack's `search` and `memories` routes.
  `create_datapack --cloud`, `delete_datapack`, `upload` (`client: "meko-cli"`) and `upload_status`
  use the routes above. `upload` waits for `completed`, and fails on `failed`, `abandoned` or
  `oversized`.
- **Messages:** those about a Cloud datapack say so (`Using Meko Cloud datapack team-notes`,
  `Added memory … to Meko Cloud datapack team-notes`). `delete_datapack` asks first, naming Meko
  Cloud. `export` works only on this Meko's datapacks, since its route is Meko's own.

## What extensions see

`meko token --json` carries `cloud`, with the key:

```json
"cloud": {"set": true, "url": "https://mcp.mekodata.ai/mcp", "api_key": "mko_tkn_…",
          "web_url": "https://cloud.mekodata.ai", "api_url": "https://cloud.mekodata.ai/api/v1"}
```

- `set` false: the link was never set. An extension may hand Meko a key it kept itself, once,
  with `meko cloud_link` (AgentK does).
- `set` true with empty URLs and key: unlinked; use this Meko.
- `error` set (and nothing else): the link couldn't be read (a malformed `cloud.toml`, a secret
  store that refused). The rest of the reply still answers; the extension keeps the link it last
  read rather than treat it as unlinked.
- No `cloud` at all: a Meko from before the link; the extension keeps doing what it did.

After every change, once it's saved and the change's lock is released (so a slow extension holds
up neither the reply nor the next change), Meko posts `{"what": "cloud"}` to `/_meko/changed` on each running
extension's socket, with no Origin, best effort with a 5 s timeout ([web.md](web.md)); the
extension reads `meko token --json` again. A failure to reach one is logged, never the link.
