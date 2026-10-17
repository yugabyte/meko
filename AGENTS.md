# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **AgentK Local**. Humans
should read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file adds the map and the rules.

AgentK Local is a self-hosted web workbench for Claude Code and Kiro CLI (either one is enough). A project is an effort on a feature
or product, worked on in a git repo. Each task is a session with its own git worktree and a
Claude (or Kiro) process held by AgentK's session host; the agent a session runs on is its harness
(`src/server/harness/`, [docs/design/harnesses.md](docs/design/harnesses.md)). A task runs on Explore → Design → Code → PR & Review → Merge & Close,
starting at Explore, Design or Code (the user picks), and a Review a PR session (someone else's PR,
with AgentK assisting the reviewer) stays in one Reviewing PR stage. The user can move a task
between stages by hand (`src/shared/stages.ts`). Meko datapacks hold project memory (Meko is optional). The design
is in [docs/design.md](docs/design.md): an index, with one file per area under `docs/design/`.

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

The top level, and the shared code both sides use. `src/server/` and `src/web/` each have their own
AGENTS.md with a row per module; Claude Code loads it when you work in that directory, and other
agents should read it before changing code there.

| Path | What it is |
|---|---|
| `src/shared/types.ts` | The API contract: every REST route, WebSocket message and shared type. Change it first. |
| `src/server/` | The server, the session host (`host/`) and the CLI. A row per module is in [src/server/AGENTS.md](src/server/AGENTS.md). |
| `src/web/` | The React + xterm.js UI. A row per module, and the rules every UI change follows (it must work on a phone), are in [src/web/AGENTS.md](src/web/AGENTS.md) and [src/web/REVIEW.md](src/web/REVIEW.md). |
| `src/shared/harnesses.ts` | The harnesses a session can run on (`claude`, `kiro`), their labels and what each can do (`HARNESS_CAPS`: the mod, Artifacts, a needs-you hook, plan limits, tokens or credits, whether a by-model row names it), which the server and the UI both gate on; `harnessOf()` reads a stored id. |
| `src/shared/stages.ts` | The rules for moving a session by hand: where a task can move, what can't be moved, where the move dialog starts, the prompt each move sends by default, what a move does in words, skipped steps, and `stepOf` (the PR stage shows as PR & Review). Used by the server and the move dialog. |
| `src/shared/datapacks.ts` | Project datapack names (`adlc/proj/<repo>/<project>`), the description AgentK writes on them (remote credentials stripped), matching one to a local repo, and the remote helpers `git.ts` uses too. Used by the server and the web UI. |
| `src/shared/branches.ts` | The branch prefix setting's `{login}` (`expandBranchPrefix`: gh's login, or left out with its joiner when gh can't say) and the default, `ak_{login}/`. Used by the server and the web UI. |
| `src/shared/termreports.ts` | Telling a terminal's answers to a program's queries from typing (`isTerminalReport`): the browser terminal and `agent-k-local attach` drop them, since the session host answers them. Also mouse motion with no button held and focus reports (`isPassiveReport`), which don't count as using a screen (`src/server/screens.ts`). |
| `src/shared/emails.ts` | The email check, the parsing of a list of emails for the Share dialog (commas, semicolons, whitespace, `Name <address>`), pasting into its field and picking a suggested address (`typedEntry`, `pickEmail`, `suggestContacts`), and the most one invite can send to. Used by the server and the web UI. |
| `src/shared/mekourls.ts` | Meko Cloud's MCP and web app URLs, and the web app that goes with an MCP URL (`mekoWebUrl`). Used by the server and the web UI, so the Meko tab's sign-up links match what the server works out. |
| `src/shared/pricing.ts` | Anthropic API list prices per model and the cost of a set of tokens (cache writes by duration). |
| `src/mod/agentk/` | AgentK's mod: a Claude Code plugin of function hooks loaded into every session with `--plugin-dir`. It polls the hook socket for AgentK's prompts and submits them as the user's, reports the context fill and plan limits after each turn, forwards Claude's `AskUserQuestion` calls to AgentK Local while the terminal's dialog stays open (the first answer wins), reports when each of Claude's Meko tool calls starts and ends (for the session's dot, and with the call's content for the Meko call log while the project keeps one), reports a turn you interrupt (Ctrl+C, Esc: no `Stop` hook fires, `/mod/turn`), gives Claude the `design_questions` tool, runs learn steps (`$.model.fork`, then `$.model.complete` for the duplicate check), and registers `/agentk_learn`. Compiled by Claude Code against its own declarations, not by the repo's tsc; checked with `claude plugin validate` / `claude plugin test src/mod/agentk` (`test/unit/mod.test.ts`, skipped without Claude Code). |
| `scripts/install.sh` | The installer: stops early where AgentK Local is already installed (unless `--reinstall`), installs missing tools (git, curl, gh, and a coding agent when neither Claude Code nor Kiro CLI is there: it asks which, or `--agent`) after asking, checks each agent's sign-in, downloads and verifies the release, then runs `agent-k-local install`. Tested against stub tools in `test/unit/install-script.test.ts`. |
| `packaging/`, `scripts/`, `.github/` | Launcher wrapper, systemd and launchd templates, install and release scripts, CI and releases (Linux and macOS), and Dependabot (`dependabot.yml`). `scripts/check-title.sh` holds the commit subject and PR title rules, and `scripts/release-notes.sh` decides which types release; their type lists are the ones the tests check (the docs, the commit skill and What's new's `RELEASE_TYPES` in `src/server/releasenotes.ts` repeat them, so change those too), and both scripts are tested in `test/unit/check-title.test.ts` and against Dependabot's prefixes in `test/unit/dependabot.test.ts`. |
| `test/` | Unit tests, and integration tests against a real session host (in the test process) and git with a stub `claude` (`test/fixtures/fake-claude.mjs`). |
| `docs/` | The design: [docs/design.md](docs/design.md) is its index (the decisions so far, and which part covers what), with one file per area under `docs/design/`. Also the usage report's schema (`call-home-report.schema.json`) and the screenshots. |

## Invariants (don't break these)

1. **Sessions survive the server.** The session host runs in its own service (`agent-k-local-host.service`
   on Linux, the `com.yugabyte.agent-k-local.host` LaunchAgent on macOS). Never start the host or a session's
   Claude as children of the server's service or process group (one-shot `claude` commands that end with
   their request, such as `--version`, `update` and Ask AgentK's `claude -p`, are the server's own). Server restarts and updates must never stop a
   session. Manage the services only through `service.ts`. When the host service is installed, the server
   never starts a host itself; it reports the service as down.
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
6. **The session host outlives updates.** A running host keeps the code it started with, and restarting
   it ends every Claude: nothing restarts it by itself. Keep `src/server/host/` small; bump `PROTOCOL` in
   `host/protocol.ts` for a change an older peer can't read, and keep the server working with hosts back
   to `MIN_PROTOCOL`. The updater keeps the version directory the host runs from.
7. **Meko is optional; a project has at most one dedicated datapack.** Without a key (or with
   `[meko] enabled = false`) AgentK doesn't talk to Meko: sessions get no datapack, pin or Meko
   lines in their prompt, and nothing is saved there. Each session gets its own
   `MEKO_AGENT_ID` (`sessionAgentId()`: `claude_code:<slug>-<hash>`, or `kiro:<slug>-<hash>` for a Kiro session, never the project id), with
   the project's dedicated datapack pinned for it when it starts; AgentK's writes for a session use
   the id it runs with (`launchedAgentId()`). Never remove a `pin-claude_code_<project id>.json`:
   it may be the user's own pin for plain Claude Code in that repo. (Claude Code's own
   Meko setup is the user's and is changed only when they ask: the installer's question or
   **Set up Claude Code with this Meko**.) Nothing that needs Meko may stop the install, a project or a session when Meko is off or
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
  Resume, typing into it), and for a Kiro session that text names Kiro (`agentName()`). Prompts sent to Claude still address Claude.
- The server is the source of truth; the UI renders `AppState` pushed over `/ws/events`.
- Tests: new behavior gets a test, and only one: before adding a test, look for one that already
  covers the path and extend it (cases of one rule go in one `it.each` table). Don't test
  libraries, constants or copy, and don't assert internals where the visible result would do;
  REVIEW.md §6 flags redundant tests. Integration tests use a real session host and git; GitHub
  is mocked with `vi.mock('../../src/server/github.js')`. Tests must never touch the real
  `~/.claude.json` or Meko (see `test/helpers.ts`); `test/setup.ts` runs first in every test file, scrubs `MEKO_*`, `AK_*`,
  `CLAUDE*`, `KIRO_*`, `GH_*`, `GIT_*` and `TMUX*` from the environment and points `HOME` at an empty
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
| Behavior, architecture, lifecycle, security or data model | The part of `docs/design/` that covers it (a new part gets a row in the contents table of `docs/design.md`) |
| Features, requirements, install steps | `README.md` |
| Dev setup, checks, release process | `CONTRIBUTING.md` |
| Module layout, invariants, commands, conventions | `AGENTS.md`: the root one, or `src/server/AGENTS.md` or `src/web/AGENTS.md` for a module there |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design/projects-and-sessions.md` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
