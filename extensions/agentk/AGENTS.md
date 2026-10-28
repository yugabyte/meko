# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **AgentK**. Humans
should read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file adds the map and the rules.

AgentK lives in `extensions/agentk` of the meko repo; the repo's own
[AGENTS.md](../../AGENTS.md) covers the rest of it. Paths here are relative to `extensions/agentk`,
except `.github/`, `.claude/`, `scripts/check-title.sh`, `scripts/ci-scope.sh` and `LICENSE`,
which are at the repo root. Run the commands below from `extensions/agentk`.

AgentK is a self-hosted web workbench for Claude Code, OpenCode and Kiro CLI (any one is enough). A project is an effort on a feature
or product, worked on in a git repo. Each task is a session with its own git worktree and a
Claude (or OpenCode, or Kiro) process held by AgentK's session host; the agent a session runs on is its harness
(`src/server/harness/`, [docs/design/harnesses.md](docs/design/harnesses.md)). A task runs on Explore → Design → Code → PR & Review → Merge & Close,
starting at Explore, Design or Code (the user picks), and a Review a PR session (someone else's PR,
with AgentK assisting the reviewer) stays in one Reviewing PR stage. The user can move a task
between stages by hand (`src/shared/stages.ts`). Meko datapacks hold project memory (a Meko Cloud key, else the Meko on this computer). The design
is in [docs/design.md](docs/design.md): an index, with one file per area under `docs/design/`.

**Naming:** the product is **AgentK** (it was AgentK Local until October 2026) in anything people
read (UI, docs, messages), and `agentk` names the command, the npm package, the release tarballs
(`agentk-<version>-…`), the systemd units (`agentk.service`, `agentk-host.service`) and the launchd
labels (`com.yugabyte.agentk`, `.host`). What installs from before the rename still have keeps
working: every release also ships its tarball as `agent-k-local-<version>-…` (until #238), its
`bin/agent-k-local` is a link to `bin/agentk` (`scripts/package-release.sh`), the installer links
both commands (and the server adds `agentk` beside an old link: `src/server/cmdlink.ts`), and a
service keeps its old name until `agentk install` moves it (`dropOldNames` in `src/server/service.ts`).
The repo itself is `yugabyte/meko` now (it was `yugabyte/agent-k-local`; GitHub redirects the old
name, so installs that still name it keep updating). `agent-k-local` stays in the socket folder, and every id AgentK gives Meko (its agent id, conversation keys, the
`source` of its memories, its upload `client`), which name what's stored there. Strings AgentK wrote
under the old product name and reads back still match it: project datapack descriptions (`AgentK
Local project "…"`, `src/shared/datapacks.ts`) and the mark on the Kiro agents it writes
(`src/server/kiro.ts`).

**Home:** AgentK's state (its "home") is `<Meko's data folder>/extensions/agentk`, by Meko's own
rule for that folder (`mekoHome()` in `src/server/paths.ts`, tested with Meko's against
`backend/internal/paths/testdata/resolve-cases.json`); users don't pick another place. A release's
home is the folder its release runs from, and `AK_HOME` names one only without a release (dev,
tests). A home from before (`~/.agent-k-local`, or a custom `AK_HOME`) moves there by itself after
an update (`src/server/movehome.ts`, [recovery.md](docs/design/recovery.md#moving-the-home-into-mekos-data-folder)).

## Commands

```sh
cd extensions/agentk
nvm use 22
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++  # RHEL 8, for node-pty (not on macOS)
npm ci
npm run typecheck && npm run lint && npm test   # run all three before every commit
npm run build                                   # dist/server + dist/web
npm run dev                                     # the server on ./.dev-home/web.sock, Vite on :5173 proxying to it; state in ./.dev-home (AK_DEV_HOME to change)
```

## Map

AgentK's top level, and the shared code both sides use. `src/server/` and `src/web/` each have their own
AGENTS.md with a row per module; Claude Code loads it when you work in that directory, and other
agents should read it before changing code there.

| Path | What it is |
|---|---|
| `src/shared/types.ts` | The API contract: every REST route, WebSocket message and shared type. Change it first. |
| `src/server/` | The server, the session host (`host/`) and the CLI. A row per module is in [src/server/AGENTS.md](src/server/AGENTS.md). |
| `src/web/` | The React + xterm.js UI. A row per module, and the rules every UI change follows (it must work on a phone), are in [src/web/AGENTS.md](src/web/AGENTS.md) and [src/web/REVIEW.md](src/web/REVIEW.md). |
| `src/shared/harnesses.ts` | The harnesses a session can run on (`claude`, `opencode`, `kiro`, in the order every list shows them), their labels, sign-in commands and what each can do (`HARNESS_CAPS`: the mod, Artifacts, tokens or credits, whether a by-model row names it, the oldest version sessions start on, and the command that upgrades one older when its own update can't), which the server and the UI both gate on; `harnessOf()` reads a stored id. |
| `src/shared/decisions.ts` | A design's Key Design Decisions (KDDs, `DesignQuestion`): numbering questions Q1… and AgentK's own decisions D1… (`decisionNumbers`), which the user can answer (`answerable`: an open question, or a decision to change), and the marker a design puts after a decision that rests on Meko (`MEKO_SOURCE_MARK`, `MEKO_SOURCE_CLASS`). Used by the server (the prompts), the Questions tab and the design panel (`design-frame.ts`). |
| `src/shared/stages.ts` | The rules for moving a session by hand: where a task can move, what can't be moved, where the move dialog starts, the prompt each move sends by default, what a move does in words, skipped steps, and `stepOf` (the PR stage shows as PR & Review). Used by the server and the move dialog. Also the Starting view's steps, which name the session's agent (`startupSteps()`). |
| `src/shared/datapacks.ts` | Project datapack names (`adlc/proj/<repo>/<project>`), the description AgentK writes on them (remote credentials stripped), matching one to a local repo, and the remote helpers `git.ts` uses too. Used by the server and the web UI. |
| `src/shared/worklinks.ts` | Links on work items (`parseWorkLink`: GitHub PRs and issues, Jira issues, Slack messages, other pages, each spelling of one thing under one key, and its chip's label; `workLinkKey` for the palette's lookup). Used by the server and the web UI. |
| `src/shared/branches.ts` | The branch prefix setting's `{login}` (`expandBranchPrefix`: gh's login, or left out with its joiner when gh can't say) and the default, `ak_{login}/`. Used by the server and the web UI. |
| `src/shared/termreports.ts` | Telling a terminal's answers to a program's queries from typing (`isTerminalReport`): the browser terminal and `agentk attach` drop them, since the session host answers them. Also mouse motion with no button held and focus reports (`isPassiveReport`), which don't count as using a screen (`src/server/screens.ts`). |
| `src/shared/emails.ts` | The email check, the parsing of a list of emails for the Share dialog (commas, semicolons, whitespace, `Name <address>`), pasting into its field and picking a suggested address (`typedEntry`, `pickEmail`, `suggestContacts`), and the most one invite can send to. Used by the server and the web UI. |
| `src/shared/mekourls.ts` | Meko Cloud's MCP and web app URLs, the command that installs Meko on this computer (`MEKO_INSTALL_COMMAND`, for the banner shown while there's none), the web app that goes with an MCP URL (`mekoWebUrl`), and a datapack's share link on it (`shareLinkUrl`, and `shareLinkToken`, which reads a pasted one only from that web app), and Meko's own pages AgentK links to (`MEKO_HOME_PATH`, its main page, from the projects pane's "m"; `MEKO_CLOUD_PATH` and `MEKO_ACCOUNT_PATH`, from Settings; `mekoDatapackPath()`, a datapack's page, which datapack names link to while AgentK uses the Meko on this computer: `mekoDatapackUrl()` in `src/web/util.ts`). Used by the server and the web UI, so Ask AgentK offers to join only the links the server will. |
| `src/shared/pricing.ts` | Anthropic API list prices per model and the cost of a set of tokens (cache writes by duration). |
| `src/mod/agentk/` | AgentK's mod: a Claude Code plugin of function hooks loaded into every session with `--plugin-dir`. It polls the hook socket for AgentK's prompts and submits them as the user's, reports the context fill and plan limits after each turn, forwards Claude's `AskUserQuestion` calls to AgentK while the terminal's dialog stays open (the first answer wins), reports when each of Claude's Meko tool calls starts and ends (for the session's dot, and with the call's content for the Meko call log while the project keeps one), reports a turn you interrupt (Ctrl+C, Esc: no `Stop` hook fires, `/mod/turn`), gives Claude the `design_questions` tool, runs learn steps (`$.model.fork`, then `$.model.complete` for the duplicate check), and registers `/agentk_learn`. Compiled by Claude Code against its own declarations, not by the repo's tsc; checked with `claude plugin validate` / `claude plugin test src/mod/agentk` (`test/unit/mod.test.ts`, skipped without Claude Code). |
| `src/tools/skill/SKILL.md` | The skill for AgentK's session tools (the `agentk_sessions` MCP server, `src/server/mcp.ts`): how an agent opens projects, starts sessions (from GitHub issues and PRs too), moves them and opens another session's PR. Its one source; the server copies it for every harness (`writeToolFiles` in `src/server/tools.ts`), and the build into `dist/tools`. See [docs/design/tools.md](docs/design/tools.md). |
| `scripts/deps.sh`, `scripts/install.sh` | `deps.sh`: AgentK's prerequisites, which `agentk install` runs first (a release has it as `lib/scripts/deps.sh`): installs missing tools (git, curl, gh, and a coding agent when none of Claude Code, OpenCode and Kiro CLI is there: it asks which, OpenCode second, or `--agent`) after asking (`--install-deps`, `--no-install-deps`; nothing with `--non-interactive` unless `--install-deps`), and checks each agent's sign-in and gh's. Meko installs AgentK (`meko install_extension agentk`: it unpacks the release, then runs `agentk install`), so `install.sh` only says so: releases still carry it for the Mekos and instructions that run it. Both tested against stub tools in `test/unit/install-script.test.ts`. |
| `packaging/`, `scripts/`, `.github/` | Launcher wrapper (`packaging/agentk`: runs the bundled Node, and hands `serve` and `host` to Meko's launcher, `meko _launch`, which runs a new version's probation and rollback), systemd and launchd templates, the prerequisites and release scripts, and (at the repo root, running from `extensions/agentk`) CI and releases (Linux and macOS) and Dependabot (`dependabot.yml`). The repo root's `scripts/check-title.sh` holds the commit subject and PR title rules, and `scripts/release/release-notes.sh` decides which types and components release each product (AgentK or Meko, `--product`; `scripts/release/product.sh` names each one's tags and components); their type lists are the ones the tests check (the docs, the commit skill and What's new's `RELEASE_TYPES` in `src/server/releasenotes.ts` repeat them, so change those too), and both scripts are tested in `test/unit/check-title.test.ts` and against Dependabot's prefixes in `test/unit/dependabot.test.ts`; `test/unit/release-scripts.test.ts` tests what each product counts (versions, notes, pruning). The repo root's `scripts/ci-scope.sh` decides which components' CI jobs a PR runs; `test/unit/ci-scope.test.ts` tests it, checks that every root file a test here reads (`../../../../…`) runs AgentK's job, that the Markdown a backend test globs or AgentK's release packs runs its component's, and that the `CI gate` job needs every other job. |
| `test/` | Unit tests, and integration tests against a real session host (in the test process) and git with a stub `claude` (`test/fixtures/fake-claude.mjs`), and stubs of `kiro-cli` and `opencode` (`fake-kiro.mjs`; `fake-opencode.mjs`, which runs AgentK's real OpenCode plugin as OpenCode 2 does). |
| `docs/` | The design: [docs/design.md](docs/design.md) is its index (the decisions so far, and which part covers what), with one file per area under `docs/design/`. Also the usage report's schema (`call-home-report.schema.json`) and the screenshots. |

## Invariants (don't break these)

1. **Sessions survive the server.** The session host runs in its own service (`agentk-host.service`
   on Linux, the `com.yugabyte.agentk.host` LaunchAgent on macOS). Never start the host or a session's
   Claude as children of the server's service or process group (one-shot `claude` commands that end with
   their request, such as `--version`, `update` and Ask AgentK's `claude -p`, are the server's own). Server restarts and updates must never stop a
   session. Manage the services only through `service.ts`. When the host service is installed, the server
   never starts a host itself; it reports the service as down. **The one exception** is the move of an
   older home into Meko's data folder (`src/server/movehome.ts`, decided by the project owner for
   #236): once, right after the release that brings it proves healthy, its job (outside both services)
   stops the server and the host, ending every session whatever it's doing, and recovery resumes them
   in the new home.
2. **Recovery is idempotent.** `recover()` and `reconcile()` may run at any time. A session with a live
   pane is left alone, and so is one still being created or launched (it has no pane yet); a missing
   pane is relaunched with `claude --resume`; auto-continue is guarded (once per 15 minutes; after two
   auto-continues in an hour the next one is parked). Archive and project delete wait for a session
   that is still starting; `launch()` never replaces a live pane.
3. **Only through Meko.** The server listens on nothing but its unix socket (`Paths.webSock`, in a
   folder only the user can open) and has no login of its own: the Meko on this computer serves it
   to browsers at `/ext/agentk/` after its own sign-in (local access, or the owner's login), and
   every request on the socket is let in. Never add a TCP listener, a login, or a route that trusts
   a header to say who sent it. Writes and WebSocket upgrades refuse an `Origin` that isn't the
   page's own (Meko passes the browser's `Host` and `Origin` on unchanged), keyed on the matched
   route (`req.routeOptions.url`), never the raw URL, which can be percent-encoded. Hooks use their
   own unix socket with a per-session token.
4. **No shell interpolation of user input.** Use `execFile`/`spawn` with argument arrays. The only
   shell text we generate is the launcher script, and every value in it goes through `shq()`.
5. **Migrations are append-only.** Add a new entry to `MIGRATIONS` in `db.ts`; never edit a shipped one.
6. **The session host outlives updates.** A running host keeps the code it started with, and restarting
   it ends every Claude: nothing restarts it by itself, except the one-time move of the home into Meko's
   data folder (invariant 1). Keep `src/server/host/` small; bump `PROTOCOL` in
   `host/protocol.ts` for a change an older peer can't read, and keep the server working with hosts back
   to `MIN_PROTOCOL`. Meko installs updates and deletes old versions, but never the folder a running
   host started from: the launcher starts `host` through Meko's launcher (`meko _launch`), which
   holds a lock on that folder while the host runs.
7. **Meko is Meko's Meko Cloud link, else the Meko on this computer; a project has at most one
   dedicated datapack.** It can't be turned off. Meko owns the link (its web UI's Settings › Meko
   Cloud, `meko cloud_link`); AgentK reads it, and the local Meko's URL and token, from `meko token
   --json` (`mekolocal.ts`), keeps them in memory and never writes them to `config.toml`, and has no
   route or UI that sets a key. A key AgentK kept itself before is handed to Meko once
   (`mekohandover.ts`) and never read again; until then, or under a Meko too old to keep a link,
   AgentK keeps using it rather than move to the local Meko unasked. The local token goes only to
   its own URL. With neither, AgentK doesn't talk to Meko: sessions get no datapack, pin or Meko
   lines in their prompt, and nothing is saved there. Each session gets its own
   `MEKO_AGENT_ID` (`sessionAgentId()`: `claude_code:<slug>-<hash>`, or `opencode:…` or `kiro:…` for an OpenCode or Kiro session, never the project id), with
   the project's dedicated datapack pinned for it when it starts; AgentK's writes for a session use
   the id it runs with (`launchedAgentId()`). Never remove a `pin-claude_code_<project id>.json`:
   it may be the user's own pin for plain Claude Code in that repo. (Claude Code's own
   Meko setup is the user's and is changed only when they ask: the installer's question or
   **Set up Claude Code with this Meko** in Settings › Harness.) Nothing that needs Meko may stop the install, a project or a session when there's no Meko or
   it's down: it's skipped or offered later.
8. **Treat the repo as public** (it's internal today). No internal hostnames, mount paths, tokens or
   keys in code, docs, tests or commit messages, and no personal data: no real person's name,
   email, handle or other details, and nothing a user wrote or stored. Tests and docs use made-up
   values (`alice@example.com`, `github.com/acme/demo-repo`). The repo root's AGENTS.md has the
   full rule.

## Conventions

- Shell commands and scripts run on Linux and macOS: stick to POSIX flags that GNU and BSD tools
  share (no `mv -T`, `stat -c`, `readlink -f`, `sleep infinity`; `sha256sum` or `shasum -a 256`).
- TypeScript strict, ES modules, Node 22. Lint includes the React hooks rules (`rules-of-hooks`,
  `exhaustive-deps`) for `src/web/`. Keep modules small and single-purpose, and match the style
  around you.
- User-facing text: plain, direct, active voice; errors say what went wrong and how to fix it.
  AgentK runs the workflow, so text about the work (designing, coding, reviews, turns) names
  **AgentK**; **Claude** is only for the Claude Code process and its terminal (starting, exits,
  Resume, typing into it), and for an OpenCode or Kiro session that text names it (`agentName()`). Prompts sent to Claude still address Claude.
- The server is the source of truth; the UI renders `AppState` pushed over `/ws/events`.
- Tests: new behavior gets a test, and only one: before adding a test, look for one that already
  covers the path and extend it (cases of one rule go in one `it.each` table). Don't test
  libraries, constants or copy, and don't assert internals where the visible result would do;
  REVIEW.md §6 flags redundant tests. Integration tests use a real session host and git; GitHub
  is mocked with `vi.mock('../../src/server/github.js')`. Tests must never touch the real
  `~/.claude.json` or Meko (see `test/helpers.ts`); `test/setup.ts` runs first in every test file, scrubs `MEKO_*`, `AK_*`,
  `CLAUDE*`, `KIRO_*`, `OPENCODE*`, `GH_*`, `GIT_*` and `TMUX*` from the environment and points `HOME` at an empty
  folder. `npm run typecheck` covers the tests too (`tsconfig.test.json`).
- Commits and PR titles: `<type>(<component>[/<area>])[!]: <summary>` with one of nine types and,
  for AgentK, the component `agentk` (`feat(agentk/web): …`; `feat(agentk,meko): …` for a change
  that ships in Meko too, which releases both), checked by
  the repo root's `scripts/check-title.sh` (see the repo root's `.claude/skills/commit/SKILL.md`).
  `feat`/`fix`/`perf`/`deps` commits trigger a release (`0.YYYYMMDD.N`) when pushed to `main`;
  `ci`/`test`/`docs`/`nit`/`chore` don't. PRs are squash-merged, so the PR title is the subject;
  the `PR title` workflow (`pr-title.yml`) checks it. Changes reach `main` only through PRs that
  pass CI and that check.

## Keep the docs current

Docs are part of the change. Update them **in the same commit** as the code:

| When you change… | Update |
|---|---|
| Behavior, architecture, lifecycle, security or data model | The part of `docs/design/` that covers it (a new part gets a row in the contents table of `docs/design.md`) |
| Features, requirements, install steps | `README.md` (the repo root's README is Meko's, and points here) |
| Dev setup, checks, release process | `CONTRIBUTING.md` |
| Module layout, invariants, commands, conventions | `AGENTS.md`: this one, or `src/server/AGENTS.md` or `src/web/AGENTS.md` for a module there (the repo root's for the repo layout and repo-wide rules) |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design/projects-and-sessions.md` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
