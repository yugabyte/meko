# AGENTS.md

Guide for coding agents (Claude Code, Codex, Cursor, and others) working on **Kode**. Humans
should read the repo's [CONTRIBUTING.md](../../../CONTRIBUTING.md) first; this file adds the map and
the rules.

Kode lives in `src/extensions/agentk` of the meko repo; the repo's own
[AGENTS.md](../../../AGENTS.md) covers the rest of it. Paths here are relative to
`src/extensions/agentk`,
except `.github/`, `.claude/`, `scripts/check-title.sh`, `scripts/ci-scope.sh` and `LICENSE`,
which are at the repo root. Run the commands below from `src/extensions/agentk`.

Kode is a self-hosted web workbench for Claude Code, OpenCode and Kiro CLI (any one is enough). A project is an effort on a feature
or product, worked on in a git repo. Each task is a session with its own git worktree and a
Claude (or OpenCode, or Kiro) process held by Kode's session host; the agent a session runs on is its harness
(`src/server/harness/`, [docs/design/harnesses.md](docs/design/harnesses.md)). A task runs on Explore → Design → Code → Pull Request → Merge & Close,
starting at Explore, Design or Code (the user picks), and a Review a PR session (someone else's PR,
with Kode assisting the reviewer) stays in one Reviewing PR stage. The user can move a task
between stages by hand (`src/shared/stages.ts`). Meko datapacks hold project memory (the Meko on this computer, which reaches Meko Cloud once it's linked there). The design
is in [docs/design.md](docs/design.md): an index, with one file per area under `docs/design/`.

**Naming:** the product is **Kode** (it was AgentK until November 2026, and AgentK Local until
October 2026) in anything people and sessions see: the UI, docs, messages and prompts, Ask Kode,
the session tools (`kode_sessions`, `mcp__kode__design_questions`, `/kode_learn`), the mods'
plugins (`kode`, `kode-view`), Kiro's agents (`kode`, `kode-trusted`, `kode-ask`, `kode-usage`) and
the variables sessions get (`KODE_HOOK_SOCK`, `KODE_HOOK_TOKEN`…). Meko knows it as the extension
`kode`: its folder in Meko's data folder (`extensions/kode`), `/ext/kode/`, the tags (`kode-v…`) and
the release tarballs (`kode-<version>-…`). The repo keeps the name `agentk`: this folder,
`src/mod/agentk`, the npm package, the commit component and label, the CI job and
`agentk-release.yml`; and the branch prefix stays `ak_{login}/`. People run it through Meko,
`meko kode <command>`: it has no command of its own on PATH. The program is `me_kode`, so a process
list and the services say it's a Meko extension's: the release's launcher (`bin/me_kode`, which
`meko kode` runs) and its Node (`node/bin/me_kode`), the systemd units (`me_kode.service`,
`me_kode-host.service`) and the launchd labels (`com.yugabyte.me_kode`, `.host`). It was `me_koder`
before, `agentk` before that, and `agent-k-local` before the rename to AgentK.
What installs from before still have keeps working: every release's `bin/me_koder` and `bin/agentk`
are links to `bin/me_kode` (`scripts/package-release.sh`), Meko's start removes the old
`~/.local/bin` links (Kode never makes one: `src/server/cmdlink.ts`), and a service keeps its old
name until `meko kode install` moves it (`dropOldNames` in `src/server/service.ts`). One from before
the rename to AgentK is the exception: it starts a program releases since #515 don't have, so after
an update it doesn't start until `meko kode install` moves it. A Meko from
before Kode was called Kode knows it only as `agentk`: the launcher asks Meko which name it knows
(`meko _launch --supported kode`), the page takes its `<base href>` from the path Meko serves it
under (`X-Meko-Prefix`, else `/ext/agentk/`: `pageBase` in `src/server/server.ts`), and the home
stays in `extensions/agentk` until Meko knows `kode` (`mekoKnowsKode`). Sessions launched before the
update carry the `AK_*` variables, which the hook scripts, the mods and the MCP server still read.
The repo itself is `yugabyte/meko` now (it was `yugabyte/agent-k-local`; GitHub redirects the old
name, so installs that still name it keep updating). `agent-k-local` stays in the socket folder, and
every id Kode gives Meko (its agent id, conversation keys, the `source` of its memories, its upload
`client`), which name what's stored there; so do the usage report's wire names (`callhome_type`,
`agentk`, `askAgentk`), SQLite's table and column names, and the review clones' remote. Strings Kode
wrote under an older name and reads back still match it: project datapack descriptions (`Kode
project "…"`, and `AgentK project` and `AgentK Local project` before: `src/shared/datapacks.ts`),
the mark on the Kiro agents it writes (`src/server/kiro.ts`), the PR's `me_kode_review:` line
(`me_ak_review:` before), a design question's `decidedBy` (`kode`, `agentk` before), a repo's
`.kode/stages` prompts (`.agent-k/stages` before) and `config.toml`'s `[ask_kode]` (`[ask_agentk]`
before).

**Home:** Kode's state (its "home") is `<Meko's data folder>/extensions/kode`, by Meko's own
rule for that folder (`mekoHome()` in `src/server/paths.ts`, tested with Meko's against
`src/meko/internal/paths/testdata/resolve-cases.json`); users don't pick another place. A release's
home is the folder its release runs from, and `KODE_HOME` names one only without a release (dev,
tests). A home from before (`~/.agent-k-local`, a custom `AK_HOME`, or `extensions/agentk`, its
folder before Meko called it Kode, which is left as a link) moves there by itself after an update (`src/server/movehome.ts`, [recovery.md](docs/design/recovery.md#moving-the-home-into-mekos-data-folder)).

## Commands

```sh
cd src/extensions/agentk
nvm use 22
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++  # RHEL 8, for node-pty (not on macOS)
npm ci
npm run typecheck && npm run lint && npm test   # run all three before every commit
npm run build                                   # dist/server + dist/web
npm run dev                                     # the server on ./.dev-home/web.sock, Vite on :5173 proxying to it; state in ./.dev-home (KODE_DEV_HOME to change)
npm link                                        # after build: puts this build on PATH as me_kode, for Meko to use instead of a release
```

`package.json`'s `bin` (`me_kode`) is only for a build from source: `npm link` puts it on PATH,
where `meko extension install kode` finds it and uses it as it is, never updating it. People run
Kode through Meko (`meko kode <command>`), never by that name.

Requirements: Linux or macOS, Node 22 (`.nvmrc` pins the major), git, and a coding agent. node-pty
is a native module that needs a C++20 compiler: on RHEL 8 the system g++ (8.5) is too old, hence
gcc-toolset-13 above; on macOS, `xcode-select --install` is enough (and `brew install gh` for PRs). `npm ci`
also makes node-pty's macOS spawn-helper executable (`scripts/postinstall.mjs`).

The dev server is at http://127.0.0.1:5173. The server listens only on its unix socket, as it does
behind Meko, and Vite proxies `/api/`, `/healthz` and `/ws/` to it (`KODE_DEV_SOCK` names another
socket); the page has no `<base>` there, so it runs at `/` rather than `/ext/kode/`, and opens
without a login. Its state is in `./.dev-home` (`KODE_DEV_HOME` names another folder; `KODE_HOME` names
a home only without a release, as here and in tests), so it never opens an installed Kode's
database, hook socket or session host: it starts a session host of its own for that folder, which
keeps running when `npm run dev` restarts (stop `npm run dev`, then
`KODE_HOME=.dev-home npx tsx src/server/cli.ts host stop` ends it). To run a branch beside an
installed Kode on a fresh home, with the tour, and take it down afterwards, use the repo's
side-install skill (`.claude/skills/side-install/`). `npm test` is unit and
integration (`npm run test:unit`, `npm run test:integration`, which needs git and uses a stub
`claude`). `node:sqlite` prints an experimental warning on Node 22; the release wrapper runs Node
with `--disable-warning=ExperimentalWarning`.

## Map

Kode's top level, and the shared code both sides use. `src/server/` and `src/web/` each have their own
AGENTS.md with a row per module; Claude Code loads it when you work in that directory, and other
agents should read it before changing code there.

| Path | What it is |
|---|---|
| `src/shared/types.ts` | The API contract: every REST route, WebSocket message and shared type. Change it first. |
| `src/server/` | The server, the session host (`host/`) and the CLI. A row per module is in [src/server/AGENTS.md](src/server/AGENTS.md). |
| `src/web/` | The React + xterm.js UI. A row per module, and the rules every UI change follows (it must work on a phone), are in [src/web/AGENTS.md](src/web/AGENTS.md) and [src/web/REVIEW.md](src/web/REVIEW.md). |
| `src/shared/harnesses.ts` | The harnesses a session can run on (`claude`, `opencode`, `kiro`, in the order every list shows them), their labels, sign-in commands and what each can do (`HARNESS_CAPS`: the mod, Artifacts, tokens or credits, whether a by-model row names it, the oldest version sessions start on, the command that upgrades one older when its own update can't, whether Kode connects it to Meko by itself (`autoConnect`: Kiro and OpenCode), and the models New session offers: Claude Code's aliases, or `cli` for a list read from the CLI), which the server and the UI both gate on; `harnessOf()` reads a stored id; `checkModel()` (and `MODEL_MAX`) checks a model name before it's passed to an agent, and `modelLabel()` shows one by its label on the agent's list. |
| `src/shared/decisions.ts` | A design's Key Design Decisions (KDDs, `DesignQuestion`): numbering questions Q1… and Kode's own decisions D1… (`decisionNumbers`), which the user can answer (`answerable`: an open question, or a decision to change), and the marker a design puts after a decision that rests on Meko (`MEKO_SOURCE_MARK`, `MEKO_SOURCE_CLASS`). Used by the server (the prompts), the Questions tab and the design panel (`design-frame.ts`). |
| `src/shared/stages.ts` | The rules for moving a session by hand: where a task can move, what can't be moved, where the move dialog starts, the prompt each move sends by default, what a move does in words, skipped steps, and `stepOf` (the PR stage shows as Pull Request). Used by the server and the move dialog. Also the Starting view's steps, which name the session's agent (`startupSteps()`). |
| `src/shared/datapacks.ts` | Project datapack names (`adlc/proj/<repo>/<project>`), the description Kode writes on them (remote credentials stripped), matching one to a local repo, and the remote helpers `git.ts` uses too. Used by the server and the web UI. |
| `src/shared/worklinks.ts` | Links on work items (`parseWorkLink`: GitHub PRs and issues, Jira issues, Slack messages, other pages, each spelling of one thing under one key, and its chip's label; `workLinkKey` for the palette's lookup). Used by the server and the web UI. |
| `src/shared/codereview.ts` | The [code review](docs/design/code-review.md)'s shared part: its strategies (`STRATEGIES`, in the settings' order: Sirius 1.0, Sagittarius 3.0), the four settings (strategy, rounds, depth, check) and how a session's, its project's and the global values resolve (`resolveReview`, `readOverrides`), the PR description's `me_kode_review:` line with the strategy that ran (`meAkReviewLine`, `strategyTag`, its parser, `withMeAkReviewLine`), `CodeReviewSummary` (`Session.codeReview`) and the short label the review's part of a task's tab shows (`reviewSegmentText`, `reviewSegmentLabel`), whether a round's findings wait on the user (`reviewWaitsOnYou`, the `triage` state) and the user's note or dismissal of one (`FindingChoice`, `DISMISSED_BY_YOU`), and the findings panel's `CodeReviewDetail`. Used by the server and the web UI. |
| `src/shared/mekomd.ts` | Reading a repo's `MEKO.md` (its table of Datapack, ID and What's in it: `parseMekoMd`) and telling the gotchas datapack (`isGotchasDatapack`: a name ending in `/gotchas`). The code review lists them for its passes. |
| `src/shared/branches.ts` | The branch prefix setting's `{login}` (`expandBranchPrefix`: gh's login, or left out with its joiner when gh can't say) and the default, `ak_{login}/`. Used by the server and the web UI. |
| `src/shared/termcolors.ts` | The terminals' two themes' foreground and background (`TERM_COLORS`, which the web UI's palettes start from), `parseTermTheme`, and a color as an OSC 10/11 answer gives it (`oscColor`). Used by the web UI, the server (the colors sessions start with) and the session host (its answers). |
| `src/shared/termreports.ts` | Telling a terminal's answers to a program's queries from typing (`isTerminalReport`): the browser terminal and `meko kode attach` drop them, since the session host answers them. Also mouse motion with no button held and focus reports (`isPassiveReport`), which don't count as using a screen (`src/server/screens.ts`). |
| `src/shared/emails.ts` | The email check, the parsing of a list of emails for the Share dialog (commas, semicolons, whitespace, `Name <address>`), pasting into its field and picking a suggested address (`typedEntry`, `pickEmail`, `suggestContacts`), and the most one invite can send to. Used by the server and the web UI. |
| `src/shared/mekourls.ts` | Meko Cloud's MCP and web app URLs, the command that installs Meko on this computer (`MEKO_INSTALL_COMMAND`, for the banner shown while there's none), a datapack's share link on the web app Meko names (`shareLinkUrl`, and `shareLinkToken`, which reads a pasted one only from that web app), and Meko's own pages Kode links to (`MEKO_HOME_PATH`, its main page, from the projects pane's "m"; `mekoDatapackPath()`, a datapack's page, which datapack names link to: `mekoDatapackUrl()` in `src/web/util.ts`; `mekoGapsPath()`, its Context Gaps tab, from the project home's Meko line; Meko's web UI sends a Meko Cloud datapack's on to Meko Cloud's web app). Used by the server and the web UI, so Ask Kode offers to join only the links the server will. |
| `src/shared/tour.ts` | How the Kode tour's scoreboard says its numbers (`tokensText`, `sideText`, `scoreDifference`): the server fills a stage's card text with them (`src/server/tour.ts`) and the page shows them (`TourPanel.tsx`). |
| `src/shared/pricing.ts` | Anthropic API list prices per model and the cost of a set of tokens (cache writes by duration). |
| `src/mod/agentk/` | Kode's mod: a Claude Code plugin of function hooks loaded into every session with `--plugin-dir`. It polls the hook socket for Kode's prompts and submits them as the user's, reports the context fill and plan limits after each turn, forwards Claude's `AskUserQuestion` calls to Kode while the terminal's dialog stays open (the first answer wins), reports when each of Claude's Meko tool calls starts and ends (for the session's dot), reports a turn you interrupt (Ctrl+C, Esc: no `Stop` hook fires, `/mod/turn`), gives Claude the `design_questions` tool, runs learn steps (`$.model.fork`, then `$.model.complete` for the duplicate check), and registers `/kode_learn`. Compiled by Claude Code against its own declarations, not by the repo's tsc; checked with `claude plugin validate` / `claude plugin test src/mod/agentk` (`test/unit/mod.test.ts`, skipped without Claude Code). |
| `src/mod/agentk-view/` | Kode's view mod, loaded beside the mod with a second `--plugin-dir`: draws Kode's prompts in the terminal as their first line (ctrl+o shows them whole), with a `ui.render` hook on `UserMessage` rows. A plugin of its own because Claude Code never shows a plugin the rows of the prompts it submitted itself. It tells the session's first prompt (passed on the command line, drawn like typed text) by `KODE_FIRST_PROMPT`. Checked like the mod. See [docs/design/mod.md](docs/design/mod.md#how-kodes-prompts-look). |
| `src/tools/skill/SKILL.md` | The skill for Kode's session tools (the `kode_sessions` MCP server, `src/server/mcp.ts`): how an agent opens projects, starts sessions (from GitHub issues and PRs too), moves them and opens another session's PR. Its one source; the server copies it for every harness (`writeToolFiles` in `src/server/tools.ts`), and the build into `dist/tools`. See [docs/design/tools.md](docs/design/tools.md). |
| `src/workshops/` | The workshops bundled with Kode ([docs/design/workshops.md](docs/design/workshops.md)): each folder a `workshop.json` with its levels' stages (the tour Kode runs, `src/server/tour.ts`) and their card text, its levels' prompts, the demo repo's files (`repo/`, a small browser game the lint leaves out), a teammate's change as a patch and the gotchas for the demo's gotchas datapack; the Kode tour is `agentk-101`. Read by `src/server/workshops.ts`, copied into `dist/workshops` by `npm run build:workshops`. `npm run check:demo-review` (`scripts/check-demo-review.mjs`, by hand against a running Kode) measures that Level 1's trap is caught only with Meko. |
| `scripts/deps.sh`, `scripts/install.sh` | `deps.sh`: Kode's prerequisites, which `meko kode install` runs first (a release has it as `lib/scripts/deps.sh`): installs missing tools (git, curl, and a coding agent when none of Claude Code, OpenCode and Kiro CLI is there: it asks which, OpenCode second, or `--agent`) after asking (`--install-deps`, `--no-install-deps`; nothing with `--non-interactive` unless `--install-deps`), never offers gh (installed with `--install-deps`, skipped with a warning otherwise: Kode asks for it when something needs it, [lifecycle.md](docs/design/lifecycle.md#gh-on-demand)), and checks each agent's sign-in and gh's. `--only gh` installs gh alone, without root, as the GitHub dialog's **Install gh** runs it. Meko installs Kode (`meko extension install kode`: it unpacks the release, then runs `meko kode install`), so `install.sh` only says so: releases still carry it for the Mekos and instructions that run it. Both tested against stub tools in `test/unit/install-script.test.ts`. |
| `packaging/`, `scripts/`, `.github/` | Launcher wrapper (`packaging/me_kode`: runs the bundled Node, and hands `serve` and `host` to Meko's launcher, `meko _launch`, which runs a new version's probation and rollback), systemd and launchd templates, the prerequisites and release scripts, and (at the repo root, running from `src/extensions/agentk`) CI and releases (Linux and macOS) and Dependabot (`dependabot.yml`). The repo root's `scripts/check-title.sh` holds the commit subject and PR title rules, and `scripts/release/release-notes.sh` decides which types and components release each product (Kode or Meko, `--product`; `scripts/release/product.sh` names each one's tags and components); their type lists are the ones the tests check (the docs, the commit skill and What's new's `RELEASE_TYPES` in `src/server/releasenotes.ts` repeat them, so change those too), and both scripts are tested in `test/unit/check-title.test.ts` and against Dependabot's prefixes in `test/unit/dependabot.test.ts`; `test/unit/release-scripts.test.ts` tests what each product counts (versions, notes, pruning). The repo root's `scripts/ci-scope.sh` decides which components' CI jobs a PR runs; `test/unit/ci-scope.test.ts` tests it, checks that every root file a test here reads (`../../../../../…`) runs Kode's job, that the Markdown a Meko test globs or Kode's release packs runs its component's, and that the `CI gate` job needs every other job. |
| `test/` | Unit tests, and integration tests against a real session host (in the test process) and git with a stub `claude` (`test/fixtures/fake-claude.mjs`), and stubs of `kiro-cli` and `opencode` (`fake-kiro.mjs`; `fake-opencode.mjs`, which runs Kode's real OpenCode plugin as OpenCode 2 does). |
| `docs/` | The design: [docs/design.md](docs/design.md) is its index (the decisions so far, and which part covers what), with one file per area under `docs/design/`. Also every feature ([docs/features.md](docs/features.md)), the install details and every command ([docs/install.md](docs/install.md)), the usage report's schema (`call-home-report.schema.json`) and the screenshots. |
| `MEKO.md` | The Meko datapacks that hold context for Kode, beside the repo root's [MEKO.md](../../../MEKO.md). Search them before any work here. |

## Invariants (don't break these)

1. **Sessions survive the server.** The session host runs in its own service (`me_kode-host.service`
   on Linux, the `com.yugabyte.me_kode.host` LaunchAgent on macOS). Never start the host or a session's
   Claude as children of the server's service or process group (one-shot `claude` commands that end with
   their request, such as `--version`, `update` and Ask Kode's `claude -p`, are the server's own). Server restarts and updates must never stop a
   session. Manage the services only through `service.ts`. When the host service is installed, the server
   never starts a host itself; it reports the service as down. **The one exception** is the move of an
   older home into Meko's data folder (`src/server/movehome.ts`, decided by the project owner for
   #236): once, right after the release that brings it proves healthy, its job (outside both services)
   stops the server and the host, ending every session whatever it's doing, and recovery resumes them
   in the new home.
2. **Recovery is idempotent.** `recover()` and `reconcile()` may run at any time. A session with a live
   pane is left alone, and so is one still being created or launched (it has no pane yet); a missing
   pane is relaunched with `claude --resume`, except a session whose agent Kode stopped for sitting
   idle (`sessions.idle_stopped_at`), which stays stopped until something needs it; auto-continue is guarded (once per 15 minutes; after two
   auto-continues in an hour the next one is parked). Archive and project delete wait for a session
   that is still starting; `launch()` never replaces a live pane.
3. **Only through Meko.** The server listens on nothing but its unix socket (`Paths.webSock`, in a
   folder only the user can open) and has no login of its own: the Meko on this computer serves it
   to browsers at `/ext/kode/` after its own access check (local or remote access, no login), and
   every request on the socket is let in. Never add a TCP listener, a login, or a route that trusts
   a header to say who sent it. Writes and WebSocket upgrades refuse an `Origin` that isn't the
   page's own (Meko passes the browser's `Host` and `Origin` on unchanged), keyed on the matched
   route (`req.routeOptions.url`), never the raw URL, which can be percent-encoded. Hooks use their
   own unix socket with a per-session token.
4. **No shell interpolation of user input.** Use `execFile`/`spawn` with argument arrays. The only
   shell text we generate is the session launcher script and the LaunchAgents' launcher
   (`writeServiceLauncher`), and every value in them goes through `shq()`.
5. **Migrations are append-only.** Add a new entry to `MIGRATIONS` in `db.ts`; never edit a shipped one.
6. **The session host outlives updates.** A running host keeps the code it started with, and restarting
   it ends every Claude: nothing restarts it by itself, except the one-time move of the home into Meko's
   data folder (invariant 1). Keep `src/server/host/` small; bump `PROTOCOL` in
   `host/protocol.ts` for a change an older peer can't read, and keep the server working with hosts back
   to `MIN_PROTOCOL`. Meko installs updates and deletes old versions, but never the folder a running
   host started from: the launcher starts `host` through Meko's launcher (`meko _launch`), which
   holds a lock on that folder while the host runs.
7. **Meko is the Meko on this computer; a project has at most one dedicated datapack.** It can't be
   turned off, and Kode doesn't know or ask whether it's linked to Meko Cloud: no code branches on
   local or Meko Cloud. Meko owns the link (its web UI's Settings › Meko Cloud, `meko cloud-link`)
   and its key, and its `/mcp` and its `/api/v1` (on the same origin, `mekoApiBase`) serve its own
   datapacks and pass any other id on to Meko Cloud while it's linked; what it can't do (sharing
   while unlinked) it refuses with an error that says what to do, which Kode shows. Kode reads
   that Meko's URL and token, and its link's account and web app, from `meko token --json`
   (`mekolocal.ts`), keeps them in memory and never writes them to `config.toml`, and has no route or
   UI that sets a key. It reads no key of its own (`config.toml`'s old `[meko]` keys, `MEKO_*`,
   Claude Code's settings). The token goes only to that Meko's own URL. With no Meko running,
   Kode doesn't talk to Meko: sessions get no datapack, pin or Meko
   lines in their prompt, and nothing is saved there. Each session gets its own
   `MEKO_AGENT_ID` (`sessionAgentId()`: `claude_code:<slug>-<hash>`, or `opencode:…` or `kiro:…` for an OpenCode or Kiro session, never the project id), with
   the project's dedicated datapack pinned for it when it starts; Kode's writes for a session use
   the id it runs with (`launchedAgentId()`). Never remove a `pin-claude_code_<project id>.json`:
   it may be the user's own pin for plain Claude Code in that repo. (Claude Code's own
   Meko setup is the user's and is changed only when they ask: the installer's question, or
   **Connect to Meko** or **Set up Claude Code with this Meko** in Settings › Harness.) Meko owns
   the agents' connections (their `meko` MCP servers): Kode's server only reads Meko's list of
   agents (`mekoagents.ts`) and never posts to Meko's agent routes; the page does, with the
   owner's sign-in. New sessions don't start on agents that list says aren't connected (`none`,
   `stale`, `elsewhere`; one it doesn't list stays available); with no list
   ever read, or a Meko too old to give one, nothing is gated, and while Meko doesn't answer the
   last list read stands. The page also connects Kiro and OpenCode by itself, once each, while Meko
   reports them `none` or `stale` (`MekoAutoConnect`; Claude Code never). Nothing that needs Meko may stop the install, a project or a session when there's no Meko or
   it's down: it's skipped or offered later.
8. **The repo is public.** No internal hostnames, mount paths, tokens or
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
  Kode runs the workflow, so text about the work (designing, coding, reviews, turns) names
  **Kode**; **Claude** is only for the Claude Code process and its terminal (starting, exits,
  Resume, typing into it), and for an OpenCode or Kiro session that text names it (`agentName()`). Prompts sent to Claude still address Claude.
- The server is the source of truth; the UI renders `AppState` pushed over `/ws/events`.
- Tests: new behavior gets a test, and only one: before adding a test, look for one that already
  covers the path and extend it (cases of one rule go in one `it.each` table). Don't test
  libraries, constants or copy, and don't assert internals where the visible result would do;
  REVIEW.md §6 flags redundant tests. Integration tests use a real session host and git; GitHub
  is mocked with `vi.mock('../../src/server/github.js')`. Tests must never touch the real
  `~/.claude.json` or Meko (see `test/helpers.ts`); `test/setup.ts` runs first in every test file, scrubs `MEKO_*`, `KODE_*`, `AK_*` (its variables before Kode was called Kode),
  `CLAUDE*`, `KIRO_*`, `OPENCODE*`, `GH_*`, `GIT_*` and `TMUX*` from the environment and points `HOME` at an empty
  folder. `npm run typecheck` covers the tests too (`tsconfig.test.json`).
- Commits and PR titles: `<type>(<component>[/<area>])[!]: <summary>` with one of nine types and,
  for Kode, the component `agentk` (`feat(agentk/web): …`; `feat(agentk,meko): …` for a change
  that ships in Meko too, which releases both), checked by
  the repo root's `scripts/check-title.sh` (see the repo root's `.claude/skills/commit/SKILL.md`).
  `feat`/`fix`/`perf`/`deps` commits trigger a release (`1.YYYYMMDD.N`) when pushed to `main`;
  `ci`/`test`/`docs`/`nit`/`chore` don't. PRs are squash-merged, so the PR title is the subject;
  the `PR title` workflow (`pr-title.yml`) checks it. Changes reach `main` only through PRs that
  pass CI, that check and the CLA check (`license/cla`), and that the `meko-maintainers` team
  approves (the `approvals` ruleset).

## Keep the docs current

Docs are part of the change. Update them **in the same commit** as the code:

| When you change… | Update |
|---|---|
| Behavior, architecture, lifecycle, security or data model | The part of `docs/design/` that covers it (a new part gets a row in the contents table of `docs/design.md`) |
| Features, requirements, install steps | `docs/features.md` (every feature) and `docs/install.md` (install details and every command), and `README.md` when it changes anything the README states (a major feature, the requirements, the install steps, linking Meko Cloud, who can open Kode, or a common command): the README stays short (the repo root's README is Meko's, and points here) |
| Dev setup, checks | **Commands** above |
| Release process, Dependabot's rules | [docs/design/releases.md](docs/design/releases.md) |
| Module layout, invariants, commands, conventions | `AGENTS.md`: this one, or `src/server/AGENTS.md` or `src/web/AGENTS.md` for a module there (the repo root's for the repo layout and repo-wide rules) |
| What reviewers should check | `REVIEW.md` |
| An API route or message | `src/shared/types.ts` (the route list at the bottom) |
| A config key | `DEFAULT_CONFIG` in `src/server/config.ts` and the `config.toml` excerpt in `docs/design/projects-and-sessions.md` |

Before you finish, re-read the docs you touched against the code; stale docs are treated as bugs in
review.
