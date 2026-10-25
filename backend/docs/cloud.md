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
| **A saved key goes only where it already goes, or to Meko Cloud** (`Service.Link`): a change of the MCP, web app or API URL to another origin without the key typed again is refused, before anything is sent | Otherwise anyone with the browser's login could point the link at their own server and read the key off the check, or off an extension's next call. |
| **Meko Cloud's MCP URL is saved as empty**, and its web app and API URLs are worked out (`https://cloud.mekodata.ai`, plus `/api/v1`); another server's web app must be set | The same rule AgentK's `mekoWebUrl` had. A server whose web app isn't known has no datapack links or sharing until it's set, which the page says. |
| **Unlink keeps the URLs and removes the key** | Linking again takes only the key. Unlinked, the extensions use this Meko. |
| **Extensions read the link from `meko token --json`**, and are told when it changes (below) | Extensions already run it for this Meko's own MCP URL and token; one command gives them both. |

## Routes and command

- `GET /api/v1/settings/meko-cloud` reads the link: `{set, linked, url, web_url, api_url,
  effective_url, effective_web_url, effective_api_url, key_hint, default_url, default_web_url}`.
  `key_hint` is the key's last four characters ("" for a key under 12, null with none), never the
  key. `PUT` takes `{url?, web_url?, api_url?, api_key?}` (a field left out is unchanged; an empty
  `api_key` keeps the saved one) and answers the same shape, or 400 `VALIDATION_ERROR` with why it
  was refused. `DELETE` unlinks.
- On the web listener they take the owner: local access or the session cookie, from Meko's own
  origin. On the API listener they take only the admin token (`adminOnly`), for `meko
  cloud_link`; an agent's client token gets 403, so it can't read the link's state or redirect
  the key.
- `meko cloud_link [--url U] [--web-url W] [--api-url A]` reads the key from stdin, or asks for it
  on a terminal with echo off (Enter keeps a saved one), and links; `--off` unlinks. With a server
  running it goes through the API listener (admin token), so the running extensions are told;
  otherwise it saves the link itself, and they read it when they next ask. `meko status` shows
  the link (`Meko Cloud: linked to <url> (key …abcd)`, or not linked).

## What extensions see

`meko token --json` carries `cloud`, with the key:

```json
"cloud": {"set": true, "url": "https://mcp.mekodata.ai/mcp", "api_key": "mko_tkn_…",
          "web_url": "https://cloud.mekodata.ai", "api_url": "https://cloud.mekodata.ai/api/v1"}
```

- `set` false: the link was never set. An extension may hand Meko a key it kept itself, once,
  with `meko cloud_link` (AgentK does).
- `set` true with empty URLs and key: unlinked; use this Meko.
- No `cloud` at all: a Meko from before the link; the extension keeps doing what it did.

After every change, Meko posts `{"what": "cloud"}` to `/_meko/changed` on each running
extension's socket, with no Origin, best effort with a 5 s timeout ([web.md](web.md)); the
extension reads `meko token --json` again. A failure to reach one is logged, never the link.
