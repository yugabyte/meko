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
| **No login: `meko ui` opens `http://127.0.0.1:<port>/#token=<client token>`**, and the page keeps the token in `localStorage` | A fragment never reaches the server or its access log. Only someone who can read the data folder gets the token, as before, so another user on a shared host can't open the UI. A page without a working token shows how to open it with `meko ui`. With `[server] auth = "none"` it needs no token. |
| **The server takes its own origin from a browser, and only it** (`http://127.0.0.1:<port>`, `http://localhost:<port>`), and sends no CORS headers | Another site's page can't call the API or read its replies. The Host check still stops DNS rebinding. |
| **The UI's files need no token**; `/api` and `/mcp` paths never fall through to it | It's static and holds nothing secret. An unknown API route stays a 401 without a token and a 404 with one. |
| **Frames refused, `Content-Security-Policy` of `self`**, assets cached for good (their names are hashed) and `index.html` never | Another site can't frame the page that holds the token, and a rebuilt UI loads at once. |
| **Search goes through `context_search` on `/mcp`** | There's no REST search route; the UI shows what an agent would get. Its calls are recorded as agent `meko_web_ui`. |
| **Uploads use Cloud's presigned flow** (`upload-url`, then the multipart POST), hashing the file in the browser | The same path AgentK and meko_ui take, so nothing new is trusted. |

## Develop

```sh
cd backend
make web_ui                      # npm ci when the lockfile changes, then web_ui/dist
./bin/meko start && ./bin/meko ui
cd web_ui && npm run dev         # Vite on :5178, proxying the API to MEKO_URL (default :7778)
```

With `npm run dev`, open `http://localhost:5178/#token=<meko token>`; the proxy sends Meko its
own origin.
