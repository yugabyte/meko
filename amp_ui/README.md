# AMP UI

React 19 + Vite + Tailwind v4 SPA. The workspace pages (Shared pools, Fleet,
Multitenancy) talk to the Meko Controller REST API; sign in / sign up are still
a client-side stub that accept any input.

## Run locally

Requires **Node 22** (`.nvmrc` provided).

```bash
nvm use            # or: nvm install 22
npm install
npm run dev        # http://localhost:3020
```

Sign in with any email + password → lands on **Welcome to AMP**.

## Other commands

```bash
npm run build      # production build → dist/
npm run preview    # serve the production build
npm run check      # TypeScript typecheck
```

## Configuration

The workspace pages call the Meko Controller REST API. The base URL comes from
`VITE_API_BASE_URL` and defaults to the relative `/api/v1`. In dev, Vite proxies
`/api/v1` to `http://localhost:8080` (override with `VITE_API_PROXY_TARGET`), so
there's no CORS to configure. For a deployed build, set `VITE_API_BASE_URL` to
the controller's origin + `/api/v1`.

## Project structure

```
src/
├── main.tsx              App entry — applies the saved theme, then mounts <App>
├── App.tsx               Providers (react-query, auth, tooltip) + routing
├── index.css             Tailwind import + light/dark theme tokens
│
├── auth/                 Client-side auth (stub): AuthContext + AuthShell
│
├── components/
│   ├── layout/           App shell — Sidebar, Header, AppLayout, the agent
│   │                     side panel, and navConfig (single source for the
│   │                     nav, platform-model legend & agent definitions)
│   ├── data/             Shared page chrome — PageHeader / loading / error /
│   │                     container used by the data-backed pages
│   └── ui/               shadcn + Radix primitives (Button, Dialog, Select,
│                         Table, sonner toaster, …)
│
├── hooks/                Reusable hooks (e.g. useEndpoints — branch endpoints)
│
├── lib/
│   ├── api/              Controller REST API integration:
│   │                       client.ts    — fetch wrapper + base URL
│   │                       types.ts     — domain types (User, Pool, Project, …)
│   │                       resources.ts — one function per endpoint
│   │                       queries.ts   — react-query hooks, keys, identity bootstrap
│   ├── theme.ts          Light/dark theme toggle + persistence
│   ├── format.ts         Date / id formatting helpers
│   └── utils.ts          cn() Tailwind class-merge helper
│
└── pages/                Route pages — Welcome (Home), SharedPools, Fleet,
                          Multitenancy, SignIn, SignUp, Placeholder
```

Routes are generated from `components/layout/navConfig.tsx`, so adding a nav
item there wires up both the sidebar and the route table.

