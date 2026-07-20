# AGENTS.md — meko_admin (operator console)

React 19 + Vite + Tailwind v4 + shadcn/ui + TanStack Query SPA, plus an
**Express BFF** (`server/`) that proxies to `api_server`. Same front-end
conventions as `meko_ui` (see `meko_ui/AGENTS.md`) — this file covers what's
different.

## Folder structure

```
src/
  features/<area>/pages|components/   # dataplane, users, metrics, exec, amp, auth, layout
  components/ui/                      # shadcn primitives (kept byte-aligned with amp_ui)
  lib/
    adminApi.ts                       # client → the Express BFF (/api/admin/*)
    authContext.tsx                   # Cognito-admin auth + adminHttp
    queryClient.ts
  app/routes/paths.ts                 # ROUTES + route wiring
server/
  index.ts                            # Express app; mounts routers under /api/admin/*
  apiServerClient.ts                  # apiServerGet / apiServerSend → api_server admin API (X-Meko-Admin-Secret)
  routes/                             # thin proxy routers (usage, metrics, ampRegistration, auth)
  cognito.ts, cloudwatch.ts, db.ts    # external integrations
```

## The BFF rule (important)

**Prefer proxying to `api_server` over running SQL in `server/`.** api_server
owns the DBs, migrations, and cross-store queries; meko_admin's own `server/db.ts`
pool is a drift hazard — pointing it at a different database than api_server is
exactly what caused the `amp_registration` "relation does not exist" bug. New
admin data/business logic → add an `api_server` `/admin/*` endpoint and proxy it
via `apiServerClient` (`apiServerGet`/`apiServerSend`, `X-Meko-Admin-Secret`);
don't reach for the local pg pool.

- Client (`adminApi.ts`) calls the BFF (`/api/admin/*`); the BFF forwards to
  api_server. Keep request/response shapes matching so the client is a passthrough.

## Auth & data

- Cognito admin auth in `authContext.tsx`. **`queryClient.clear()` on both login
  and logout** — query keys aren't per-user, so skipping this leaks the previous
  admin's cached data to the next.
- Same TanStack Query rules as meko_ui (stable keys, `invalidateQueries` on
  mutations).

## Styling

`src/index.css` token system is intentionally **byte-aligned with amp_ui**
(neutral near-black dark palette, orange `--primary`/`--brand`). Keep dark text
(`--*-foreground` = charcoal, not white) on the orange fills for WCAG contrast.
Use semantic tokens; confine palette changes to `index.css` + `components/ui/*`.

## Checks

`npm run check` (tsc). Verify on your own running server (no in-editor preview).
