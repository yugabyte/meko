# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **AgentK Local**. Humans
should read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file adds the map and the rules.

AgentK Local is a self-hosted web workbench for Claude Code. A project is an effort on a feature
or product, worked on in a git repo. Each task is a session with its own git worktree and a
tmux-hosted Claude process. A task runs on Explore → Design → Code → PR & Review → Merge & Close,
starting at Explore, Design or Code (the user picks), and a Review a PR session (someone else's PR,
with AgentK assisting the reviewer) stays in one Reviewing PR stage. The user can move a task
between stages by hand (`src/shared/stages.ts`). Meko datapacks hold project memory (Meko is optional). The design
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
| `src/server/sessions.ts` | Session lifecycle in tmux: create (`begin` answers once the row is in; the worktree and launch follow, and a worktree that can't be made is a failed start), launch, hook-driven status, recovery and auto-continue, archive. |
| `src/server/lifecycle.ts` | Stages and gates, moves by hand between stages, GitHub polling, the review watchers (by reviewer mode), the merge-conflict watcher (`[pr] resolve_conflicts`, also in Settings → General) and CI-fix watcher with their prompt queue, Merge & Close (merge, then cleanup), and a Review a PR session's watch on its PR (new commits, merged or closed) and Update to latest. |
| `src/server/prreview.ts` | Review a PR and sharing with reviewers: the PRs a review can start on (found on GitHub by the project's datapack ids, in any repo, each matched to a local clone, a fork first), cloning a PR's repo, and who reviews the project's open PRs (with emails remembered per GitHub login). A review's create path (fetching `refs/pull/<n>/head`, a review branch with no upstream) is in `sessions.ts`, its git helpers in `git.ts`. |
| `src/shared/stages.ts` | The rules for moving a session by hand: where a task can move, what can't be moved, where the move dialog starts, the prompt each move sends by default, what a move does in words, skipped steps, and `stepOf` (the PR stage shows as PR & Review). Used by the server and the move dialog. |
| `src/server/projects.ts` | Projects (a closed one opens again when its dedicated datapack is picked), repo scan, datapacks (create with the repo remote in the description, attach, pin; an invite is accepted in Meko before its project is opened), project home and search. |
| `src/server/prompts.ts` | Instructions sent to Claude at each stage (overridable per repo in `.agent-k/stages/`), and the Meko recall lines added to them in code. |
| `src/server/recall.ts` | Meko recall at session start: the search on a new session's task (and the issues it names), picking and formatting what it found, and holding it for the `SessionStart` context hook (`/context` on the hook socket). |
| `src/server/claude.ts` | Launcher scripts, the hooks settings file, the hook forwarder script, the SessionStart context hook, and the status line relay (reports plan limits, then runs the user's own status line, minus Meko's datapack suffix). |
| `src/server/limits.ts` | Plan usage limits (5-hour and weekly) from status line reports: parsing, expiry, and the latest reading kept in `kv`. |
| `src/server/hooks.ts` | Unix-socket receiver for Claude Code hook events, the edit guard's decisions and the SessionStart context. It binds under a temporary name and renames into place, so closing never removes another server's socket; `socketId()` tells its file from one put there since (the server binds again when it changes). |
| `src/server/instance.ts` | One server per AgentK home, even across hosts sharing it over NFS: the `server.lock` lease (heartbeat, stale after 45 s), taken before `serve` opens anything; on this host it waits for the old lease to be released or go stale, and a refused start exits 75. |
| `src/server/tmux.ts`, `git.ts`, `github.ts` | Thin wrappers over the tmux, git and gh CLIs. |
| `src/server/worktrees.ts` | The worktree pool: which session owns each worktree, idle ones shared by every project on a repo (keyed by its real path) so build outputs carry over, claiming, releasing, trimming to `[worktrees] idle_per_repo`, removing ones unused for `idle_days`, and the startup sweep. |
| `src/server/design.ts` | Reading a session's design: the markdown design file, or the page and claude.ai link of a published Artifact; its open questions (`questions.json` beside it, or a markdown design's "Open questions" list) and the answers formatted for Claude. The Design-stage edit guard is `SessionManager.guard()` (hook `guard.sh`, route `/guard` on the hook socket). |
| `src/server/changes.ts` | What a session changed vs. its base (committed, uncommitted, untracked): footer diff size and the Changes panel's per-file patches, with size caps, and both sides of changed images for previews. |
| `src/server/comments.ts` | Review comments on a session's diff: drafts, validation, formatting them for Claude, and delivery (now if Claude is at its prompt, else at the next turn end). |
| `src/server/answers.ts` | Answers to a design's open questions: drafts (one per question, plus other feedback), validation against the stored questions, delivery like review comments (a design waiting for approval counts as at its prompt), and the unsent ones taken into an approval. Reading and normalizing the questions (the questions file, or a markdown design's "Open questions" list) and formatting answers are in `design.ts`. |
| `src/server/meko.ts` | Meko MCP client. |
| `src/server/mekoapi.ts` | Meko REST client for sharing (share, members, invites, accept or decline). Whether sharing is on (`MekoStatus.sharing`) is worked out from the config in `core.ts`. |
| `src/server/invites.ts` | Project datapacks shared with us: synced from Meko into `datapack_invites`, the first-sync baseline, accept, decline, later. |
| `src/server/mekosettings.ts` | The Meko tab in Settings: what it shows (never the key), checking a change against Meko before it's saved, and running Meko's setup for Claude Code. |
| `src/shared/datapacks.ts` | Project datapack names (`adlc/proj/<repo>/<project>`), the description AgentK writes on them (remote credentials stripped), matching one to a local repo, and the remote helpers `git.ts` uses too. Used by the server and the web UI. |
| `src/server/usage.ts` | Token usage: incremental transcript reader (main + subagents), which also finds pushes to the base branch and Meko calls, and per session/project/model/day totals and costs plus the all-projects overview (`GET /api/usage`). |
| `src/server/callhome.ts`, `filetypes.ts` | Anonymous usage stats: the report (`buildReport`, counts only, Meko use included; its contract is `CallHomeReport` and `docs/call-home-report.schema.json`), sending it to the collector (hourly in an install's first day, then daily; `[call_home]`, or the `CALL_HOME_URL` a release carries), and file types (paths reduced to extensions right away). |
| `src/server/mekousage.ts`, `mekosavings.ts` | Meko calls in transcripts: finding them and their results (found, empty, failed), the counts per session and project, a `seed` row for what AgentK's search at start passed to Claude, and the tokens Meko brought into first turns, and for the usage stats calls by tool (`toolCounts`, naming only the lookups and saves) and the searches at start (`mekoStarts`); the fixed formula, with its constants, for what a search that found something (or a seed) saved. |
| `src/shared/mekourls.ts` | Meko Cloud's MCP and web app URLs, and the web app that goes with an MCP URL (`mekoWebUrl`). Used by the server and the web UI, so the Meko tab's sign-up links match what the server works out. |
| `src/shared/pricing.ts` | Anthropic API list prices per model and the cost of a set of tokens (cache writes by duration). |
| `src/server/db.ts`, `store.ts` | SQLite (`node:sqlite`), migrations, row ↔ API type mapping (`sessions.kind`, and `Session.kind`, keep the old type names for tasks; `kindOf()` in `stages.ts` reads them as `task`). |
| `src/server/auth.ts`, `tls.ts`, `trust.ts` | Login, TOTP, rate limiting; TLS certs; Claude Code folder trust. |
| `src/server/updater.ts`, `install.ts` | Self-update with rollback (tarball per platform and CPU); host install. |
| `src/server/mekosetup.ts` | Meko set up for Claude Code (plugin, MCP server, key): what's missing, and running Meko's own setup package from npm (integrity-checked) with the bundled Node. |
| `scripts/install.sh` | The installer: installs missing tools (tmux, git, curl, gh, Claude Code) after asking, downloads and verifies the release, then runs `agent-k-local install`. Tested against stub tools in `test/unit/install-script.test.ts`. |
| `src/server/service.ts` | The two background services (server, tmux) behind one `ServiceManager`: systemd user units on Linux (run only on the install host, `ConditionHost=`), LaunchAgents on macOS. Install, start, state, remove, boot check. |
| `src/server/tty.ts`, `health.ts` | CLI output helpers (sections, marks, color only on a TTY); local `/healthz` probe used by `install` and `status`. |
| `src/web/components/StartupPanel.tsx` | The "Starting…" view over the terminal, shown as soon as the New session dialog closes: startup steps from `Session.startup`, elapsed time, and the opening stage; after 90 s on the last steps, **Show terminal** hides it for that start. |
| `src/web/components/SidePanel.tsx`, `DesignPanel.tsx`, `DesignQuestions.tsx`, `ChangesPanel.tsx` | Resizable right-hand panels: the design (also full page at `/p/:id/s/:sid/design`; Artifacts render in a sandboxed frame built by `src/web/design-frame.ts`) with its open questions on a Questions tab (beside it on the wide full page; helpers in `src/web/design-answers.ts`) and the per-file diff of a session's changes with review comments and a Files menu (`src/web/diff.ts` parses the patches and re-anchors comments). |
| `src/web/components/NewProjectDialog.tsx`, `ShareDialog.tsx`, `ShareReviewers.tsx`, `Banners.tsx` | Open project (the dedicated datapack first, or No datapack, then the project name above the repo, picked, cloned, another folder or a new repo made by `initRepo()` in `git.ts`, with a new datapack's name last; invites and shares listed first, the repo and name filled in from the datapack, or from the closed project it opens again), the Share dialog (through Meko's share API, with the reviewers of the project's open PRs first, `ShareReviewers.tsx`), and the banners, share invites and failed starts (Try again, Dismiss) included. |
| `src/web/components/NewSessionDialog.tsx`, `ReviewPrPicker.tsx` | New session (closes once the session is written, not when it has started; filled in from a failed start by **Try again**): name, where to start and first prompt, then **Advanced** (collapsed, with a one-line summary): the repo (the project's or another scanned one), the base branch, and the branch (typed as is, or named from the session). It opens by itself for a project just opened, on Review a PR when the project has PRs to review. For Review a PR, `ReviewPrPicker` replaces the first prompt: the project's PRs, another PR by number or link, Clone and Attach it. The start picker (`StartPicker`: Explore, Design and Code as pills joined like the stage bar, dotted from Explore, then "or" and Review a PR; one column on a phone) is in `common.tsx`. |
| `src/web/components/MoveSessionDialog.tsx` | Move to another stage: the stages a task can move to, the task when the session has none, whether to send the new stage's prompt, and a line on what the move does. Opened from the stage bar (a stage, or Explore's **Move on…**), the ⋯ sheet and the command palette; hosted in `App.tsx`. |
| `src/web/components/CloseProjectDialog.tsx` | Offered when the project on screen loses its last open session: Close project, New session or Keep it open (`lastSessionClosed()` in `src/web/util.ts` spots it). |
| `src/web/components/MekoKeyDialog.tsx`, `MekoGetKey.tsx`, `AddDatapackDialog.tsx` | Turning Meko on with an API key (`MekoGetKey` links to Meko's sign-up and login, here and on the Meko tab, to make one), and giving a project without one its dedicated datapack, or switching it to another (the old one kept as shared or detached; both dialogs reached from the project home's Share and Datapacks box). |
| `src/web/components/UsagePanel.tsx`, `UsageView.tsx`, `MekoUsageSection.tsx` | Token usage: the project home's panel (headline numbers with the estimated cost, 14-day chart with table view, per-session and per-model tables) and the all-projects Usage page at `/usage` (adds merged PRs, pushed commits and a per-project table; both its tables sort on any column through `src/web/sort.ts`). Both show the Meko section: lookups, saves, the estimated savings and the tokens Meko brought into first turns (the seed AgentK passes at session start, and Claude's searches; seeds count toward the savings too), under the Meko logo for the theme (`public/meko-black.png`, `meko-white.png`). |
| `src/web/events.ts`, `hooks/useEvents.ts` | The `/ws/events` connection behind the UI's `AppState`: reconnecting with backoff, and pinging a socket that has been quiet for 30 seconds or whose page was just shown again or came back online, then replacing it when no answer comes back (a phone often never hears that the server dropped its socket); deadlines that pass double the next ones, for slow links. |
| `src/web/reload.ts`, `hooks/useReload.ts` | Reloading an open page once the server runs another version than the one stamped into the page (`<meta name="ak-version">`, added in `server.ts`), waiting while the user is busy (a dialog open, a field focused, unsent text), with a Reload banner meanwhile and a once-per-version guard. |
| `src/web/notifyLeader.ts`, `public/sw.js` | Which open page shows browser notifications (the one used last, through a Web Lock it steals on focus), and the service worker that shows them and, on a click, switches to the tab already showing the session. |
| `src/web/` | React + xterm.js UI. `api.ts` wraps every route; `terminals.ts` caches one terminal per session, copies selections to the clipboard (tmux OSC 52, ⌘C/Ctrl+C, and the key bar's select mode on touch screens, via `clipboard.ts`), and handles touch scrolling (batched wheel steps, momentum after a flick, WebGL rendering on touch screens), the key bar's input, reconnecting (backoff, and the login screen on a 4401 close via `checkLogin()` in `api.ts`) and releasing phone terminals that are hidden or off screen. Has its own [AGENTS.md](src/web/AGENTS.md) and [REVIEW.md](src/web/REVIEW.md): every UI change must work on a phone. |
| `src/web/hooks/useMedia.ts`, `components/TopBar.tsx`, `SessionSheet.tsx`, `TermKeys.tsx` | The phone layout: breakpoint hooks and `--app-h`/`--app-top` (pin the app and the dialog and sheet overlays to the visible viewport, above the on-screen keyboard), the app bar (on the session screen it holds the session tabs), the ⋯ session sheet (which also holds the stage bar), and the terminal key bar and compose box. `src/web/keys.ts` holds the key sequences, bracketed paste and wheel reports they send. |
| `packaging/`, `scripts/`, `.github/` | Launcher wrapper, systemd and launchd templates, install and release scripts, CI and releases (Linux and macOS), and Dependabot (`dependabot.yml`). `scripts/check-title.sh` holds the commit subject and PR title rules, and `scripts/release-notes.sh` decides which types release; their type lists are the ones the tests check (the docs and the commit skill repeat them, so change those too), and both scripts are tested in `test/unit/check-title.test.ts` and against Dependabot's prefixes in `test/unit/dependabot.test.ts`. |
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
   session cookie, and every WebSocket checks the cookie and refuses a cross-origin `Origin`. The
   one exception is local access (`localAccess()` in `auth.ts`): with the server listening only on
   loopback, a same-site loopback request whose `Host` is localhost needs no cookie. A network bind always
   requires the login, even from localhost. Access
   checks key on the matched route (`req.routeOptions.url`), never the raw URL, which can be
   percent-encoded. Hooks use the unix socket with a
   per-session token, never the network port.
4. **No shell interpolation of user input.** Use `execFile`/`spawn` with argument arrays. The only
   shell text we generate is the launcher script, and every value in it goes through `shq()`.
5. **Migrations are append-only.** Add a new entry to `MIGRATIONS` in `db.ts`; never edit a shipped one.
6. **tmux 2.7 compatibility** (RHEL 8): no `new-session -e`, window options via `set-window-option`,
   exact targets as `=name:`. macOS needs tmux 3.2 (the launchd job runs `tmux -D`), so 3.2-only
   flags are allowed only on the macOS path.
7. **Meko is optional; a project has at most one dedicated datapack.** Without a key (or with
   `[meko] enabled = false`) AgentK doesn't talk to Meko: sessions get no datapack, pin or Meko
   lines in their prompt, and nothing is saved there. Each session gets its own
   `MEKO_AGENT_ID` (`sessionAgentId()`: `claude_code:<slug>-<hash>`, never the project id), with
   the project's dedicated datapack pinned for it when it starts; AgentK's writes for a session use
   the id it runs with (`launchedAgentId()`). Never remove a `pin-claude_code_<project id>.json`:
   it may be the user's own pin for plain Claude Code in that repo. (Claude Code's own
   Meko setup is the user's and isn't changed.) Nothing that needs Meko may stop the install, a project or a session when Meko is off or
   down: it's skipped or offered later.
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
- Commits and PR titles: `<type>[(<component>)][!]: <summary>` with one of nine types, checked by
  `scripts/check-title.sh`, and **no agent attribution** (see `.claude/skills/commit/SKILL.md`).
  `feat`/`fix`/`perf`/`deps` commits trigger a release (`0.YYYYMMDD.N`) when pushed to `main`;
  `ci`/`test`/`docs`/`nit`/`chore` don't. PRs are squash-merged, so the PR title is the subject;
  the `PR title` workflow (`pr-title.yml`) checks it. Changes reach `main` only through PRs that
  pass CI and that check.

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
