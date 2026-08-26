# amp_ui — agent guide

React 19 + Vite + TypeScript + Tailwind SPA for the AMP control plane. Read this
before adding or changing UI here. (See also `amp_ui/README.md` for run/build.)

## Component structure — keep pages thin, extract components

Pages under `src/pages/` must be **thin compositions**, not large files that
define many components inline. When a page grows beyond its own top-level JSX,
extract its sub-pieces into their own files so they're reusable and individually
testable.

- **One component per file.** A page may keep a tiny, single-use helper inline,
  but anything with its own state / props / meaningful markup gets its own file.
- Put page-specific components in a folder next to the page —
  `src/pages/<page>/components/` — and promote genuinely shared ones to
  `src/components/`.
- **Compose from the shared primitives in `src/components/ui/`** (Button, Input,
  Label, Select/dropdown, Dialog / AlertDialog, Badge, Tooltip, …). Don't
  hand-roll raw `<button>` / `<input>` / `<select>` or ad-hoc styles — consistency,
  theming, and accessibility come from the shared components.
- ❌ **Don't add new monolithic multi-component pages.** A page that defines many
  components inline is hard to reuse and review — split it.

## Directory structure

```
src/
├── auth/                Client-side auth context + guarded app shell
├── components/
│   ├── ui/              Shared shadcn/Radix primitives (Button, Input, Dialog,
│   │                    AlertDialog, Select, Badge, Tooltip, …) — REUSE these
│   ├── layout/          App chrome (sidebar, header, nav) shared across pages
│   ├── data/            Shared data-display components used by multiple pages
│   └── *.tsx            Cross-page shared components (e.g. ResourceTag,
│                        ThemeToggle)
├── hooks/               Reusable React hooks (e.g. useEndpoints, use-toast)
├── lib/
│   ├── api/
│   │   ├── client.ts    fetch wrapper + base URL
│   │   ├── types.ts     Controller response/request types
│   │   ├── resources.ts One function per REST endpoint
│   │   └── queries.ts   react-query hooks + query keys
│   ├── messages.ts      Centralized user-facing copy (see below)
│   ├── compute.ts       Compute-reservation rules (cpu/memory quantities)
│   ├── format.ts        Formatting helpers (dates, ids, sizes)
│   ├── theme.ts         Theme constants
│   └── utils.ts         `cn()` class-merge + misc helpers
└── pages/               Route pages — thin compositions, one per route
    └── <page>/components/   Components extracted from a specific page
```

**Where new code goes**
- New route page → `pages/`, kept thin.
- Component used by only one page → `pages/<page>/components/`.
- Component reused across pages → `components/` (or `components/ui/` if it's a
  generic primitive).
- New REST endpoint → a function in `lib/api/resources.ts` + a hook in
  `lib/api/queries.ts`.
- Any user-facing string → `lib/messages.ts`.

## User-facing copy

Centralize all toast / dialog / tooltip / hint / empty-state strings in a single
messages module (`src/lib/messages.ts`) — static strings as consts, dynamic ones
as small functions — and reference them from components. Don't inline copy.

## API + data

REST calls live one-function-per-endpoint in `src/lib/api/resources.ts`;
react-query hooks + keys in `src/lib/api/queries.ts` (and `src/hooks/`).
Components call the hooks, never `fetch` directly.

## Before committing

`npm run check` (tsc) must pass.
