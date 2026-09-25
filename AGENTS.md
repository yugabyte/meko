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
| `src/server/claude.ts` | Launcher scripts, the hooks settings file, the hook forwarder script. |
| `src/server/hooks.ts` | Unix-socket receiver for Claude Code hook events. |
| `src/server/tmux.ts`, `git.ts`, `github.ts` | Thin wrappers over the tmux, git and gh CLIs. |
| `src/server/meko.ts` | Meko MCP client. |
| `src/server/db.ts`, `store.ts` | SQLite (`node:sqlite`), migrations, row ↔ API type mapping. |
| `src/server/auth.ts`, `tls.ts`, `trust.ts` | Login, TOTP, rate limiting; TLS certs; Claude Code folder trust. |
| `src/server/updater.ts`, `install.ts` | Self-update with rollback; host install and systemd units. |
| `src/web/` | React + xterm.js UI. `api.ts` wraps every route; `terminals.ts` caches one terminal per session. |
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
| Anything visible in the UI | `docs/screenshots/*.png` (retake them from a demo instance with neutral paths and names) |
| Dev setup, checks, release process | `CONTRIBUTING.md` |
| Module layout, invariants, commands, conventions | `AGENTS.md` |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design.html` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
