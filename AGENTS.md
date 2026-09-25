# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **AgentK Local**. Humans
should read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file adds the map and the rules.

AgentK Local is a self-hosted web workbench for Claude Code. Projects are git repos. Each task is a
session with its own git worktree and a tmux-hosted Claude process, moving through
Design → Code → PR → Review → Merge → Cleanup. Meko datapacks hold project memory. The design
is in [docs/design.html](docs/design.html).

**Naming:** the product is **AgentK Local** in anything people read (UI, docs, messages). The
identifier `agent-k-local` stays in the repo name, the CLI command, `~/.agent-k-local`, the systemd
units, tarball names and the npm package.

## Commands

```sh
nvm use 22
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++  # RHEL 8, for node-pty
npm ci
npm run typecheck && npm run lint && npm test   # run all three before every commit
npm run build                                   # dist/server + dist/web
npm run dev                                     # API on 127.0.0.1:7777, Vite on :5173 (set AK_HOME to isolate state)
```

## Map

| Path | What it is |
|---|---|
| `src/shared/types.ts` | The API contract: every REST route, WebSocket message and shared type. Change it first. |
| `src/server/cli.ts` | Entry point: `serve`, `install`, `uninstall`, `setup-link`, `status`, `selftest`, `version`. |
| `src/server/core.ts` | Wires config, db, tmux, Meko and the services; timers; `state()` snapshot for the UI. |
| `src/server/server.ts` | Fastify routes, auth hook, origin checks, WebSockets (`/ws/events`, `/ws/term/:id`), static files. |
| `src/server/sessions.ts` | Session lifecycle in tmux: create, launch, hook-driven status, recovery and auto-continue, archive. |
| `src/server/lifecycle.ts` | Stages and gates, GitHub polling, the AgentK review loop, merge, cleanup. |
| `src/server/projects.ts` | Projects, repo scan, datapacks (create, attach, pin), project home and search. |
| `src/server/prompts.ts` | Instructions sent to Claude at each stage (overridable per repo in `.agent-k/stages/`). |
| `src/server/claude.ts` | Launcher scripts, the hooks settings file, the hook forwarder script, and the status line relay (reports plan limits, then runs the user's own status line). |
| `src/server/limits.ts` | Plan usage limits (5-hour and weekly) from status line reports: parsing, expiry, and the latest reading kept in `kv`. |
| `src/server/hooks.ts` | Unix-socket receiver for Claude Code hook events. |
| `src/server/tmux.ts`, `git.ts`, `github.ts` | Thin wrappers over the tmux, git and gh CLIs. |
| `src/server/design.ts` | Reading a session's design: the markdown design file, or the page and claude.ai link of a published Artifact. The Design-stage edit guard is `SessionManager.guard()` (hook `guard.sh`, route `/guard` on the hook socket). |
| `src/server/changes.ts` | What a session changed vs. its base (committed, uncommitted, untracked): footer diff size and the Changes panel's per-file patches, with size caps, and both sides of changed images for previews. |
| `src/server/comments.ts` | Review comments on a session's diff: drafts, validation, formatting them for Claude, and delivery (now if Claude is at its prompt, else at the next turn end). |
| `src/server/meko.ts` | Meko MCP client. |
| `src/server/usage.ts` | Token usage: incremental transcript reader (main + subagents) and per session/project/model/day totals. |
| `src/server/db.ts`, `store.ts` | SQLite (`node:sqlite`), migrations, row ↔ API type mapping. |
| `src/server/auth.ts`, `tls.ts`, `trust.ts` | Login, TOTP, rate limiting; TLS certs; Claude Code folder trust. |
| `src/server/updater.ts`, `install.ts` | Self-update with rollback; host install and systemd units. |
| `src/server/tty.ts`, `health.ts` | CLI output helpers (sections, marks, color only on a TTY); local `/healthz` probe used by `install` and `status`. |
| `src/web/components/StartupPanel.tsx` | The "Starting…" view over the terminal: startup steps from `Session.startup`, elapsed time, and the opening stage. |
| `src/web/components/SidePanel.tsx`, `DesignPanel.tsx`, `ChangesPanel.tsx` | Resizable right-hand panels: the design (also full page at `/p/:id/s/:sid/design`; Artifacts render in a sandboxed frame built by `src/web/design-frame.ts`) and the per-file diff of a session's changes with review comments and a Files menu (`src/web/diff.ts` parses the patches and re-anchors comments). |
| `src/web/components/UsagePanel.tsx` | Project token usage: headline numbers, 14-day chart with table view, per-session and per-model tables. |
| `src/web/` | React + xterm.js UI. `api.ts` wraps every route; `terminals.ts` caches one terminal per session, copies selections to the clipboard (tmux OSC 52 and ⌘C/Ctrl+C, via `clipboard.ts`), and handles touch scrolling, the key bar's input and releasing hidden phone terminals. Has its own [AGENTS.md](src/web/AGENTS.md) and [REVIEW.md](src/web/REVIEW.md): every UI change must work on a phone. |
| `src/web/hooks/useMedia.ts`, `components/TopBar.tsx`, `SessionSheet.tsx`, `TermKeys.tsx` | The phone layout: breakpoint hooks and `--app-h` (follows the on-screen keyboard), the app bar, the ⋯ session sheet, and the terminal key bar and compose box. `src/web/keys.ts` holds the key sequences, bracketed paste and wheel reports they send. |
| `packaging/`, `scripts/`, `.github/workflows/` | Launcher wrapper, systemd templates, install and release scripts, CI and releases. |
| `test/` | Unit tests, and integration tests against real tmux and git with a stub `claude` (`test/fixtures/fake-claude.mjs`). |

## Invariants (don't break these)

1. **Sessions survive the server.** tmux runs in its own systemd unit (`agent-k-local-tmux.service`).
   Never start tmux or Claude as children of the server's unit or process group. Server restarts and
   updates must never stop a session.
2. **Recovery is idempotent.** `recover()` and `reconcile()` may run at any time. A session with a live
   pane is left alone; a missing pane is relaunched with `claude --resume`; auto-continue is guarded
   (once per 15 minutes, parked after two in an hour).
3. **Everything behind login.** Every `/api/*` route except `auth/status|setup|login` requires the
   session cookie, and every WebSocket checks cookie and `Origin`. Hooks use the unix socket with a
   per-session token, never the network port.
4. **No shell interpolation of user input.** Use `execFile`/`spawn` with argument arrays. The only
   shell text we generate is the launcher script, and every value in it goes through `shq()`.
5. **Migrations are append-only.** Add a new entry to `MIGRATIONS` in `db.ts`; never edit a shipped one.
6. **tmux 2.7 compatibility** (RHEL 8): no `new-session -e`, window options via `set-window-option`,
   exact targets as `=name:`.
7. **Meko is required.** Projects can't exist without a dedicated datapack; sessions always get
   `MEKO_AGENT_ID=claude_code:<project>`, and the project's datapack is pinned for that agent id.
8. **Treat the repo as public** (it's internal today). No internal hostnames, mount paths, tokens or
   keys in code, docs, tests or commit messages.

## Conventions

- TypeScript strict, ES modules, Node 22. Keep modules small and single-purpose, and match the style
  around you.
- User-facing text: plain, direct, active voice; errors say what went wrong and how to fix it.
- The server is the source of truth; the UI renders `AppState` pushed over `/ws/events`.
- Tests: new behavior gets a test. Integration tests use real tmux and git; GitHub is mocked with
  `vi.mock('../../src/server/github.js')`. Tests must never touch the real `~/.claude.json` or Meko
  (see `test/helpers.ts`).
- Commits: Conventional Commits, **no agent attribution** (see `.claude/skills/commit/SKILL.md`).
  `feat`/`fix`/`perf` commits trigger a release (`0.YYYYMMDD.N`) when pushed to `main`.

## Keep the docs current

Docs are part of the change. Update them **in the same commit** as the code:

| When you change… | Update |
|---|---|
| Behavior, architecture, lifecycle, security or data model | `docs/design.html` (and bump its version label) |
| Features, requirements, install steps | `README.md` |
| Anything visible in the UI | `docs/screenshots/*.png` (retake them from a demo instance with neutral paths and names), including `mobile.png` for the phone layout |
| Dev setup, checks, release process | `CONTRIBUTING.md` |
| Module layout, invariants, commands, conventions | `AGENTS.md` |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design.html` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
