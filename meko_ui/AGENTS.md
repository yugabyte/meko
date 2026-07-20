# AGENTS.md — meko_ui (web app)

React 19 + Vite + TypeScript + Tailwind v4 + shadcn/ui + TanStack Query, with a
Node/Express server (`server/`) that proxies to `api_server`. Follow the
conventions already in the tree — match the surrounding code over any generic
habit.

## Folder structure

```
src/
  features/<feature>/            # one folder per product area (datapacks, auth, billing, onboarding, workbench-lm, …)
    components/                  # feature-scoped components (may nest, e.g. datapack-detail/, datapack-detail/dialogs/)
    pages/                       # route-level screens
    hooks/                       # feature-scoped hooks (use*)
  components/ui/                 # shadcn primitives — shared, presentation-only
  lib/
    api/                         # HTTP client layer: mekoApi.ts (core) + one module per domain (ampApi.ts, …)
    <util>.ts                    # cross-cutting utils (runtimeEnv, utils/cn, telemetry, …)
  hooks/                         # app-wide hooks (useCurrentUser, use-toast, …)
  app/routes/paths.ts            # route path builders — never hardcode route strings in components
server/                          # Node/Express server
  index.ts                       # Express app entry
  routes.ts                      # API-proxy routes to api_server
  static.ts / vite.ts            # static asset serving / vite dev middleware
  storage.ts                     # server-side storage helper
```

Put new UI in the owning `features/<feature>/`. Reach for `components/ui/*`
before hand-rolling a control; add a genuinely shared primitive there (e.g. the
searchable `Combobox`) and let feature code wrap it (see `UserScopeSelect` in
meko_admin, the Fleet pool filter).

## Data fetching & the API layer

- **All HTTP goes through `src/lib/api/`** — never `fetch`/`axios` from a
  component. Add a per-domain module (like `ampApi.ts`) that calls the shared
  `request<T>(path, method, body?, { signal })` from `mekoApi.ts` (it unwraps the
  `{data,error}` envelope and normalizes errors). Surface errors with
  `getApiErrorMessage(e, fallback)`.
- Use **route-builder helpers** inside the module (e.g. `clustersRoute(id)`),
  `encodeURIComponent` path params, and accept a `{ signal }: RouteOptions` for
  cancelable reads.
- **Server state = TanStack Query.** `useQuery`/`useMutation` with a stable
  `queryKey`, a sensible `staleTime`, and `queryClient.invalidateQueries({ queryKey })`
  on mutation success. Keep query keys consistent across the app so invalidation
  hits. Don't keep server data in `useState`.

## Components & styling

- Start most `.tsx` files with the **2-line purpose comment** already common in
  the tree (what the file is / does).
- Keep components **presentational + reusable**; lift shared bits into
  `components/ui` or a small local helper rather than duplicating.
- **Centralize repeated string literals** — any string used in 2+ files becomes
  an exported const, not an inline duplicate.
- **Styling is Tailwind + the CSS-variable token system in `src/index.css`.** Use
  semantic tokens (`bg-card`, `text-muted-foreground`, `border-border`,
  `bg-primary`, `bg-input`) — never raw hex; both light and `.dark` must work.
  Merge classes with `cn()`. Custom brand SVG icons live in `public/` as plain
  `.svg` and are referenced via `<img src="/name.svg">`.
- Feature flags come from **`runtimeEnv`** (e.g. `SHOW_PG_AMP`), not hardcoded.

## Naming

- Components + their files: `PascalCase` (`CreateClusterDialog.tsx`). Hooks:
  `useX`. Utils/vars/functions: `camelCase`. Types: `PascalCase`.
- Import via the `@/` alias (`@/lib/api/...`, `@/components/ui/...`) — no deep
  relative `../../..`.

## Checks

`npm run check` (tsc) must pass. `npm run dev` for the full server;
`npm run dev:client` for vite-only on :3010. Verify visible changes on your own
running server (no in-editor browser preview).
