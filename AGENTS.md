# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **AgentK Local**. Humans
should read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file adds the map and the rules.

AgentK Local is a self-hosted web workbench for Claude Code. Projects are git repos. Each task is a
session with its own git worktree and a tmux-hosted Claude process. Its type decides the stages:
Plan & Implement goes Design → Code → PR → Review → Merge & Close, Quick Code skips Design, and
Exploration stays in one Explore stage. Meko datapacks hold project memory. The design
is in [docs/design.md](docs/design.md).

**Naming:** the product is **AgentK Local** in anything people read (UI, docs, messages). The
identifier `agent-k-local` stays in the repo name, the CLI command, `~/.agent-k-local`, the systemd
units and launchd labels, tarball names and the npm package.

## Commands

```sh
nvm use 22
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++  # RHEL 8, for node-pty (not on macOS)
npm ci
npm run typecheck && npm run lint && npm test   # run all three before every commit
npm run build                                   # dist/server + dist/web
npm run dev                                     # API on 127.0.0.1:7777, Vite on :5173; state in ./.dev-home (AK_DEV_HOME to change)
```

## Map

| Path | What it is |
|---|---|
| `src/shared/types.ts` | The API contract: every REST route, WebSocket message and shared type. Change it first. |
| `src/server/cli.ts` | Entry point: `serve`, `install`, `uninstall`, `setup-link`, `status`, `logs`, `selftest`, `version`. |
| `src/server/core.ts` | Wires config, db, tmux, Meko and the services; timers; `state()` snapshot for the UI. |
| `src/server/server.ts` | Fastify routes, auth hook, origin checks, WebSockets (`/ws/events`, `/ws/term/:id`), static files. |
| `src/server/sessions.ts` | Session lifecycle in tmux: create, launch, hook-driven status, recovery and auto-continue, archive. |
| `src/server/lifecycle.ts` | Stages and gates, GitHub polling, the review watchers (by reviewer mode) and CI-fix watcher with their prompt queue, Merge & Close (merge, then cleanup). |
| `src/server/projects.ts` | Projects, repo scan, datapacks (create, attach, pin), project home and search. |
| `src/server/prompts.ts` | Instructions sent to Claude at each stage (overridable per repo in `.agent-k/stages/`). |
| `src/server/claude.ts` | Launcher scripts, the hooks settings file, the hook forwarder script, and the status line relay (reports plan limits, then runs the user's own status line, minus Meko's datapack suffix). |
| `src/server/limits.ts` | Plan usage limits (5-hour and weekly) from status line reports: parsing, expiry, and the latest reading kept in `kv`. |
| `src/server/hooks.ts` | Unix-socket receiver for Claude Code hook events. |
| `src/server/tmux.ts`, `git.ts`, `github.ts` | Thin wrappers over the tmux, git and gh CLIs. |
| `src/server/design.ts` | Reading a session's design: the markdown design file, or the page and claude.ai link of a published Artifact. The Design-stage edit guard is `SessionManager.guard()` (hook `guard.sh`, route `/guard` on the hook socket). |
| `src/server/changes.ts` | What a session changed vs. its base (committed, uncommitted, untracked): footer diff size and the Changes panel's per-file patches, with size caps, and both sides of changed images for previews. |
| `src/server/comments.ts` | Review comments on a session's diff: drafts, validation, formatting them for Claude, and delivery (now if Claude is at its prompt, else at the next turn end). |
| `src/server/meko.ts` | Meko MCP client. |
| `src/server/usage.ts` | Token usage: incremental transcript reader (main + subagents), which also finds pushes to the base branch, and per session/project/model/day totals plus the all-projects overview (`GET /api/usage`). |
| `src/server/db.ts`, `store.ts` | SQLite (`node:sqlite`), migrations, row ↔ API type mapping. |
| `src/server/auth.ts`, `tls.ts`, `trust.ts` | Login, TOTP, rate limiting; TLS certs; Claude Code folder trust. |
| `src/server/updater.ts`, `install.ts` | Self-update with rollback (tarball per platform and CPU); host install. |
| `src/server/mekosetup.ts` | Meko set up for Claude Code (plugin, MCP server, key): what's missing, and running Meko's own setup package from npm (integrity-checked) with the bundled Node. |
| `scripts/install.sh` | The installer: installs missing tools (tmux, git, curl, gh, Claude Code) after asking, downloads and verifies the release, then runs `agent-k-local install`. Tested against stub tools in `test/unit/install-script.test.ts`. |
| `src/server/service.ts` | The two background services (server, tmux) behind one `ServiceManager`: systemd user units on Linux, LaunchAgents on macOS. Install, start, state, remove, boot check. |
| `src/server/tty.ts`, `health.ts` | CLI output helpers (sections, marks, color only on a TTY); local `/healthz` probe used by `install` and `status`. |
| `src/web/components/StartupPanel.tsx` | The "Starting…" view over the terminal: startup steps from `Session.startup`, elapsed time, and the opening stage. |
| `src/web/components/SidePanel.tsx`, `DesignPanel.tsx`, `ChangesPanel.tsx` | Resizable right-hand panels: the design (also full page at `/p/:id/s/:sid/design`; Artifacts render in a sandboxed frame built by `src/web/design-frame.ts`) and the per-file diff of a session's changes with review comments and a Files menu (`src/web/diff.ts` parses the patches and re-anchors comments). |
| `src/web/components/UsagePanel.tsx`, `UsageView.tsx` | Token usage: the project home's panel (headline numbers, 14-day chart with table view, per-session and per-model tables) and the all-projects Usage page at `/usage` (adds merged PRs, pushed commits and a per-project table; both its tables sort on any column through `src/web/sort.ts`). |
| `src/web/notifyLeader.ts` | Which open page shows browser notifications: the one used last, through a Web Lock it steals on focus. |
| `src/web/` | React + xterm.js UI. `api.ts` wraps every route; `terminals.ts` caches one terminal per session, copies selections to the clipboard (tmux OSC 52 and ⌘C/Ctrl+C, via `clipboard.ts`), and handles touch scrolling (batched wheel steps, momentum after a flick, WebGL rendering on touch screens), the key bar's input, reconnecting (backoff, and the login screen on a 4401 close via `checkLogin()` in `api.ts`) and releasing phone terminals that are hidden or off screen. Has its own [AGENTS.md](src/web/AGENTS.md) and [REVIEW.md](src/web/REVIEW.md): every UI change must work on a phone. |
| `src/web/hooks/useMedia.ts`, `components/TopBar.tsx`, `SessionSheet.tsx`, `TermKeys.tsx` | The phone layout: breakpoint hooks and `--app-h`/`--app-top` (pin the app and the dialog and sheet overlays to the visible viewport, above the on-screen keyboard), the app bar (on the session screen it holds the session tabs), the ⋯ session sheet (which also holds the stage bar), and the terminal key bar and compose box. `src/web/keys.ts` holds the key sequences, bracketed paste and wheel reports they send. |
| `packaging/`, `scripts/`, `.github/workflows/` | Launcher wrapper, systemd and launchd templates, install and release scripts, CI and releases (Linux and macOS). |
| `test/` | Unit tests, and integration tests against real tmux and git with a stub `claude` (`test/fixtures/fake-claude.mjs`). |

## Invariants (don't break these)

1. **Sessions survive the server.** tmux runs in its own service (`agent-k-local-tmux.service` on
   Linux, the `com.yugabyte.agent-k-local.tmux` LaunchAgent on macOS). Never start tmux or Claude as
   children of the server's service or process group. Server restarts and updates must never stop a
   session. Manage the services only through `service.ts`. When the tmux service is installed, the server
   never starts tmux itself; it reports the service as down.
2. **Recovery is idempotent.** `recover()` and `reconcile()` may run at any time. A session with a live
   pane is left alone, and so is one still being created or launched (it has no pane yet); a missing
   pane is relaunched with `claude --resume`; auto-continue is guarded (once per 15 minutes; after two
   auto-continues in an hour the next one is parked). Archive and project delete wait for a session
   that is still starting; `launch()` never replaces a live pane.
3. **Everything behind login.** Every `/api/*` route except `auth/status|setup|login` requires the
   session cookie, and every WebSocket checks the cookie and refuses a cross-origin `Origin`. Access
   checks key on the matched route (`req.routeOptions.url`), never the raw URL, which can be
   percent-encoded. Hooks use the unix socket with a
   per-session token, never the network port.
4. **No shell interpolation of user input.** Use `execFile`/`spawn` with argument arrays. The only
   shell text we generate is the launcher script, and every value in it goes through `shq()`.
5. **Migrations are append-only.** Add a new entry to `MIGRATIONS` in `db.ts`; never edit a shipped one.
6. **tmux 2.7 compatibility** (RHEL 8): no `new-session -e`, window options via `set-window-option`,
   exact targets as `=name:`. macOS needs tmux 3.2 (the launchd job runs `tmux -D`), so 3.2-only
   flags are allowed only on the macOS path.
7. **Meko is required.** Projects can't exist without a dedicated datapack; sessions always get
   `MEKO_AGENT_ID=claude_code:<project>`, and the project's datapack is pinned for that agent id.
8. **Treat the repo as public** (it's internal today). No internal hostnames, mount paths, tokens or
   keys in code, docs, tests or commit messages.

## Conventions

- Shell commands and scripts run on Linux and macOS: stick to POSIX flags that GNU and BSD tools
  share (no `mv -T`, `stat -c`, `readlink -f`, `sleep infinity`; `sha256sum` or `shasum -a 256`).
- TypeScript strict, ES modules, Node 22. Lint includes the React hooks rules (`rules-of-hooks`,
  `exhaustive-deps`) for `src/web/`. Keep modules small and single-purpose, and match the style
  around you.
- User-facing text: plain, direct, active voice; errors say what went wrong and how to fix it.
  AgentK runs the workflow, so text about the work (designing, coding, reviews, turns) names
  **AgentK**; **Claude** is only for the Claude Code process and its terminal (starting, exits,
  Resume, typing into it). Prompts sent to Claude still address Claude.
- The server is the source of truth; the UI renders `AppState` pushed over `/ws/events`.
- Tests: new behavior gets a test. Integration tests use real tmux and git; GitHub is mocked with
  `vi.mock('../../src/server/github.js')`. Tests must never touch the real `~/.claude.json` or Meko
  (see `test/helpers.ts`); `test/setup.ts` runs first in every test file, scrubs `MEKO_*`, `AK_*`,
  `CLAUDE*`, `GH_*`, `GIT_*` and `TMUX*` from the environment and points `HOME` at an empty
  folder. `npm run typecheck` covers the tests too (`tsconfig.test.json`).
- Commits: Conventional Commits, **no agent attribution** (see `.claude/skills/commit/SKILL.md`).
  `feat`/`fix`/`perf` commits trigger a release (`0.YYYYMMDD.N`) when pushed to `main`.

## Keep the docs current

Docs are part of the change. Update them **in the same commit** as the code:

| When you change… | Update |
|---|---|
| Behavior, architecture, lifecycle, security or data model | `docs/design.md` |
| Features, requirements, install steps | `README.md` |
| Dev setup, checks, release process | `CONTRIBUTING.md` |
| Module layout, invariants, commands, conventions | `AGENTS.md` |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design.md` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
