# Meko backend: web UI

Milestone 6 (#172). A light UI for one person on their own machine: browse and search a datapack,
add, keep, promote and delete memories, upload and manage knowledge files, read conversations, and
copy the commands that connect an agent. It calls the REST routes and MCP tools in
[api.md](api.md) and adds no routes of its own.

## Decisions

| Decision | Why |
|---|---|
| **Its own small app in `backend/web_ui`**, not meko_ui: React, Vite, Tailwind and lucide-react, with Meko Cloud's colors | meko_ui is Cloud's private app, and most of it (login, billing, sharing, onboarding) has nothing to do here. This one is about 80 KB gzipped, with three runtime dependencies. |
| **Served from `web_ui/dist` on disk**, found as the memory engine is: `[web_ui] dir` (or `MEKO_WEB_UI_DIR`), else `<bin>/../web_ui/dist` | `go build` and `go test` don't need Node. Without a built UI, `/` says how to build one and the API is unchanged. |
| **No login: `meko ui` asks the server for a one-time code (admin token) and opens `http://127.0.0.1:<port>/#code=<code>`**; the page swaps it at `/api/v1/ui-code/redeem` for the client token and keeps that in `localStorage` | The link goes to `open` or `xdg-open` as an argument, which other local users can read in `ps`, so it carries a code that works once and for 60 seconds, not the token. A fragment never reaches the server's access log. A page without a working token shows how to open it with `meko ui`. With `[server] auth = "none"` it needs no token. |
| **The server takes its own origin from a browser, and only it** (`http://127.0.0.1:<port>`, `http://localhost:<port>`), and sends no CORS headers | Another site's page can't call the API or read its replies. The Host check still stops DNS rebinding. |
| **The UI's files need no token**; catch-all `/api/*` and `/mcp/*` routes keep API paths off the UI's route | It's static and holds nothing secret. The credential check keys on the matched route alone, and an unknown API route stays a 401 without a token and a 404 with one. |
| **Frames refused, `Content-Security-Policy` of `self`**, assets cached for good (their names are hashed) and `index.html` never | Another site can't frame the page that holds the token, and a rebuilt UI loads at once. |
| **Search goes through `context_search` on `/mcp`** | There's no REST search route; the UI shows what an agent would get. Its calls are recorded as agent `meko_web_ui`, which the agent counts leave out. |
| **Search folds memories below 0.35 cosine away as weak matches**, and shows every hit's cosine similarity | `context_search` applies no floor to memories, as Cloud's doesn't: an agent weighs them beside the other sources, and a floor costs recall on names and IDs. A person reading the list needs the noise out of the way. With Meko's embedder, right answers measured 0.40 to 0.72 and unrelated memories 0.11 to 0.29; a 0.65 floor would drop most right answers. |
| **Analytics is Cloud's dashboard on the same `/metrics` payload**, plus a Usage section from `/usage` | The cards read what Cloud's read, so the two can't drift. Token estimates come from bytes (about 4 a token), since Meko's chunks are 1000 characters, not Cloud's 1024 tokens. Usage (per-tool latency and errors, agents, the latest calls) is Meko's own: the call log already holds it. |
| **Invite is shown and disabled** | Sharing needs the datapack in Meko Cloud; until it can sync there (#173), everything stays on this machine. |
| **Uploads use Cloud's presigned flow** (`upload-url`, then the multipart POST), hashing the file in the browser | The same path AgentK and meko_ui take, so nothing new is trusted. |

## Develop

```sh
cd backend
make web_ui                      # npm ci when the lockfile changes, then web_ui/dist
./bin/meko start && ./bin/meko ui
cd web_ui && npm run dev         # Vite on :5178, proxying the API to MEKO_URL (default :7778)
```

With `npm run dev`, open the link `meko ui --no-open` prints with its port changed to 5178; the
proxy sends Meko its own Host and origin.
