# Meko backend: web UI

Milestone 6 (#172). A light UI for one person on their own machine: browse, edit and search a
datapack (all sources or one), add memories and keep, promote or delete several at once, upload and
manage knowledge files, download artifacts, read conversations, see its analytics, and copy the
commands that connect an agent. It calls the REST routes and MCP tools in [api.md](api.md); the one
route it added is `/usage`.

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
| **Search shows only memories at 0.40 cosine or above** (rounded to the two places shown), and every hit's cosine similarity | `context_search` applies no floor to memories, as Cloud's doesn't: an agent weighs them beside the other sources, and a floor costs recall on names and IDs. A person reading the list needs only the matches. With Meko's embedder, right answers measured 0.40 to 0.72 and unrelated memories 0.11 to 0.29; a 0.65 floor would drop most right answers. |
| **Analytics is Cloud's dashboard on the same `/metrics` payload and datapack counts**, plus a Usage section from `/usage` | The cards read what Cloud's read, so the two can't drift; Refresh reloads all three. Token estimates come from bytes (about 4 a token), since Meko's chunks are 1000 characters, not Cloud's 1024 tokens. Usage (per-tool latency and errors, agents, the latest calls) is Meko's own: the call log already holds it. |
| **Analytics picks 7, 30 or 90 days** (`?days=` on `/metrics` and `/usage`); a recent call opens its steps' timings; **Status** reads `/api/v1/server` | Without `days`, `/metrics` keeps Cloud's windows for Cloud-shaped clients. `/api/v1/status` stays the CLI's (admin token); `/api/v1/server` gives the same reply to the client token, since it describes nothing but the user's own install. |
| **Invite opens a banner pointing to Meko Cloud** (`cloud.mekodata.ai`) | Sharing needs the datapack in Cloud, so Meko local has none (#172 drops sharing and members); the button says where it lives instead of leaving the user to look for it. |
| **Status is the sidebar's first link**, with a dot for the memory engine's state (polled every 30 s); pages render inside an error boundary | It's where someone looks when something seems off. A page that throws (as against a server older than the UI, between a rebuild and a restart) says so and suggests `meko restart` instead of going blank. |
| **Each conversation turn has a decision trace**: what the agent looked up, its reasoning and plan, and what it saved, each call opening its step timings | Cloud builds its trace from Langfuse, which Meko local doesn't have. The call log already records every call's conversation, result counts and steps, and the turn stores the reasoning and plan the agent sent, so no new data is collected. Lookups and actions are grouped by each tool's own read-only flag. The reasoning and plan are only as good as what the agent sends. The calls go back to the conversation's start (an earlier conversation under the same id doesn't leak in) and include calls that failed before their datapack was known; the call log keeps 90 days, so an older turn says its calls may be gone. |
| **Overview leads with the collective-learning ribbon** (conversations → memories → learnings → collective memory, each opening its tab); **Knowledge** ends with its activity log (uploads with their chunks, promotions); collective memories can be deleted | As Cloud's datapack page shows them, from routes Meko already serves (the datapack's counts, `activity-log`, `DELETE /promoted-memories/:id`). |
| **A new datapack's Overview starts with "Get started"** (connect an agent, add a memory, upload a file, run a search), each ticked from existing counts and gone once all are done; **`/` jumps to Search** from anywhere outside a field, and Esc closes dialogs | Cloud's `DatapackOnboardingStepper` shows the same first steps. The shortcut is the one place a keyboard user wants to reach fastest. |
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
