# AgentK Local

The agentic coder for ADLC. AgentK Local helps you work across many projects and
[Claude Code](https://claude.com/claude-code) sessions at once, with [Meko](https://mekodata.ai)
as its data layer. It's self-hosted, runs on your devserver (headless) or your Mac, and you use it
from a browser.

The design is split by area. Read this page first, then the part you need:

| Part | Read it for |
|---|---|
| [Architecture](design/architecture.md) | The server, the session host, hooks, the state directory, and how they fit together |
| [AgentK's mod in Claude](design/mod.md) | The Claude Code plugin AgentK loads into sessions: prompts, questions, Meko calls, learn steps |
| [The app](design/app.md) | The UI: layout, Changes, review comments, side panels, Starting, keyboard, notifications, the phone layout |
| [Open project and New session](design/open-project-and-new-session.md) | The two dialogs that start work |
| [Design panel](design/design-panel.md) | Showing a design, its open questions and the approval |
| [Project home](design/project-home.md) | The project's home page |
| [Projects, worktrees and sessions](design/projects-and-sessions.md) | What a project and a session are, the layout on the host, `config.toml`, the schema, worktree reuse |
| [Task lifecycle](design/lifecycle.md) | Stages and gates, queued gates, the design after approval, moving a session, instructions with a move |
| [Reviewing a PR](design/review-pr.md) | Review a PR sessions |
| [GitHub links, Ask AgentK and finding a session](design/palette.md) | Starting from a pasted GitHub link, Ask AgentK and finding a session by what it's about in the command palette |
| [Datapacks](design/datapacks.md) | Meko datapacks: dedicated and shared, pins, invites, adding files to the knowledge base |
| [Sharing a project](design/sharing.md) | Sharing a project's datapack with teammates and reviewers |
| [Meko recall and the learn step](design/recall-and-learn.md) | What Meko gives a session at start, and what it saves at the end |
| [Token usage](design/token-usage.md) | Reading transcripts, totals and costs, Meko's savings |
| [Usage stats](design/usage-stats.md) | The anonymous usage report |
| [Login and security](design/security.md) | Login, TOTP, TLS, local access, origin checks |
| [Surviving crashes, restarts and updates](design/recovery.md) | Recovery, auto-continue, one server per home |
| [Releases and auto-update](design/releases.md) | Release builds, the installer, self-update and rollback |

Below: [Decisions so far](#decisions-so-far) · [Tech stack](#tech-stack) · [Milestones](#milestones) ·
[Open questions](#open-questions)

## Decisions so far

| Topic | Decision |
|---|---|
| Session UI | Embed the real Claude Code TUI (xterm.js over WebSocket). Don't rebuild Claude's interface. |
| Process host | AgentK's own session host (`agent-k-local host`): one Node process that holds every Claude in a pty, in its own background service (a systemd user unit on Linux, a LaunchAgent on macOS), so the server can restart or update without touching sessions. No tmux. |
| Open project | Pick a repo from a list of the git repos under `~/code`. The folder is configurable, and you can clone a repo into it from a URL or start a new one there. |
| Local state | SQLite (`~/.agent-k-local/state.db`), WAL mode, schema versioned with `PRAGMA user_version`. |
| Working copy | Each session gets its own git worktree on its own branch. When it ends, a clean worktree waits, build outputs and all, for the next session on the repo. |
| Datapacks | Each project gets one dedicated datapack (the write target) and can attach any number of shared datapacks (read targets). |
| Meko | Optional. With a key, projects get a dedicated datapack (or start without one, when Meko is down or the plan's datapack limit is reached, and get one later). Without a key, or turned off in Settings, AgentK works without datapacks. |
| Meko wiring | Each session gets its own `MEKO_AGENT_ID=claude_code:<session slug>-<hash>`, and the project's datapack is pinned for that agent id when it starts. The Meko hooks already honour both, so no plugin change is needed. |
| Datapack names | New dedicated datapacks default to `adlc/proj/<repo>/<project_name>`. Datapacks with the `adlc/proj/` prefix are *project datapacks*: offered when shared with you, and filled in automatically. |
| Project sharing | A project is shared by sharing its dedicated datapack in Meko, from the project home or, for a PR's reviewers, from its PR & Review step. A project datapack shared with you shows a banner and a notification, and opens as a new project with its repo and name filled in. |
| Reviewing a PR | A **Review a PR** session checks out someone else's PR, in any repo, and AgentK assists the user who reviews it: a short summary to get them oriented, then answers to what they ask, with the PR's design (named in its description, fetched from Meko) open beside it and Meko searched for the decisions behind it. It posts to GitHub only when the user says. |
| Runtime | Node 22 (bundled in releases). State uses the built-in `node:sqlite`; node-pty is the only native module. |
| Network | Reachable directly on the corp network over HTTPS, behind a login; or local only, listening on 127.0.0.1 with no login. |
| Other terminal sessions | Not adopted. Only sessions created by AgentK Local are shown. |
| Crash behaviour | After a host reboot, the server automatically brings back every open project and session with `claude --resume`. Sessions that were mid-turn are told to continue. |
| Token usage | Tracked per session and per project (per model and per day too), read from Claude Code transcripts including subagents, with an estimated cost at API list prices. Meko calls are counted from the same transcripts, with an estimate of what Meko searches saved. |
| Usage stats | Each install sends anonymous counts (projects, sessions, PRs, commits, file types, tokens by model) once a day, hourly in a new install's first day, to a collector the AgentK Local team runs as a separate service. On by default; one checkbox in Settings → Account turns it off. |
| Task lifecycle | Explore → Design → Code → PR & Review (AgentK) → Merge & Close, with three human gates. A task starts at Explore, Design or Code, and you can move it between stages by hand; a Review a PR session has none of it. More ADLC stages plug in later. |
| Host OS | Linux devservers, and Macs on Apple Silicon (macOS 14 or newer) while you're logged in to the desktop. Intel Macs and headless Macs aren't supported. |
| Distribution | Public GitHub repo. GitHub Actions for CI and releases, date-based versions (`0.YYYYMMDD.N`), tarball releases, and an update check built into the server. |
| Repo & license | Internal at `github.com/yugabyte/agent-k-local` for now, to be made public later. Apache-2.0. |
| Sharing | Everyone installs their own copy. One owner per install, with no shared instances. |
| Client | Browser first. Chrome's "Install app" gives it a dock icon: the web app manifest (`manifest.webmanifest`) names it "AgentK Local" and supplies the logo as regular and maskable icons. `/favicon.ico` and `/apple-touch-icon.png` are served at the root too, for browsers and phones that look there instead of reading the page's links; a missing file is a 404, never the app page. A native Mac wrapper is deferred. |

## Tech stack

| Layer | Choice | Why |
|---|---|---|
| Server | Node 22 + TypeScript, Fastify, `ws`, `node-pty` | node-pty builds on the target host. Using one language across server and web keeps it simple. |
| Web | Vite + React, xterm.js with the fit and web-links addons | xterm.js is the standard browser terminal. React handles the rail, tabs and dialogs. |
| State | SQLite via the built-in `node:sqlite`, WAL, `user_version` migrations | A synchronous API suits a single-writer server, and it needs no native module. |
| Processes | AgentK's session host (Node and node-pty, with a headless xterm per pane), in its own background service (a systemd user unit on Linux, a LaunchAgent on macOS) | It keeps sessions alive when the server restarts, with nothing to install beyond what AgentK ships, and you can still attach from iTerm. |
| Integrations | Meko MCP over HTTP (optional) and its REST API for sharing, Claude Code hooks, git worktrees | Meko holds projects' datapacks when it's on. The hooks and git are thin adapters. |
| Release | GitHub Actions, date-based versions, artifact attestations | Versioning and supply-chain checks with no extra tooling. |

*Escape hatch:* `ssh host -t agent-k-local attach fix-retry-backoff` attaches to any session from
iTerm (`Ctrl+\` detaches).

## Milestones

- **M0 · Repo.** Git repo, the commit skill without agent attribution, CLAUDE.md, a license, the CI
  skeleton, and publishing to GitHub.
- **M1 · Core.** The server with login and TLS, a Meko connection check, the New project repo
  picker with a mandatory dedicated datapack, worktree-backed sessions (then in tmux) launched with the
  Meko env vars, and the browser terminal, rail and tabs. Folder trust for worktrees is handled
  automatically.
- **M2 · Status and durability.** Hooks drive the badges and notifications. The two systemd units,
  recovery on boot with guarded auto-continue, and resume for exited sessions. (A "Meko
  unreachable" banner, fed by a Meko check every minute, was dropped later: an action that needs
  Meko says so when it fails.)
- **M3 · Task lifecycle.** Stages and gates, the design panel, opening the PR, the AgentK review
  loop, merge, and cleanup of worktrees and branches.
- **M4 · Shared datapacks.** Attaching shared datapacks, `MEKO_DATAPACK_IDS` and the system-prompt
  listing, and the project home page with activity and search.
- **M5 · Ship it.** The release pipeline, `install.sh`, the update loop with rollback, and making
  the repo public.
- **M6 · Polish and next stages.** The command palette, a diff view per session, passkeys, more
  lifecycle stages (test, deploy, monitor), and macOS support.

## Open questions

1. **Where approved designs live.** Only in the datapack, or also committed to the repo (for
   example `docs/designs/`) as part of the PR?
2. **Automatic gates.** Should a project be allowed to make **Merge** automatic once AgentK has no
   open findings and CI is green?
3. **Stacked PRs.** Should a session be able to produce a stack with `gh stack` instead of a single
   PR?
