# AgentK Local

The agentic coder for ADLC. AgentK Local helps you work across many projects and
[Claude Code](https://claude.com/claude-code) sessions at once, with [Meko](https://mekodata.ai)
as its data layer. It's self-hosted, runs on your devserver (headless) or your Mac, and you use it
from a browser.

**Contents:** [Decisions](#decisions-so-far) · [Architecture](#architecture) · [The app](#the-app) ·
[Projects & sessions](#projects-worktrees-and-sessions) · [Task lifecycle](#task-lifecycle) ·
[Datapacks](#datapacks) · [Token usage](#token-usage) · [Usage stats](#usage-stats) · [Login & security](#login-and-security) ·
[Crash recovery](#surviving-crashes-restarts-and-updates) ·
[Releases & updates](#releases-and-auto-update) · [Tech stack](#tech-stack) ·
[Milestones](#milestones) · [Open questions](#open-questions)

## Decisions so far

| Topic | Decision |
|---|---|
| Session UI | Embed the real Claude Code TUI (xterm.js over WebSocket). Don't rebuild Claude's interface. |
| Process host | tmux on a dedicated socket, in its own background service (a systemd user unit on Linux, a LaunchAgent on macOS), so the server can restart or update without touching sessions. |
| Open project | Pick a repo from a list of the git repos under `~/code`. The folder is configurable, and you can clone a repo into it from a URL or start a new one there. |
| Local state | SQLite (`~/.agent-k-local/state.db`), WAL mode, schema versioned with `PRAGMA user_version`. |
| Working copy | Each session gets its own git worktree on its own branch. When it ends, a clean worktree waits, build outputs and all, for the next session on the repo. |
| Datapacks | Each project gets one dedicated datapack (the write target) and can attach any number of shared datapacks (read targets). |
| Meko | Optional. With a key, projects get a dedicated datapack (or start without one, when Meko is down or the plan's datapack limit is reached, and get one later). Without a key, or turned off in Settings, AgentK works without datapacks. |
| Meko wiring | Each session gets its own `MEKO_AGENT_ID=claude_code:<session slug>-<hash>`, and the project's datapack is pinned for that agent id when it starts. The Meko hooks already honour both, so no plugin change is needed. |
| Datapack names | New dedicated datapacks default to `adlc/proj/<repo>/<project_name>`. Datapacks with the `adlc/proj/` prefix are *project datapacks*: offered when shared with you, and filled in automatically. |
| Project sharing | A project is shared by sharing its dedicated datapack in Meko, from the project home or, for a PR's reviewers, from its PR & Review step. A project datapack shared with you shows a banner and a notification, and opens as a new project with its repo and name filled in. |
| Reviewing a PR | A **Review a PR** session checks out someone else's PR, in any repo, and AgentK assists the user who reviews it: a short summary to get them oriented, then answers to what they ask, with Meko searched for the design and decisions behind it. It posts to GitHub only when the user says. |
| Runtime | Node 22 (bundled in releases). State uses the built-in `node:sqlite`; node-pty is the only native module. |
| Network | Reachable directly on the corp network over HTTPS, behind a login; or local only, listening on 127.0.0.1 with no login. |
| Existing tmux sessions | Not adopted. Only sessions created by AgentK Local are shown. |
| Crash behaviour | After a host reboot, the server automatically brings back every open project and session with `claude --resume`. Sessions that were mid-turn are told to continue. |
| Token usage | Tracked per session and per project (per model and per day too), read from Claude Code transcripts including subagents, with an estimated cost at API list prices. Meko calls are counted from the same transcripts, with an estimate of what Meko searches saved. |
| Usage stats | Each install sends anonymous counts (projects, sessions, PRs, commits, file types, tokens by model) once a day, hourly in a new install's first day, to a collector the AgentK Local team runs as a separate service. On by default; one checkbox in Settings → Account turns it off. |
| Task lifecycle | Explore → Design → Code → PR & Review (AgentK) → Merge & Close, with three human gates. A task starts at Explore, Design or Code, and you can move it between stages by hand; a Review a PR session has none of it. More ADLC stages plug in later. |
| Host OS | Linux devservers, and Macs on Apple Silicon (macOS 14 or newer) while you're logged in to the desktop. Intel Macs and headless Macs aren't supported. |
| Distribution | Public GitHub repo. GitHub Actions for CI and releases, date-based versions (`0.YYYYMMDD.N`), tarball releases, and an update check built into the server. |
| Repo & license | Internal at `github.com/yugabyte/agent-k-local` for now, to be made public later. Apache-2.0. |
| Sharing | Everyone installs their own copy. One owner per install, with no shared instances. |
| Client | Browser first. Chrome's "Install app" gives it a dock icon: the web app manifest (`manifest.webmanifest`) names it "AgentK Local" and supplies the logo as regular and maskable icons. `/favicon.ico` and `/apple-touch-icon.png` are served at the root too, for browsers and phones that look there instead of reading the page's links; a missing file is a 404, never the app page. A native Mac wrapper is deferred. |

## Architecture

There are three long-lived pieces on the host: the web server, the tmux server that owns every
Claude process, and the state directory at `~/.agent-k-local`. The server is stateless enough to
be killed at any time. tmux and the on-disk state are what carry your work.

```mermaid
flowchart LR
  subgraph Mac["Your laptop"]
    B["Browser or installed web app"]
  end
  subgraph Host["Devserver (headless) or your Mac"]
    S["AgentK Local server<br/>HTTPS :7777"]
    H["hook.sock<br/>unix socket"]
    T["tmux server<br/>own service (systemd or launchd)"]
    C1["claude in worktree A"]
    C2["claude in worktree B"]
    ST[("~/.agent-k-local<br/>state, certs, app versions")]
    U["updater"]
  end
  M["Meko MCP"]
  GH["GitHub Releases"]
  B -- "HTTPS + WebSocket, cookie auth" --> S
  S -- "node-pty runs tmux attach" --> T
  T --> C1
  T --> C2
  C1 -- "Claude hooks" --> H
  C2 -- "Claude hooks" --> H
  H --> S
  S --- ST
  S -- "datapack list and create" --> M
  C1 -- "capture into MEKO_DATAPACK_ID" --> M
  U -- "poll, download, verify" --> GH
```

**Terminal path.** When you open a tab, the browser opens a WebSocket. The server spawns
`tmux -L agentk attach -t ak-…` in a pty and pipes it through. Closing the tab kills only that pty,
and Claude keeps running. tmux has no prefix key, so Ctrl+B reaches Claude Code and tmux's own
commands can't be typed from the terminal.

**Status path.** Each session starts with `claude --settings` pointing at an agent-k hooks file.
The `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PermissionRequest`,
`Notification`, `Stop` and `SessionEnd` hooks (and the status line relay) post to a unix socket,
which drives the badges in the rail. Your global Claude settings stay untouched. When `/clear` (or
`/resume` inside Claude) switches conversations, `SessionStart` carries the new session id, and
Resume and recovery continue that conversation. `SessionStart` has a second hook, `context.sh`,
which asks the socket's `/context` route for context to start with and prints it for Claude Code
(what AgentK's Meko search found: see [Meko recall at start](#meko-recall-at-start)). Like the
edit guard, it prints nothing when the server can't be reached, and its curl gives up after 4 seconds.

The hook forwarder gives up quietly when the socket isn't there, so the server makes sure it is.
It binds the socket under a temporary name and renames it into place, removes it when it stops
only if the file is still its own, and every 10 seconds binds it again if the file was removed or
replaced (see [One server per home](#one-server-per-home)).

### AgentK's mod in Claude

Claude Code runs *mods*: plugins of TypeScript function hooks that run inside the Claude process
(early access). AgentK Local ships one, `agentk` (`src/mod/agentk`, a couple of hundred lines), that
gives the server a way into each session besides the terminal. The server copies it to
`run/mod/agentk` at startup, writing only files that changed, each through a temp file renamed into
place; when a source file is missing or a write fails, it removes the run copy, so sessions start
without the mod rather than with an old or half-updated one. Claude reloads a `--plugin-dir` mod whenever its files change, so an update reaches running
sessions without a relaunch, and a plain restart reloads nothing. Every launch adds
`--plugin-dir run/mod/agentk` and `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1`, since hooks modules of
installed plugins are otherwise off unless Claude Code's own rollout turns them on. That variable
only helps while the rollout hasn't decided: when its switch serves off, Claude loads no plugin's
hooks module whatever the variable says, and AgentK types prompts as before. It does this when:

- `[claude] mod` is on (the default);
- the mod's files are there;
- `claude --version` (read at startup, then at most every 10 minutes) is 2.1.286 or later. Older
  versions frame a plugin's prompt as the plugin's message, not the person's.

**Claude Code's version.** AgentK installs Claude Code with Anthropic's installer (`install.sh`) and
never updates it; Claude Code updates itself, unless that's turned off or it hasn't run for a while.
`ClaudeCode` (`claudecode.ts`) keeps the version read above and puts it in `AppState.claudeCode`,
with whether it runs the mod. A running session keeps the version it started with until Claude
restarts in it, so what matters is the version the next launch gets. While `[claude] mod` is on and
that version is older than 2.1.286, a banner says so, and that AgentK types its prompts instead:

- **Update Claude Code** runs `claude update`, Claude Code's own updater (`POST /api/claude/update`,
  one at a time, stopped after 5 minutes), then reads the version again;
- the outcome shows as its own banner: updated (running sessions keep their version until Claude
  restarts in them), nothing newer found (its release channel or the organization's settings may
  hold it back), or failed, with the last lines it printed and what to run on the host instead;
- each can be dismissed (the too-old banner once per version, in the browser's storage).

When Claude Code's rollout switch keeps mods off despite a new enough version, nothing shows: an
update can't fix it, and the switch flips back on by itself.

The mod reaches the server on the hook socket, with the session's `AK_HOOK_SOCK` and
`AK_HOOK_TOKEN`, like the hook scripts. Its routes answer with `Connection: close`, so a mod that
keeps polling on one connection can't hold the socket open. Before the socket closes (a stop, or a
rebind after its file went missing) the server answers every held poll and holds none until it's
bound again, so no hook is kept out for the 20 seconds a held poll would take:

| Route | What it does |
|---|---|
| `/mod/hello` | Sent once per load, from `session.start`, with the mod's protocol: 2 runs learn steps, so a session still running a mod from before them gets the typed learn prompt instead. |
| `/mod/poll` | A loop the mod keeps going with `$.clock.after` (a reload drops the old module's timers, so only the new loop polls). The poll carries the ids of the prompts the mod submitted that Claude still holds (`{ held: [...] }`). The server holds each poll up to 20 seconds and answers the prompts it has for the session (`{ prompts: [{ id, text }] }`, with `learn: { id, prompt }` when a learn step is waiting), or 204. A newer poll answers the one before it with nothing. Unknown tokens get 403; a closing socket, or `[claude] mod` turned off, 503. The mod waits 3 seconds after any failure. |
| `/mod/ack` | `accepted` for each prompt as it arrives. The server answers 204 when it still wants the prompt, and only then does the mod hand it to `$.prompt.submit({ text, asUser: true })`, so the model reads it as the person's own words; 410 (it gave up on the prompt) or no answer, and the mod drops it. `dropped` with a reason, and whether it filled the prompt box, when another plugin's `prompt.submit` hook refused it. |
| `/mod/measure` | From `session.measure` after each turn: the context window's fill, and the plan limits. |
| `/mod/held` | `{ held: [...] }`, as on a poll, as soon as it changes: a held prompt's `$.prompt.submit` resolved (its turn started) or another plugin refused it. |
| `/mod/ask` | From a `tool.call` hook on Claude's `AskUserQuestion`: `{ id, questions }` (the call's `tool_use_id` and its questions). The server holds it up to 20 seconds and answers the user's answers by question index (`{ answers: { 0: "Blue" } }`), or 204, and the mod asks again; 410 when it dropped the ask, 400 (`{ error }`) for questions it can't show. Either way the mod then leaves the question to the terminal's dialog. |
| `/mod/ask/done` | `{ id }`: the call has its answer, from AgentK Local or the terminal, or ended. |
| `/mod/questions` | From the `design_questions` tool the mod gives Claude: the design's questions, checked and saved (`{ text }` for Claude), or what's wrong (400, `{ error }`). |
| `/mod/learn` | A [learn step](#the-learn-step)'s results, `{ id, step, model, text \| reason, usage }`: what the fork of the session found (`step: "facts"`), answered with the duplicate check to run (`{ prompt, model }`) or `{ done }`; then that check's reply (`step: "decisions"`), answered `{ done }` with what AgentK saved, which the mod shows as a toast. 410 for a step AgentK gave up on. |
| `/mod/learn/start` | From `/agentk_learn`, a slash command the mod registers: starts a learn step, answered `{ text }` (the command's output: started, or why not). |

**Prompts.** `submit()` (`sessions.ts`) sends a prompt through the mod while its link is live:

- live means the mod is polling, or polled in the last 10 seconds (`modlink.ts`), and `[claude] mod`
  is on. `config.toml` is read when the server starts, so turning it off takes effect at the next
  server restart; from then on prompts are typed, for running sessions too, though those keep the
  mod (and `CLAUDE_CODE_ENABLE_FUNCTION_HOOKS`) until they're relaunched;
- the send resolves when the server answers the `accepted` ack with 204;
- the engine holds the prompt until Claude is idle, so it never lands in a permission dialog, a
  menu or a running turn, and a long one arrives whole rather than as `[Pasted text]`;
- the transcript row is a plain user message (it adds an `origin` naming the plugin), and the
  `UserPromptSubmit` and `Stop` hooks fire as for a typed prompt, so status and usage work as before.

Without a live link the prompt is typed into the terminal as before (tmux bracketed paste, then
Enter). A prompt is never sent both ways:

- once handed to the mod, a send that isn't acked within 5 seconds fails, as a failed paste does,
  and the caller keeps the prompt queued; a late ack gets 410, so the mod never runs it;
- a relaunch, `SessionEnd`, archive, project delete or stop resets the link and fails what was on
  its way, and those prompts' acks get 410 too;
- the one gap left is on the other side: if the 204 answering an ack never reaches the mod, the
  prompt counts as sent but doesn't run. That costs a prompt, never a duplicate;
- a `dropped` ack comes after the send resolved, so the caller already counts the prompt as sent.
  The session goes to **Needs you**, naming the prompt (its first line) and the plugin's reason.
  When Claude's prompt box was empty, the mod put the prompt's text there, and the detail says to
  press Enter in the terminal to send it; otherwise it says to ask AgentK for it in the terminal.

The links are in memory only. After a server restart the mod's next poll brings its link back
within seconds, and the prompts AgentK queues are in the database anyway. Whether a session's link
is live is in `Session.modLive`: the server pushes a change when the mod is first heard from, on a
reset, and when a sweep every 2 seconds finds a link gone stale. The UI offers what needs the mod
(below) only while it's live, and the server checks again on every request.

**Claude's questions.** The mod's `tool.call` hook on `AskUserQuestion` lets Claude's own dialog
open in the terminal as always (`next(e)`), and sends the questions to the server too
(`/mod/ask`, asked again after each hold). Whichever is answered first wins: the terminal's answer
is returned as it is; AgentK Local's comes back as the tool's result,
`{ questions, answers: { "<question>": "<answer>" } }`, which closes the terminal's dialog (checked
on Claude Code 2.1.286, the oldest that loads the mod, and on 2.1.287 and 2.1.291; so is the
`design_questions` tool below). The hook's `.catch` falls back to `next(e)`, and any failure
leaves the dialog to answer alone. On the server (`modlink.ts`) an ask lives only while Claude's
call waits: in `Session.asking` (`AskRequest`), with no copy in the database. It's dropped when the
mod says it has its answer (`/mod/ask/done`), when the mod stops asking for 10 seconds (the call
was interrupted, or Claude died), at the turn's end (`Stop`), and on a reset; and since the mod
sends the questions with every ask, a server restart brings the ask back with the next one. Only
choice questions (1 to 4, each with options) are offered: a question of another kind (Claude
Code's `text` and `number` questions) is left to the terminal. Text is capped for the browser (the
question 1,000 characters, a label 200, a description 1,000, a preview 8,000); labels are kept
whole, since the answer names them.

The session goes to **Needs you** with *AgentK asks: <first question>*, and you're notified once;
the classic `PermissionRequest` hook for `AskUserQuestion`, which Claude raises as well, uses the
same words (without the mod too). The questions show:

- in the design's **Questions** view, under **AgentK is asking**, at the top (its tab counts them);
- in any stage, in a bar above the terminal, *AgentK asks: …* with **Answer**, and on a phone also
  as the ⋯ sheet's first row (with a dot on ⋯). **Answer** opens the questions in a dialog that
  fills the window (a full-height sheet from the bottom on a phone): the questions scroll and
  **Answer** stays at the bottom. Nothing opens by itself: you may be answering in the terminal.

Each question is a card: its header, the options (radio rows, or checkboxes for a multi-select
question, each with its description and its preview as preformatted text with a **Wrap lines**
toggle for lines too long to fit: off at first on a desktop so tables and code stay aligned, on at
first on a phone; in the dialog a preview grows to 60% of the window's height before it scrolls,
40% on a phone), and **Other** with a
text box, which is the dialog's "Other". `POST /api/sessions/:id/ask/:askId` checks the answers
against the questions (labels must be the question's, one unless it's multi-select, the text
within 10,000 characters) and hands them to the mod: the labels comma-joined, then the text, as the
dialog gives them. A 409 means AgentK has moved on: the terminal answered first, or the turn ended.

**Answers and approvals mid-turn.** Mid-turn means a turn is running (`Session.midTurn`): from its
`UserPromptSubmit` to its `Stop` (or Claude's end), while AgentK works or Claude waits on a
permission prompt or a question during it. A Needs you with no turn behind it (a parked recovery,
a prompt another plugin refused) isn't, and nor is a design waiting for approval; after a server
restart only a session showing as working counts. While the mod is live, the design's **Send for
revision** works mid-turn: the answers go through the mod at once and Claude holds them until the
turn ends, so the server keeps nothing. A prompt sent mid-turn leaves the session's status alone (a
permission prompt still shows as Needs you). Which prompts Claude still holds comes from the mod
(`held` on each poll, and `/mod/held`), so one another plugin refused, or one still held after a
server restart, counts right; a mod too old to report it (until it reloads) has one taken off at
each `UserPromptSubmit`. They're in `Session.promptsHeld`. While Claude holds any, the *Design
ready* notification at a turn end is skipped, since AgentK revises the design again right away, and
an approval is refused, mid-turn or not: the held answers run first, and the approval would move
the session to Code while AgentK revises the design.

**Approve design** works mid-turn too, but waits on the server instead: a turn in Design may still
change the design (often it's the revision you asked for), and moving to Code at once would lift
the Design edit guard while that turn runs. The approval, with your answers, is kept in memory
(`Session.approvalQueued`; the gate and the questions' footer say so, with **Cancel approval**). At
the turn's end it's sent, as an approval between turns is, if the turn left the design and its
questions alone; if it changed them it's dropped, and you're notified to review the design and
approve again (the answers are still in the browser). It's dropped, with a line in the activity
log, when Claude exits (with its `SessionEnd`, or found dead by `reconcile()`) or is relaunched, or
the session is archived; a server restart forgets it. While it waits, the answers it took can't be
edited, and answers can't be sent; the browser marks its drafts (`approved` in
`ak-design-answers:<session>`) and forgets them once the session has left Design, or makes them
drafts again when the approval was dropped. Without the mod, approving and sending answers are
refused mid-turn, as before.

**The design's questions tool.** The mod registers a `design_questions` tool at `session.start`
(`mcp__agentk__design_questions`; Claude Code may defer it, so the prompt says to load it with
ToolSearch). Its input is the questions file's list. The server checks it strictly
(`checkToolQuestions()` in `design.ts`: each question an object with an id and a question, ids not
repeated, options with labels, a recommendation that points at an option, a status of open or
resolved, at most 30, and no more than the file can hold) and answers what's wrong, so Claude fixes
it in the same turn. Good questions are saved to `questions.json` at once (through a temp file
renamed into place, so a restart and the next turn end read the same ones) and shown straight away:
when they differ from the ones kept, the design's time moves on and the change counts as a change to
the design (the same list again changes nothing). The tool is refused
outside Design. Without it (no mod, or an older session) Claude writes the file as before.

**Context fill.** The session footer (and the ⋯ sheet on a phone) shows `context` and a ring for
the latest measurement. The ring fills counter-clockwise from 12 o'clock (a C at 50%, a closed O
at 100%), so its shape gives the level without colour; its arc is blue, amber from 75% and red
from 95%. The exact percent is in its tooltip and its meter value, and shown with the ring: with a mouse it pops up over the ring as soon as the mouse is on it (the tooltip takes a second), floating so the footer doesn't shift; on a touch screen, which can't hover, it is written after the ring, in the footer on a tablet and in the ⋯ sheet on a phone. It is cleared when Claude is relaunched or
`/clear`ed, until the next turn measures it again. The plan limits in the measurement are merged
like the status line's (see [Plan limits](#token-usage)).

Without the mod everything works as before; the footer just shows no context fill. The mod
swallows every failure, and a hook that throws is skipped by Claude Code. `claude plugin validate`
and `claude plugin test src/mod/agentk` check it against the engine (`test/unit/mod.test.ts` runs
both where Claude Code is installed). Its source compiles against Claude Code's own declarations,
not the repo's tsconfig.

## The app

The left rail holds projects, each with its repo's folder name in small type under the project
name and the most urgent session state shown on it. Drag a project to reorder the list; on a touch
screen, use **Move up** / **Move down** in the project home's settings. The top tabs hold that project's sessions. The
main pane is the live Claude terminal, and the footer shows where the session is working: branch,
diff size, tokens, the project's Meko datapack (`Meko:<datapack>`) and stage. The foot of the rail
shows the user and the server (`hari@dev-box`) with the version, so windows on different machines
are easy to tell apart. The server name is `[server] name` (set in Settings), else the machine's
host name up to its first dot; the tooltip adds the full host name. When space is short, the
version moves below it first, then the user name is cut off before the server name.

![The AgentK Local window: the project rail, session tabs, the stage bar and a live Claude terminal](screenshots/workbench.png)

Session states, shown with a dot in the rail and on each tab:

- **Working**
- **Needs you:** a permission prompt or a question
- **Idle**, waiting for your next prompt
- **Recovered** after a restart
- **Exited**

**Renaming a session.** Double-click its tab (on a phone, **Rename session** in the ⋯ sheet; or
**Rename** in the `⌘K` palette) to give it a new name (`PATCH /api/sessions/:id`). Only the name
changes: the branch, worktree, tmux session and Meko agent id were named from it once, at the start,
and stay. The name is trimmed, its line breaks and runs of spaces made one space, and it can't be
empty, longer than 100 characters or hold control characters. The project's activity notes the
rename, the session's Meko conversation is retitled in the background (`conversation_update`, in
the datapack the session was launched with, as its other Meko writes; Meko off or failing, it
keeps the old title), and Claude reads the new name in its system prompt
the next time it's launched. Claude Code's own name for the conversation (`--name`, given at the
first launch) stays. An archived session can't be renamed.

### Open project

You open a project on a repo you already have, with a new datapack or one that exists (the
datapack of a project you closed, say, which opens it again). The dialog is **Open project**, and so
are the rail's and command palette's actions. It lists every git repo directly under
your repos folder (`~/code` by default), with the most recently touched first. A repo can back
several projects: one that already has a project is marked with its name (*In "infra"*, or *In 2
projects*) and can still be picked. A project stores its repo's real path, so one reached through a
symlink or another folder is the same repo (and its id comes from the real folder's name). Each
project on a repo gets its own datapack, settings, sessions and worktrees; project
names must differ within a repo, and a name is taken from the moment its open starts, so two
opens of the same name at once can't both succeed. Their session branches
share the repo, so a branch name that another project already uses gets a number added
(`ak/fix-tests-2`). Per-repo files such as `.agent-k/stages/` overrides apply to every project on
the repo.

![The Open project dialog listing the git repos under ~/code](screenshots/new-project.png)

- **What the list shows.** For each repo: its folder name, the `origin` remote, the current
  branch, and when it was last touched. The scan also reads a fork's `upstream` remote, for naming
  and matching (below). The server scans the folder when the dialog opens, so
  repos you clone by hand appear straight away.
- **What it leaves out.** Folders that aren't git repos, and git worktrees (a `.git` *file*
  rather than a directory), are left out. Only the top level is scanned by default. The scan depth
  and the list of folders are both settings.
- **Clone from URL.** Paste a git URL and the server clones it into `~/code/<name>` using your
  existing git credentials, then selects it. "Other folder" takes any repo path on the host; an
  empty folder there is made a repo as a new repo is (below), once the project name is checked,
  and on a failure its `.git` is removed again. A folder with files that isn't a repo is left alone, and the error says to
  `git init` it and commit its files.
- **A repo with no commits yet.** A worktree needs a commit to branch from, so a repo that is just
  `git init` (picked, typed, or cloned from an empty remote) gets the same empty first commit as a
  new repo when its project opens (`ensureFirstCommit()` in `git.ts`), and so does a session's repo
  when the session starts (for a repo opened before AgentK did this, or another scanned repo under
  Advanced). One with files in it (untracked or staged) is refused instead, changing nothing: an
  empty commit would leave them out of every worktree, and what to commit is the user's call, so
  the error says how to commit them. These run one at a time per repo, so two opens or starts at
  once make one repo and one commit. They come before the datapack, so Meko never gets a datapack
  for an open that then fails here; a repo or commit made for an open Meko then refuses stays, and
  a retry opens the repo as it is.
- **New repo.** Type a folder name (it's never filled in for you) and the server makes
  `~/code/<name>` (the clone folder):
  `git init`, `HEAD` on your `init.defaultBranch` (or `main`), and an empty first commit as you, so
  a session's worktree has a base to branch from. The commit skips signing and hooks (the server
  has no terminal to answer a passphrase prompt, and an empty commit has nothing to check) and runs
  in the C locale, so a missing name and email is recognised in any language; then the folder is
  removed and the error says how to set them. The name is one folder name (letters, digits, `.`,
  `_`, `-`, not starting with `.` or `-`, up to 100 characters; `newRepoNameProblem()` in
  `types.ts`, which the dialog also checks as you type). A folder already there is left alone
  (409), except a repo just as an earlier try left it, which is used again, so a create that
  failed later (Meko refusing the datapack, say) can be retried: spelled exactly the same (on a
  case-insensitive disk "Fresh" finds "fresh", which is another repo), no remote, one commit with
  no files, nothing uncommitted, untracked or ignored (`status --ignored`, so files your global
  excludes hide count), and not in use (no open project on it, and no running session of an open
  project working in it). While a new repo's project is being opened its folder is held (case
  folded), so a second open of the same name meanwhile gets the 409 instead of sharing the repo. The repo has no remote: sessions work in it, but a PR needs one added first.

The dialog asks for the **dedicated datapack**, then the **Project name**, then the repo. It starts with the datapack: **Create a new one** (the first option and the
default), **Use an existing one** (the default when it's opened for a share, from its banner) or
**No datapack** (the default while Meko is off). The existing list shows invites waiting for you first, then datapacks shared with you, then yours, each with its
access and, for shares, its owner. It holds project datapacks (`adlc/proj/…`) unless you tap **Show
all datapacks**. A datapack a project here already uses is tagged with that project and listed
last. A viewer-only datapack is listed but can't be chosen, because sessions write to the dedicated
datapack. **Refresh** lists them again and checks Meko for new shares. Choosing a datapack collapses
the list to that one row (**Change** brings it back) and fills in the rest from its name
`adlc/proj/<repo>/<project>`:

- **Project name:** the `<project>` part (everything after the repo part). You can still change it.
- **Repo:** by the remote in the datapack's description (AgentK writes it there when it creates a
  datapack; SSH and HTTPS forms compare equal): first a fork whose `upstream` is that remote, since
  that's where you work on it, then a clone whose `origin` is the remote itself. So with
  `~/code/hari-yugabyte-db` (a fork of `yugabyte/yugabyte-db`) and `~/code/yugabyte-db` (a clone of
  it), the fork is picked. Otherwise by `<repo>` against a fork's upstream repo name, the origin's
  name or the folder name, all alike (a name says less than a remote, so an old fork doesn't beat
  the clone you use). With several matches, the most recently touched, and the hint says so. The
  repo filter also searches the upstream remote. With none, the remote (if known) is filled in under Clone from URL, or the
  hint says no repo by that name was found.

The name and repo matching live in `src/shared/datapacks.ts`, so the server and the dialog use the
same rules. **Create a new one** puts its **New datapack name** field at the bottom, under
the repo, since it's filled in from the repo and the **Project name**: it suggests `adlc/proj/<repo>/<project>` from the
repo and the name you type (until you edit it), where `<repo>` is the upstream repo's name for a
fork, else the origin's, else the folder's (`hari-yugabyte-db` gives `yugabyte-db`), whether the
repo was picked from the list or typed under Other folder; a name without the prefix gets a hint
that people you share it with won't be offered it as a project. Choosing an invite changes the
button to **Accept and open project**: the server accepts the invite in Meko first, and opens
nothing if Meko refuses. The server refuses a viewer-only datapack as a project's own too (from the
shares table's grant, else `datapack_describe`), not only the dialog.

**Opening a closed project again.** When the chosen datapack was the dedicated datapack of a closed
project (the most recently closed one, if several), that project opens again rather than a new
one: same id, settings, sessions (archived), token usage, attached shared datapacks and Meko
conversation, with the name and repo given in the dialog (the repo can be another clone; each
session keeps its own `repo_path`). It's done in the same write that would add a new project, so
two opens at once can't both take it. So a project is one thing across closes and opens, on the
Usage page too. A new datapack, or **No datapack**, always opens a new project. The dialog knows
the closed projects (`InviteCheck.closed`, the latest closed per datapack): their datapacks are
tagged **Closed "*name*"**, choosing one fills in the project's name and repo (under Other folder
when the repo isn't in the list), and a hint says the project opens again, and where it moves
from when another repo is picked. The close dialog and **Close project** confirmation mention
reopening only for a project with a dedicated datapack.

Once the project is open, the New session dialog opens for it, so you go straight on to its
first task. The UI keeps the project the create reply returned and lists it (`withOpened()` in
`src/web/util.ts`) until a state push brings it, so the route, the rail and the dialog never wait
on the events socket, which may be slow or reconnecting. A push without it may be an older
snapshot, or mean it was closed meanwhile: the UI asks `GET /api/projects/:id`, and a 404 drops it
(the dialog then closes, as it does for any project closed under it). Closing it leaves the project's empty view with its **New session** button.

### New session

You give it a name, where to start and, optionally, a first prompt. Every task runs on one
pipeline, Explore → Design → Code → PR & Review → Merge & Close, and **Where do you want to
start?** picks its first stage: the stages it can start at are pills like the stage bar's steps,
numbered from 0 and joined by lines (dotted from Explore, which is optional), then "or" and Review
a PR. They sit in one row on a desktop and one column on a phone, with a line under them saying what
the selected one is for. The Name and First prompt fields show an example of work that suits it
(`START_EXAMPLES`): a question for Explore, a feature with choices to make for Design, a one-line
fix for Code:

| Start | For | First prompt |
|---|---|---|
| **0 Explore** (`start: 'explore'`) | Investigating, answering questions, POCs and experiments; you move it on to Design, Code or a PR when you're ready | `explore`; with no first prompt it starts at an empty prompt |
| **1 Design** (the default) | Normal tasks: a design for you to approve, then code, PR, review and merge | `design`; with no first prompt the name is the task |
| **2 Code** | Small tasks: straight to code, then PR, review and merge | `code`; with no first prompt the name is the task |
| **Review a PR** (`kind: 'review-pr'`) | Reviewing someone else's PR, with AgentK assisting | `review-pr`; it stays in Reviewing PR until you **Close review** (see [Reviewing a PR](#reviewing-a-pr)) |

A task can't start at PR & Review: it gets there through **Open PR** or a move. A session is a
task or a Review a PR (`kindOf()` in `src/shared/stages.ts`). `sessions.kind` and `Session.kind`
keep the old type names for tasks, and new tasks are stored and sent as `plan`, because an older
release after a rollback, and a page still running the older UI until it reloads, index their stage
lists with it (`task` would make them throw). Where a task started is its first `session_stages` row,
and steps before it show as skipped. The workflow line of the system prompt Claude gets lists the
task's stages, and says what Explore means while the session is there. The old session types still
work in `CreateSessionRequest.kind` for a page that hasn't reloaded yet (`explore`, `plan` and
`quick` start at Explore, Design and Code), and so does `skipDesign: true` (Code). The server fetches the base from `origin` and fast-forwards your local base
branch to it when that's safe, then creates the worktree and branch from it, starts Claude inside
it (or in an idle worktree of the repo: see [Worktree reuse](#worktree-reuse)), and focuses the tab. If the local base can't be fast-forwarded (it has commits origin lacks,
local edits would be overwritten, or it is checked out in another session's worktree), the session starts from `origin/<base>` and the activity log
says why.

**The dialog doesn't wait for the worktree.** On a big repo the fetch and checkout take minutes,
so creating a session (`SessionManager.begin`) is in two parts. The quick checks come first and
answer in the dialog: the kind and start, the repo, a Review a PR's PR on GitHub, a typed branch
(fetched, and checked against running sessions and other checkouts), and the base branch, which
must be a branch of the repo or of its origin (fetched only when it's in neither, so the usual
start runs no fetch here). Then the session's row goes in and the request returns, so the dialog
closes on the session's tab, where the startup steps tick off (see [Starting](#starting)) while you
work elsewhere. The page shows the session from the reply until a state push brings it
(`withCreated`), since the reply comes first. Fetching a
review's PR head, making the worktree and starting Claude follow in the background. If the worktree
or the PR head can't be had there, the session is removed (its worktree slot goes back to the pool)
and becomes a **failed start** (`AppState.failedStarts`, kept in memory until dismissed, at most
20): a banner says what went wrong, with **Try again**, which opens the dialog filled in as before
(without its images: they're removed with the failed start, and the dialog says to add them again),
and **Dismiss** (`DELETE /api/failed-starts/:id`), and the project's notifications, when on, send
one. Starting a session under the same name again replaces its failed start. When a failed start
is the reason the project's last session went away, the Close project offer stays away. Claude
failing to start leaves the session as it always did: exited, with the error, and **Resume**.
Archive and Discard wait up to a minute for a session that's still starting, then refuse (409) and
ask you to try again once its startup steps are done; on a big repo the worktree can take longer
than that, and a start can't be cancelled.

**Advanced**, collapsed by default under the first prompt, holds the repo and what the session works
on: a new branch from a base, an existing branch, or an open pull request's branch. Closed, it shows
them on one line (`agent-k-local · from main · ak/fix-retry-backoff`, `agent-k-local · on hsunder/foo`,
or `agent-k-local · PR #123 · hsunder/foo`).

- **Repo.** The project's repo, or any other repo the Open project dialog lists (the scanned repos
  only, loaded when Advanced first opens; `GET /api/repos/branches` gives a repo's branches and
  default branch). A session in another repo still belongs to its project (datapack,
  settings, usage), but its worktree, branch, PR and cleanup are in that repo: `sessions.repo_path`
  holds it, and everything after create reads it from there. The session's system prompt names the
  repo, and its footer shows the repo's folder before the branch.
- **Work on: New branch** (the default). **Base branch** is the repo's default branch unless you pick
  another. **Branch**, left empty, is `<prefix><slug of the name>`; a typed name is used as it is,
  with no prefix. A typed name that already exists is worked on as it is, as under Existing branch
  but without the PR lookup (the field says so when it matches a local branch).
- **Work on: Existing branch** (`existingBranch: true` with `branch`). One of the repo's local
  branches (offered as you type), or one only on origin. A name that's in neither is refused (400)
  rather than started as a new branch. **Compare with** is the base used when the branch has no
  open PR. As the session starts (after the dialog has closed), AgentK looks for the branch's open
  PR for up to 5 seconds (`findPr`, an older open PR included); one found is linked (`pr_number`,
  event *Linked PR #N*) and its base becomes the session's base. An answer after that is dropped,
  since Claude has started without it; the PR stage still finds the PR.
- **Work on: Pull request** (`pr` on a task). Your open PRs in the repo (`GET
  /api/projects/:id/task-prs`, from `gh pr list --author @me`), or another PR by number or link.
  The session works on the PR's head branch against the PR's base, with the PR linked from the start;
  picking one names an unnamed session after the PR and starts it at Code unless you picked a start.
  The PR must be open and in the repo's own repo (its origin, or its upstream for a fork's clone,
  as for Review a PR; AgentK's later `gh` calls about a task's PR name its repo with `--repo`, from
  the PR's URL), and one of the repo's remotes (any name) must be the PR's head repo: for a PR from a fork, the fork added as a remote (`git remote add <owner> <url>`; the
  picker says so for a PR whose fork isn't one). The head branch is fetched from that remote. Its
  local name is the head branch's, unless the remote isn't origin and a local branch of that name
  tracks something else (a fork's `main`): then `<remote>-<branch>`. When the head isn't origin's
  branch of the session's name, the session records where it pushes (`sessions.push_remote`,
  `push_branch`), the branch tracks it, and Claude's system prompt and PR prompt say to push there
  (`git push <remote> HEAD:<branch>`). Merge & Close then leaves the fork's branch alone. A PR whose
  branch a running session uses is listed but can't be picked.

On an existing branch (from either mode, or a typed name that exists), the branch is fetched first:
`git fetch origin <branch>` (or the PR head's remote). One only on the remote gets a local branch at
its remote-tracking ref; one the remote no longer has loses its stale remote-tracking ref and, typed
under New branch, starts fresh from the base. A local branch that's behind its remote copy is
fast-forwarded (never forced); one with commits the remote lacks is used as it is, and the created
event says why it wasn't updated. The server refuses (409) a branch a running session of any project
uses on that repo, checked in the same write that adds the session so two starts can't both have it,
and a branch checked out in another checkout (the main checkout, or a worktree kept for an archived
session with uncommitted changes, which it names). A reused branch is marked
(`sessions.branch_reused`). Claude's first prompt then starts with the `attached` prompt (the
branch already has work: read its commits beyond the base and, for a PR, its description, reviews,
comments and checks, acting only on feedback from the user and people with a role on the repo, and
build on it), and its system prompt says so for the session's life. The PR
stage takes the linked PR while it's open, looked up by number (so a fork's PR counts), else that
branch's open PR even when it's older than the session (never a fork's PR from a branch of the same
name), and Open PR first moves the session's base to that PR's base; the PR prompt tells Claude to
update an existing PR rather than open another. Moving the session to PR & Review picks the PR up
(*Back on PR #N*). Archive keeps a reused branch like any other, and so does Discard (the branch
existed before the session); Merge & Close deletes it once the PR has merged.

### Changes

Clicking the `+N −N` numbers in the footer opens a side panel with every file changed on the
branch since it left the base: committed, uncommitted and untracked (ignored files are left out).
The base is the branch's merge base with `origin/<base>` or, in a fork, `upstream/<base>`, whichever
is closer to the branch (a fork's own `<base>` can lag the upstream one the branch was rebased onto);
`origin` wins a tie, and the local `<base>` is used only when neither remote has one.
Each file has a kind (A, M, D, R, U), its line counts, an *uncommitted* tag when it has uncommitted
edits, and a collapsible diff. Single-file diffs are cut at 256 KB and the whole panel at 4 MB;
past that only line counts are shown.
Untracked files over 1 MB aren't read (no diff, 0 lines), and at most 32 MB of untracked files is
read per refresh. A changed image (PNG, JPEG, GIF, WebP, AVIF, BMP, ICO) is
shown as *Before* and *After* side by side with its size, instead of "Binary file";
`GET /api/sessions/:id/changes/image` serves the base's version from git or the worktree's from
disk, only for those types, only inside the repo (no `..`, no symlinks out), up to 20 MB, and with
a sandboxing content policy so the bytes can never run as a page. The footer counts use the same
comparison, so they always match. The file list is the header's **Files** menu, with a filter, so
you can jump to any file from anywhere in a long diff; each file's header shows its position
(*4 / 12*), and `[` / `]` step to the previous or next file.

### Review comments

Click **+** beside a line of any diff (shift-click a second line for a range) to leave a comment,
as on a PR. Comments are drafts in `state.db` until you click **Send N comments**, which sends
them to Claude as one prompt (through [AgentK's mod](#agentks-mod-in-claude), or pasted into the
terminal), grouped by file, with the lines each one is about. If Claude is
mid-turn or waiting on a permission prompt, they queue and go out when it's back at its prompt in
the Code or Review stage. Each comment keeps an excerpt of its last 20 lines (lines over 1,000 characters are cut short and
marked with "…"), so it follows them when
Claude moves code (its card then shows the line it's at now and the one it was written on). When
its lines can't be found it's shown at the top of its file, still sendable: as changed if the diff
still shows its line with other text, otherwise as not in the diff shown. Deleting a draft asks
first. Sent comments are
kept and shown with **Show sent**.

### Design panel

During Design the panel opens as soon as the design exists and again whenever it's revised, at
about 60% of the work area. An Artifact design is shown in a sandboxed frame (scripts allowed, no
same-origin access, and a content policy that blocks network requests and limits scripts, styles
and fonts to what Artifacts use, placed first in the frame's document so nothing of the page runs
before it), so the page can't read the login cookie or call the API. **Open on claude.ai** links to
the published Artifact. A markdown design is rendered as sanitized HTML with no forms or styles,
and its links open in a new tab. Its ` ```mermaid ` blocks are drawn as diagrams (the Design prompt
tells Claude they are): Mermaid is loaded only when a design has one and draws with SVG text in a
system font. What keeps a diagram out of the app is Mermaid's strict mode: labels are sanitized (or
drawn as SVG text) and click callbacks are off. Mermaid draws in the app's own page, in a container
it adds and removes, so while it draws, a label that got past its sanitizing would be live in the
app, and images a diagram names are fetched. The finished diagram is shown as an `<img>` of its SVG,
never inline, so what's on screen can't run, load anything or restyle the app. Markdown is
sanitized with its own DOMPurify instance, out of reach of the hooks Mermaid adds to the shared one.
A diagram fits the column, a tap opens it full size, its source is a tap away, and one that doesn't
parse shows Mermaid's error above its source, asking you to have AgentK fix it. An Artifact page with
`class="mermaid"` blocks that leaves drawing them to the claude.ai viewer gets the same Mermaid
version and look from jsdelivr, which the frame's policy already allows, so its diagrams show here
too. They're drawn one at a time, and a block that doesn't draw is replaced by the same note,
Mermaid's error and the source Mermaid read, as text, instead of Mermaid's error graphic. The usual
cause is `<` directly before a letter, `/`, `!` or `?` in a label (`<session>`, `List<T>`): the
browser parses the page first, so it becomes an HTML tag before Mermaid reads the block, here and
in the claude.ai viewer alike. Any other `<` or `>`, such as the arrows `->>` and `<-->`, stays
text. The Design prompt tells Claude not to write tag-like text in an Artifact's Mermaid blocks;
markdown fences are taken out as text before any HTML parsing, so they're unaffected.

**Open questions.** Besides the design, AgentK gives you its open questions in
`~/.agent-k-local/designs/<session>/questions.json` (`{"questions": [{"id", "question", "context",
"options": [{"label", "detail"}], "recommended", "status": "open" | "resolved", "decision"}]}`):
through the [mod's `design_questions` tool](#agentks-mod-in-claude) when Claude has it, which saves
the file and shows them at once, or else by writing the file, read at the turn's end.
The line asking for them is added to the Design prompt in code (`designQuestionsLine()`), so a repo's
own `design` prompt gets it too. The server keeps each session's questions in memory, never in
the database (`DesignQuestionStore` in `answers.ts`): they're read the first time they're asked
for, for every session in Design at startup (so the first turn end after a restart can tell
whether they changed), and again at each Design turn end. A read is capped (256 KB, 30 questions,
6 options each, text cut short) and normalized (ids lowercased to letters, digits and dashes;
entries without an id or question, and repeated ids, dropped; a recommendation must point at an
option). A file that's too large, can't be read or isn't questions is logged and leaves the ones
kept alone. That holds within one server run: after a restart there's nothing kept, so a broken file
shows the markdown design's list or no questions, and the session's activity log says the file
needs fixing. The `designs` questions columns and the `design_answers` table from before are left
in place, unused, so the release before this one still works after a rollback; a later migration
drops them.
Without the file, a markdown design's top-level list items under an "Open questions" heading are
its questions (only those with a "?" under a heading that also names risks), with ids from their
text. A change to the questions counts as a change to the design: the design's time moves on so
the panel refreshes, and the session is *Design ready* again.

The panel then has two tabs, **Design** and **Questions** (with the open count, also shown on the
Design doc button and the ⋯ sheet's row); on the Design tab a bar says how many questions are open
and jumps to them. The full page on a wide screen shows the questions in a column beside the design
instead. Each open question is a card: the question, its context, the options as radio rows with
AgentK's recommendation marked and picked to start with, **Custom answer**, and one text box: a
note on the option picked (optional), or the answer itself with Custom answer (or when there are no
options). Resolved questions are listed under **Decided** as read-only cards with their decision,
open when no question is left open, else collapsed. Past Design, an open question shows nothing
picked and reads *Unanswered*, never the recommendation. **Other feedback**
takes anything else about the design. Question text renders as plain text, never HTML.

Drafts aren't stored on the server. The browser keeps them per session in localStorage
(`ak-design-answers:<session>`, `src/web/design-answers.ts`), in step across tabs, so a reload or
the full page keeps them; their text boxes are reload-safe. An option is kept by label too: when
a revision moves it, the pick follows it; when it's gone, the question starts over at the
recommendation, keeping the text.

With open questions, **Approve design** reads **Answer & approve**: the gate, the panel header and
the ⋯ sheet take you to the questions, and the approval goes from their footer, **Approve with
answers**. The answers go in the approval request (`DesignAnswersRequest`), sent ahead of the Code
prompt as `approved-answers`, every open question listed: answered, or left to AgentK's
recommendation (its own judgment when it has none). **Send for revision** sends them as the `design-answers` prompt instead, and AgentK
revises the design. Without AgentK's mod it only works
while Claude is at its prompt (between turns, or with the design waiting for approval, never over a
permission or question prompt); otherwise the button says why, and nothing is queued. With the mod
both work mid-turn: see [Answers and approvals mid-turn](#agentks-mod-in-claude). The server checks the
answers against the current questions: one to a question no longer open, or whose option's label
changed, is refused (409) so the user looks again; answers too long for one prompt (50,000
characters) are refused too.

Once the answers have gone to Claude, the server records them in `questions.json`
(`DesignQuestionStore.record`): each answered question is resolved, with the answer as its decision
(the option's label, then ` — ` and the note if there is one, or the custom answer). An approval
also resolves every other open question, as *Left to AgentK: <recommended option>* (or *Left to
AgentK* without one). The questions are read from the file as it is then, so a change AgentK made
mid-turn is kept, and an answer to a question that changed since the answers were checked is left
out; a file that can't be read is left alone for AgentK to fix (logged), never overwritten from
the questions kept in memory. The file is written the same way as the tool's (temp file, then
rename), the kept questions are replaced, and the design's time moves on so every browser shows
them; this isn't a change AgentK made in its turn, so it neither drops a queued approval nor marks
the design ready. The `design-answers` prompt still asks Claude to mark each answered question
resolved, saying AgentK Local may have done so already. A markdown design's questions are only
written with the approval: until then AgentK revises its "Open questions" list, which a questions
file would hide. Nothing is recorded when the send fails.

What an approval did beyond the user's answers is noted in `questions-approval.json` beside the
questions file: the questions it left to AgentK, and whether it made the file (for a markdown
design). When the session is moved back to Design, `reopen` undoes that: those questions are open
again, and a file it made is removed so the design's own list shows again.

After an approval goes out, the browser forgets the drafts (an approval queued for the turn's end
keeps them, in case it's dropped). After a revision it forgets the feedback and keeps the answers
until their questions are no longer open, so they still show until the questions come back
resolved, and for a markdown design's questions until AgentK changes them. Answers reach
Meko in the revised design and the session's conversation.

### Side panels

The design doc and the changes panel open on the right of the terminal, one at a time. Drag the
panel's left edge (or focus it and use the arrow keys) to resize it; double-click the edge to
switch between normal and widest. Each panel's width is remembered in the browser. The design doc
also opens full page in a new tab (`/p/<project>/s/<session>/design`), where it can still be
approved.

### Starting

Claude Code can take a while to load its plugins, MCP servers and startup hooks. While a session
starts, the terminal shows its steps (updating the base branch and preparing the worktree, or for a
Review a PR fetching the PR and preparing the worktree, starting Claude Code, loading, sending your
task) with the current one animated and timed, plus the stage the session opens in. The session's
tab shows the current step in its tooltip. The view shows up as soon as the New session dialog
closes, before the worktree is made; after 10 seconds on that step it says a big repo can take a
few minutes and that you can switch to other sessions meanwhile.

The steps come from Claude's hooks. If they never arrive (the hook socket was gone, say), a
session would stay on this view while Claude works under it. So the reconcile tick also watches
the conversation's transcript: once it has been written since the start and the last step is more
than 20 seconds old, Claude has taken a prompt, and the session moves on. Until a hook arrives,
it shows working while the transcript changes (in the last 15 seconds) and idle otherwise, which
is only a stand-in: a long tool call shows as idle and permission prompts don't show. A server
that restarts while a session starts picks the startup back up, timed from when it began. After
90 seconds on the last steps, the view says Claude may already be running and offers **Show
terminal**, which hides it for that start in this page.

### Keyboard

`⌘1–9` switches projects, `⌃Tab` cycles sessions (browsers keep `⌃Tab` for themselves in a
normal tab, so it works in the installed app window), and `⌘K` opens a palette to jump to any
session or project home, create a session, or move or rename the one on screen; session names show
in the accent colour and project names in bold, so they don't read as commands. On Linux and
Windows these use Ctrl, so the terminal
doesn't get Ctrl+K, Ctrl+1–9 or Ctrl+Tab; every other key passes through to the terminal whenever
it has focus. To copy from the terminal, drag across the text: tmux selects it and sends it as
OSC 52, which the page puts on the clipboard. Shift-drag (Option-drag on a Mac) selects in the
browser instead, and then `Ctrl+C` (`⌘C`) copies rather than interrupting Claude. Clipboard access
needs HTTPS or localhost.

To give Claude an image, paste it into the terminal (`⌘V`, or `Ctrl+V` / `Ctrl+Shift+V` on Linux and
Windows) or drop image files on it. Claude Code's own `Ctrl+V` reads the clipboard of the host it
runs on, which is the browser's only when both are on one computer, so the page uploads each image
(`POST /api/sessions/:id/images`, the image as the body) and pastes the path the server saved it at;
Claude Code reads a pasted path to an image as that image and shows `[Image #1]`. The paths wait
until you send: each image shows in a strip of thumbnails under the terminal (above the compose box
on touch screens) while it uploads and after, with ✕ to take it out, and the next Enter typed into
the terminal (or the key bar's ⏎, or the compose box's Send) pastes the held paths one at a time,
200 ms apart, after anything you typed, then sends Enter, so `fix this [Image #1]` goes as one
message (`heldImages.ts`, `flush()` in `terminals.ts`). Keys typed while that runs, sticky-Ctrl
letters included, are queued and follow it; Enter waits for uploads still running, and Esc or
`Ctrl+C` meanwhile calls the send off (✕ on a stalled image also ends the wait). Pasted text and
Option+Enter (a new line in Claude Code) don't send held images. If the terminal isn't connected,
or an upload failed, nothing is sent and the images stay held. If the connection drops partway,
what went out isn't sent again (each image leaves the strip once its path is out, and the compose
box doesn't get its text back), and a notice says to check Claude's prompt. An image taken out of
the strip stays on the host until the session's pastes are removed. Held images live in the page,
on each session's terminal, so an update's reload waits while any session holds some (see Releases
and auto-update). The server takes
PNG, JPEG, GIF and WebP, checked by their bytes, up to 20 MB each, and saves them owner-only under
`~/.agent-k-local/pastes/<session>/`; they're removed when the session is archived or its project
closed. A paste that carries text as well as an image (a spreadsheet's cells come with a picture of
them) pastes the text, unless the text is only the copied files' names. The page can't tell whether
Claude Code shares its computer (a tunnel to a remote host is localhost too), so `Ctrl+V` works the
same either way. On Linux and Windows it's the browser's paste, so it pastes text as well, where it
used to send `Ctrl+V` to Claude. On a Mac, `Ctrl+V` reads the clipboard through the async Clipboard
API (Chrome asks once, Safari before each read): an image is held the same way, and otherwise
Claude gets `Ctrl+V` as before. On a phone, an image pasted into the compose box is held too (text
still pastes into the box), and the image key at the end of the key bar's ▾ row picks images to
hold.

Images can go with a session's first prompt too: paste them into the New session dialog's First
prompt field (a review's Notes), drop them on it, or pick them with **Add image**. There's no
session yet, so each one uploads to the project as it's added (`POST /api/projects/:id/images`,
the same checks), is saved owner-only under `~/.agent-k-local/pastes/.new/<project>/`, and the
dialog holds the id the server gave it (the file's name), with the same strip of thumbnails.
**Start session** waits for uploads still running, then names the images in
`CreateSessionRequest.images`, at most 20. The server takes only ids of that project's staged
images, checked with the other quick checks, so a refused start leaves them in the dialog; once
the session's row is in, it moves them to the session's `pastes/<session>/` and keeps the paths of
the ones that got there (`sessions.start_images`). One gone by then (swept, or taken by another
start) is left out, and the session's activity log says to paste it into the terminal. The first
prompt reaches Claude on its command line, where a path isn't taken as an attached image, so
every prompt that names the task (a review's notes) adds the paths with a line asking Claude to
open each with its Read tool: the first prompt, a move from Explore, a relaunch without resume.
The hooks settings file allows `Read` on the pastes folder, so that doesn't stop for a permission
prompt. The images aren't the task: at Explore, images with no text are a first prompt, but the
session still has no task (`Session.hasTask`), so moving it on asks for one. AgentK's Meko search
at start uses the task's text alone. Staged images no session started with
are removed after a day (at startup and hourly), and a project's when it's closed.

Opening a project (`⌘1–9`, the rail, Back from its home, the app's start page, or a link that
names no session) reopens the session last open there in this browser, kept in `localStorage`.
When that session is closed, or none was open there yet, it opens the one with the most recent
activity.

### Notifications

A browser notification fires when any session moves to *needs you* or finishes a turn while its
tab is hidden. You can mute it per project. Only one open AgentK page shows them, the one used
last (pages queue for a Web Lock and a page that gets focus steals it; when it closes, the next one
takes over), so each shows once. They go through a minimal service worker (`/sw.js`), so a
notification stays clickable after the page that showed it reloads (as open pages do after an
update) or closes. Clicking one switches to the browser tab already showing its session, else to
one on its project (which then opens the session), else opens it in the window you used last, or a
new one. A design doc open on its own page is left alone. If the service worker isn't running (it
failed to register, say), the page shows the notification itself, and clicking it opens the session
in that page. The service worker caches nothing and handles no requests.

A project datapack shared with you also sends one notification (an `invite` event, once per
datapack); clicking it opens the Open project dialog for it. Tapped through the service worker, it
opens the dialog in an open window, or opens `/`, where the share's banner is waiting.

### On a phone

At 760 px wide or less the app switches to a phone layout. The rail becomes a drawer behind a top
app bar (the Back gesture closes it, and it holds **Search and commands** in place of `⌘K`). The
app bar also holds the session tabs, between the drawer button and ⋯, so the session name isn't
repeated above them (each tab shows its status and stage). The session header,
the stage bar with its gate button (Approve design, Open PR, Merge & Close) and the footer details
move into a ⋯ sheet, leaving the screen to the terminal; a dot on ⋯ marks a gate that is ready for
you, and the sheet says why a closed gate is closed. Dialogs open as bottom sheets. The app, and
the overlay behind a dialog or sheet, are pinned to the visible viewport (its height and, where iOS
pans the page for a focused field, its offset), so the on-screen keyboard shrinks them instead of
covering the prompt or a dialog's fields; while you type, the terminal stays scrolled to its newest line. On
touch screens the terminal gets a key bar (Tab, arrows, Ctrl+C, Enter, select mode, typing mode,
and at the far right a ▾ key that opens a second row of more keys: a sticky Ctrl, Esc, Shift+Tab,
Home, End, Page Up and Page Down; the row stays open until ▾ is tapped again, and ▾ stays lit while
Ctrl is armed with the row closed; with Ctrl lit, the next letter typed in the compose box or in typing mode goes to
Claude as Ctrl plus that letter, such as Ctrl+O or Ctrl+R, and Ctrl then `[` sends Escape; last in
the row, an image key picks images for Claude and closes the row once they're picked), and a compose
box (it grows with its text to about a fifth of the visible screen, then scrolls) whose text goes
in as one bracketed paste (with any Escape characters removed), then any held images (see
Keyboard; images pasted into the box are held above it), then Enter; Send works with images alone,
and if the terminal isn't connected, the text and images stay. Swiping scrolls through mouse-wheel reports, which tmux hands to Claude Code (it
runs full screen and takes the mouse). The steps are sent together once a frame, a flick keeps
coasting and slows down until it stops or a new touch lands, and the terminal on screen draws with
WebGL (fetched only on touch screens; it falls back to the DOM renderer if WebGL is missing, loses
its context, or sizes its canvas wrongly). In select mode (it puts the keyboard away) a drag
selects whole lines instead of scrolling, and lifting the finger copies them; no touch reaches tmux
as a mouse click meanwhile. Tapping the terminal only brings up the keyboard in
typing mode (the keyboard key). Phones default to an 11 px terminal font (about 48 columns),
adjustable in the ⋯ sheet and remembered per browser.

### Project home

![The project home page with datapacks, recent activity, token usage and settings](screenshots/project-home.png)

Recent activity shows the project's last 50 events, newest first, and scrolls inside its box so it
never pushes the rest of the page down.

**Share** sits in the project's header: it shares the whole project, through its dedicated
datapack, and opens the Share dialog. While Meko's API URL isn't known (a self-hosted Meko with
no web app or API URL set) it's off, with a note to set the web app URL on the Meko tab. With Meko
off it asks for a Meko API key first (**Add a Meko API key**), and for a project without a
dedicated datapack it offers **Add a dedicated datapack**. Meko itself refuses a share from someone who isn't the owner or a
maintainer. See [Sharing a project](#sharing-a-project).

A project without a dedicated datapack says so in its Datapacks box, with **Add a dedicated
datapack** (a new one, or one of yours; `POST /api/projects/:id/dedicated-datapack`), or **Add a
Meko API key** while Meko is off. A shared datapack already attached can become the dedicated one.
New sessions use it, each pinning it for its own agent id. Search is hidden for a project with no
datapacks.

The dedicated datapack's row has **Switch** (while Meko is on). It opens the same dialog, titled
**Switch the dedicated datapack**, to make a new datapack or one of yours the dedicated one; attached
shared datapacks are listed first, and a new one's default name gets `-2` (or the next free number)
when the usual name is taken. The old datapack stays attached as shared, so its memories can still
be searched, unless **Detach *old* from this project** is checked. Nothing is deleted in Meko. The
dialog warns how many of the project's sessions are running (starting, working, needs you, idle,
recovered): they keep the datapack they were launched with, and so keep writing to the old one until
they're restarted. That covers Claude's own Meko calls (its prompt names the old one), capture (the
Meko hook keeps the datapack in the session's watermark once the session has started) and AgentK's
writes for the session: designs, the PR's Meko lines and what shipped go to the datapack recorded on
the session at launch (`sessions.meko_datapack_id`), under the session's own Meko conversation there
(AgentK's writes for a session always go under its own conversation). New and resumed sessions get the new one
(a resumed session's Meko conversation is keyed by datapack, so its earlier turns stay in the old
one). The request names the datapack it replaces
(`replace`), so a switch made meanwhile in another tab is refused (409) rather than undone; the old
row is demoted or detached in the same transaction as the new one is added, the project's
server-side Meko conversation (used for search) is made again in the new datapack. Pins are per session, so a running
session's stays on the old datapack and the next one to start pins the new one.

When the project on screen loses its last open session (Archive, Discard, Merge & Close, or a merge
picked up from GitHub), a dialog offers **Close project** (as on the project home; the repo and
datapacks stay), **New session** or **Keep it open**. It asks only on the project's session view,
with no other dialog open, and in a visible page, so a background tab stays quiet; loading or
switching to a project that has no sessions doesn't ask. It closes by itself when the project is
closed or gets an open session elsewhere.

## Projects, worktrees and sessions

A **project** is an effort to build or improve a feature, product or service. It works in a git
repository on the host, picked from your repos folder (`~/code`), cloned into it, or made new there at creation.
The project only points at the repo. It changes two things there, and two more for a session on
an existing branch (below). A repo with no commits and
no files gets an empty first commit on `HEAD`'s branch when its project opens or a session starts
in it (see [Open project](#open-project)), since a worktree needs a commit to branch from. And when a
session starts, it fast-forwards the local base branch to `origin`. That's a `merge --ff-only`
with autostash off when your main checkout has the branch checked out (git refuses if it would
overwrite local edits), or a non-forced local fetch (`git fetch . origin/<base>:<base>`) when no
worktree has it checked out (git refuses anything but a fast-forward, and a branch being rebased
or bisected). A base checked out in another session's worktree is left alone. Read-only git
commands run with `GIT_OPTIONAL_LOCKS=0`, so they never take a lock Claude's own git commands need. A session
started on an existing branch fast-forwards that local branch to its remote copy the same way, and
one started on a PR whose head is on another remote (a fork's) sets the branch's
`branch.<name>.remote` and `.merge` to it, so a plain `git pull` there follows the PR. It never rewrites, resets or discards anything in your main checkout. A
**session** is one task. It has one worktree, one branch and one Claude conversation. Worktrees
live outside the repo so they never show up in `git status`, and outlive their session: a clean one
goes idle for the next session on the repo (see [Worktree reuse](#worktree-reuse)).

```sh
# layout on the host
~/.agent-k-local/
  config.toml              # host settings: address and port, TLS paths, repo roots, update channel, Meko
  state.db                 # SQLite: projects, sessions, events, auth (WAL)
  hook.sock                # Claude hooks (and AgentK's mod) → server
  run/                     # launchers, the hooks settings file and scripts, run/mod/agentk (AgentK's mod)
  server.lock              # the running server's lease on this home (one server per home)
  certs/                   # self-signed pair, or symlinks to your own
  app/0.4.1/  app/current → 0.4.1
  worktrees/<repo>.<hash>/wt-<n>/  # git worktree, shared by the repo's projects (older ones: worktrees/<project>/<session>/)
  designs/<session>/design.md      # markdown design written during Design (Artifact designs live on claude.ai)
  designs/<session>/questions.json # the design's open questions for the user, written beside it
  designs/<session>/questions-approval.json # what an approval left to AgentK, undone on a move back to Design
  pastes/<session>/<time>-<random>.<ext>  # images pasted into the terminal, for Claude to read (removed on archive)
  pastes/.new/<project>/<time>-<random>.<ext>  # images added in the New session dialog, until a session starts with them (or a day)

# config.toml (excerpt)
[server]
# name = "work laptop"    # shown in the rail next to your user name; unset uses the machine's host name

[repos]
roots = ["~/code"]        # folders the Open project dialog lists
depth = 1                 # 1 = only direct children
clone_into = "~/code"     # where "Clone from URL" and "New repo" put new repos

[worktrees]
idle_per_repo = 3         # idle worktrees each repo keeps for its next session (0 to 20; 0 removes them)
idle_days = 14            # an idle worktree no session used for this many days is removed (0 to 365; 0 keeps it)

[meko]
# enabled = false            # turn Meko off (projects work without datapacks); on by default
# url = "…"                  # Meko MCP server; unset uses MEKO_MCP_URL, Claude Code's settings, then Meko Cloud
# api_key = "…"              # unset uses MEKO_API_KEY, then Claude Code's settings
# web_url = "…"              # Meko web app, for links; unset uses MEKO_WEB_URL, Claude Code's settings, then Meko Cloud's for Meko Cloud
# api_url = "…"              # Meko's REST API, for sharing; unset uses MEKO_API_URL, then web_url + /api/v1
# capture_transcripts = false  # stop sending session transcripts to Meko conversations; on by default

[claude]
# command = "claude"         # how Claude Code is run
# mod = false                # don't load AgentK's mod into sessions (prompts are then typed into the terminal); on by default

[call_home]
# enabled = false            # stop sending anonymous usage stats (Settings → Account); on by default
# url = "https://…/v1/report" # where reports go (the collector's report URL); unset uses the one release builds carry

[pr]
# resolve_conflicts = false  # don't have AgentK resolve a PR's merge conflicts with its base (Settings → General); on by default
```

| Action | What the server does |
|---|---|
| New session | `git fetch origin <base>`, fast-forward the local `<base>` to `origin/<base>` if safe, then in the repo's most recently idled worktree `git checkout --no-track -b ak/<slug> <base>` (or `origin/<base>` if the fast-forward was skipped), or with none `git worktree add … -b ak/<slug> <base>`, then `tmux -L agentk new -d -s ak-<proj>--<slug> -c <worktree>` running `claude --session-id <uuid> --settings <hooks> -n "<name>" [--plugin-dir <AgentK's mod>] [-- prompt]` through your login shell, with the session's variables (Meko, hook token) set after your profile runs so it can't override them. |
| Open tab | Spawns a pty on `tmux attach`, one per viewer. The terminal is resized to fit the browser. tmux 2.7 draws a window at its smallest viewer's size, so a phone lets go of its terminals after its page has been hidden for 30 seconds, and of a session's terminal 5 seconds after you switch away from it, and reconnects when it is shown again. The 30 seconds are measured by the clock: a phone that suspended the page (so its timers didn't run) lets go of the terminals when the page is shown again, then reconnects the ones on screen. The browser opens the socket at its terminal size (`?cols=&rows=`). A dropped connection reconnects with backoff (reset once a connection has stayed up 5 seconds); a close for a lost login (4401) goes to the login screen, a missing or archived session stops with a **Try again** button, and an attach that couldn't start retries with backoff. |
| Close tab | Kills the pty only. tmux and Claude keep running. |
| Claude exits | The pane dies and the status becomes *exited*. **Resume** relaunches `claude --resume <uuid>` in the same worktree. |
| Archive | Refused (409) while the worktree has uncommitted or untracked files, which would keep the branch checked out there: commit them, or Discard. The Archive menu says so and greys out **Archive** (it waits for the check), and uncommitted files are a Merge & Close blocker too. Otherwise kills the tmux session. The clean worktree goes idle for the next session (HEAD detached, so the branch is free), or is removed when *Idle worktrees per repo* is 0; the branch is always kept. The record stays, so the session can be restored later. If the session is still starting, Archive (and Discard) waits up to a minute for Claude to start first, then refuses (409) until it has. |
| Discard | Chosen in Archive's confirmation step. Kills the tmux session, throws away uncommitted changes and untracked files (`git reset --hard`, `git clean -fd`: ignored build outputs stay), lets the worktree go idle (or force-removes it when it can't be cleaned or reuse is off), and deletes the local branch, unless the session started on an existing branch or a PR's (`branch_reused`), which it keeps. It first warns about uncommitted files and commits that no remote has (none, for a kept branch). The remote branch and any PR are left alone. The record and its token usage stay, as with Archive. |
| Close project | After confirmation, stops the project's sessions and lets go of their worktrees as Archive does, keeping any worktree with uncommitted changes, and marks the project closed (`deleted_at`). Idle worktrees stay for the repo's other projects; the last project on a repo removes them. That goes for every repo the project's sessions used. Its sessions and token usage are kept for the Usage page. Never deletes the repo, its branches or the datapacks, so opening a project with its datapack (**Use an existing one**) brings the project back (see [Opening a closed project again](#open-project)). |

```sql
-- state.db (abridged). One writer: the server. Migrations keyed on PRAGMA user_version.
CREATE TABLE projects (
  id TEXT PRIMARY KEY,            -- slug of the repo, e.g. billing-api; later projects on it add their name: billing-api-release-tooling
  name TEXT NOT NULL, repo_path TEXT NOT NULL, default_branch TEXT NOT NULL,   -- the repo's real path; several projects may share a repo
  position INTEGER, settings_json TEXT, created_at INTEGER, deleted_at INTEGER);
CREATE TABLE project_datapacks (
  project_id TEXT REFERENCES projects, datapack_id TEXT, name TEXT,
  role TEXT CHECK (role IN ('dedicated','shared')),
  PRIMARY KEY (project_id, datapack_id));
CREATE UNIQUE INDEX one_dedicated ON project_datapacks(project_id) WHERE role = 'dedicated';
CREATE TABLE project_events (  -- closes and reopens, for the usage stats (a reopen clears deleted_at)
  project_id TEXT REFERENCES projects, kind TEXT CHECK (kind IN ('closed','reopened')), at INTEGER);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects, name TEXT,
  claude_session_id TEXT NOT NULL, branch TEXT, worktree_path TEXT, tmux_name TEXT,
  repo_path TEXT,                 -- the repo's real path: the project's, or another one picked in Advanced
  branch_reused INTEGER,          -- 1: the branch existed when the session started
  desired TEXT CHECK (desired IN ('running','archived')),   -- what recovery restores
  status TEXT,                    -- starting | working | needs_input | idle | exited | recovered | archived
  kind TEXT,                      -- review-pr, or a task: plan for new ones, explore | plan | quick from before tasks had start
                                  -- stages (older releases and pages index their stage lists with it); kindOf() reads all three as a task
  stage TEXT,                     -- explore | design | code | pr | review | merge (Merge & Close) | done | reviewing-pr
  pr_number INTEGER, pr_url TEXT, review_state TEXT, ci_state TEXT,
  pr_repo TEXT, pr_title TEXT, pr_author TEXT,  -- Review a PR only: the PR's owner/name on GitHub, title and author
  review_sha TEXT, pr_head_sha TEXT,  -- Review a PR: the commit checked out; the PR's head at the last look
  pr_new_commits INTEGER, pr_state TEXT,  -- commits on the PR since review_sha (NULL: rewritten); OPEN | CLOSED | MERGED
  pr_remote TEXT,                  -- Review a PR: the clone's remote the PR is fetched from (origin, or upstream for a fork)
  review_notes TEXT,               -- Review a PR: the user's notes for AgentK
  push_remote TEXT, push_branch TEXT,  -- a task on a PR whose head isn't origin's branch of its name (a fork's):
                                   --   the remote and branch it pushes to (NULL: origin, the session's branch)
  plan_mode INTEGER,               -- 1: designs in Claude's plan mode (sessions created before designs moved out of it)
  pending_prompts TEXT,            -- prompts queued for Claude: JSON map of 'review' | 'review-people' | 'conflicts' | 'ci' | 'review-pr-update' to
                                   --   { state: 'queued' | 'sent' (until that turn ends), vars }; replaces pending_review
  ci_fix_head TEXT, ci_fixes INTEGER,  -- the PR head that last got a CI-fix prompt; how many heads in a row did
  conflict_head TEXT, conflict_fixes INTEGER,  -- the PR head that last got a prompt to resolve merge conflicts; how many
                                   --   heads in a row did (both cleared once the PR merges cleanly)
  queued_gate TEXT,                -- a gate pressed before it could go: 'open-pr' (at the turn's end) | 'merge' (once
                                   --   nothing blocks it); NULL: none
  agentk_seen INTEGER,             -- 1 once AgentK commented on or reviewed the PR, or was asked to
  meko_datapack_id TEXT, meko_datapack_name TEXT,  -- the dedicated datapack at the last launch, where AgentK's
                                   --   writes for the session go (NULL: launched before this or without one; the project's is used)
  first_turn_end_at INTEGER,       -- when the first turn ended (from the transcript), for the Meko stats
  meko_agent_id TEXT,              -- the MEKO_AGENT_ID it runs with, set at create and each launch
                                   --   (NULL: launched before sessions had their own; claude_code:<project id> until relaunched)
  last_event_at INTEGER, created_at INTEGER, archived_at INTEGER);
CREATE TABLE worktrees (       -- session worktrees; idle ones (session_id NULL) wait for the repo's next session
  path TEXT PRIMARY KEY, repo TEXT,  -- repo: the repo's real path, the pool key its projects share
  session_id TEXT, released_at INTEGER,
  stale INTEGER,                   -- 1: idle but not reusable (files git doesn't ignore)
  created_at INTEGER);              -- (sessions.worktree_released is 1 once a session let go of its worktree)
CREATE TABLE designs (         -- one per session: markdown, or an Artifact's page + claude.ai link
  session_id TEXT PRIMARY KEY, markdown TEXT, html TEXT, artifact_url TEXT,
  md_version INTEGER, md_sent INTEGER,        -- versions sent to Meko (names the next one), and
  html_version INTEGER, html_sent INTEGER,    -- whether the current one was sent
  questions TEXT, questions_source TEXT,      -- unused: a design's questions are read from questions.json and
  open_questions INTEGER, updated_at INTEGER);  -- kept in memory; dropped once the release before that has aged out
CREATE TABLE design_answers (  -- unused: answers aren't stored, the browser sends them; dropped with the columns above
  id TEXT PRIMARY KEY, session_id TEXT, question_id TEXT, question TEXT, choice INTEGER, choice_label TEXT,
  your_call INTEGER, text TEXT, state TEXT, created_at INTEGER, updated_at INTEGER, sent_at INTEGER);
CREATE TABLE review_comments ( -- comments on a session's diff: draft → queued → sent to Claude
  id TEXT PRIMARY KEY, session_id TEXT, path TEXT, side TEXT, line INTEGER, end_line INTEGER,
  excerpt TEXT, body TEXT, state TEXT, created_at INTEGER, updated_at INTEGER, sent_at INTEGER);
CREATE TABLE session_stages (  -- stage history: who moved it, and when
  session_id TEXT, stage TEXT, entered_at INTEGER, left_at INTEGER, moved_by TEXT);
CREATE TABLE session_events (  -- every hook call; feeds badges + project activity
  id INTEGER PRIMARY KEY, session_id TEXT, kind TEXT, payload_json TEXT, at INTEGER);
CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1), username TEXT, pw_scrypt TEXT, totp_secret TEXT);
CREATE TABLE auth_sessions (token_sha256 TEXT PRIMARY KEY, created_at INTEGER, expires_at INTEGER, user_agent TEXT, ip TEXT);
CREATE TABLE audit_log (id INTEGER PRIMARY KEY, at INTEGER, action TEXT, detail_json TEXT);
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT);   -- setup token, update state, plan limits
```

- **Why SQLite.** You get transactions across projects, sessions and datapacks, an event history
  for the activity feed, and crash safety without hand-rolled atomic file writes. It's still one
  file you can back up or inspect with the `sqlite3` CLI.
- **Settings.** `journal_mode=WAL`, `synchronous=FULL` (so a host crash never loses a committed
  session record), `foreign_keys=ON` and `busy_timeout=5000`. There are so few writes that FULL
  sync costs nothing noticeable. Old `session_events` are pruned after 30 days, except `merged`
  events, which the Usage page counts.
- **Not on NFS.** SQLite locking is unreliable on network filesystems. The installer checks the
  filesystem type of `~/.agent-k-local`. If it's NFS, the installer puts `state.db` on local disk
  (`$XDG_STATE_HOME`, then `/var/tmp/$USER`) and records the path in `config.toml`. The
  `/var/tmp/$USER` folder must be a real directory owned by you that others can't write to, or the
  installer stops rather than put the database (password hash, TOTP secret) somewhere another user
  controls.

`config.toml` holds host settings: the listen address and port, the server's name, TLS paths, repo folders, the update
channel, the Meko URL and key (only when typed at install), transcript capture, the tmux socket, the
`claude` command, the database path, how many idle worktrees each repo keeps, and whether AgentK resolves PRs' merge conflicts. The Settings dialog
writes its fields (repo folders, scan depth, clone folder, idle worktrees per repo and the days to
keep an unused one, update channel, server name, resolving merge conflicts) there too. The server reads the file at
start, so an edit made while it runs takes effect after a restart, and a save from Settings meanwhile writes over it. Per-project settings, sessions and everything else live in `state.db`. Only values that
differ from the defaults are written, so a changed default reaches existing installs; a value of the wrong type is ignored with a warning. `AK_BIND` and
`AK_PORT` override `[server]` for one run (`npm run dev`) and are never written to the file. The Meko
URL and key come from `[meko]`, then `MEKO_MCP_URL`/`MEKO_API_KEY`, then the `env` block of Claude
Code's user `settings.json` (in `CLAUDE_CONFIG_DIR` if set); a later source's URL is skipped when that
source has a key of its own, and with no URL it's Meko Cloud.

### Worktree reuse

Repos like yugabyte-db take a long time to build from scratch, so a worktree outlives its session
and keeps its ignored files (`build/`, `node_modules/`, CMake caches) for the next one. The path
stays the same, which matters to build systems that record absolute source paths (CMake,
`yb_build.sh`): a copied or moved build tree would break.

- **Idle.** When a session ends (Archive, Discard, Merge & Close, Close project) with a clean
  worktree (`git status --porcelain` empty), `git checkout --detach` frees its branch without
  changing a file, and the worktree's row in `worktrees` loses its session. Archive refuses, and
  Merge & Close is blocked, while there are uncommitted changes; a worktree that still has some (a PR merged on
  GitHub, Close project) stays the session's and is never handed on.
- **Shared by the repo's projects.** Idle worktrees belong to the repo, keyed by its real path, so
  every project on it (a repo can have many at once) takes from the same pool. A session in another
  repo than its project's takes from and gives back to that repo's pool. New worktrees live
  in `worktrees/<repo folder>.<8 hex of sha256(real path)>/wt-<n>`; the dot keeps these folders
  apart from project ids, and older `worktrees/<project>/<session>` folders are reused where they are.
- **Claim.** A new session claims the repo's most recently idled worktree in the same SQLite write
  that adds the session, so two sessions starting at once never get the same one. The worktree
  must still be a clean worktree of the repo on a detached HEAD; then the session's branch is
  checked out there from the same start point a new worktree would get. Git rewrites only the files
  that differ, so builds are incremental. If the checks fail, the worktree leaves the pool (removed
  without `--force`, or kept as stale if git refuses) and the session gets a new worktree; if
  something else fails (a bad base branch, a branch checked out elsewhere), the worktree goes back
  to the pool and a new worktree is tried. Reuse never makes a start fail.
- **Removal is safe while sessions start.** Trimming, expiry and the startup sweep forget an idle
  worktree in one write that does nothing once a session has claimed it, and only then run
  `git worktree remove`. A worktree git won't remove (files it doesn't ignore) stays recorded as
  stale: never handed out or forgotten while its folder exists, and reusable once it's clean again.
- **Cap.** Each repo keeps at most `[worktrees] idle_per_repo` idle worktrees (Settings › General,
  default 3, 0 to 20). Beyond that the one idle longest is removed with a plain
  `git worktree remove` (it has only ignored files left, so no `--force`). Saving a lower number
  trims every repo in the background; 0 removes worktrees when sessions end.
- **Expiry.** An idle worktree no session has used for `[worktrees] idle_days` days (Settings ›
  General, default 14, 0 to 365; 0 keeps it until the cap trims it) is removed the same way. The
  server checks at startup, every hour, and in the background when the setting is lowered.
- **Ownership.** Once a session lets go of its worktree (idle, or removed), `sessions.worktree_released`
  is set, so the session never reads or removes that folder again, whatever happens to the pool:
  its Changes panel says the worktree is gone, Discard's check reports none, and `gh` runs in the
  main checkout.
- **Recovery.** At startup, in the background, the server forgets idle worktrees whose folder is gone,
  marks stale the ones no longer clean and detached (and stale ones that are clean again usable),
  removes those of repos no live project uses (as its repo, or any of its sessions' repos) and those
  unused too long, and trims to the cap. Closing a project keeps a repo's idle worktrees while
  another project uses the repo.
- **What carries over.** Every ignored file, not only build outputs: `.env`, `CLAUDE.local.md`,
  `.claude/settings.local.json`, local databases and logs. Set the cap to 0 for fresh worktrees.

## Task lifecycle

Every session but a Review a PR is a task that moves through stages, from exploring to a merged PR
with the branch cleaned up. This first pipeline covers seven stages of the development lifecycle. The pipeline is
defined as data, so later stages such as test, deploy and monitor can be added without reworking
sessions.

The PR stage (opening the PR) and the Review stage are one step in the UI, **PR & Review**:
they stay separate stages in `sessions.stage`, `session_stages` and the usage stats, and the stage
bar maps `pr` to the PR & Review step (`stepOf` in `src/shared/stages.ts`), with an *Opening PR…*
pill in its gate until the PR is found. The automatic PR → Review move logs no second stage event,
since "Opened PR #7" says it.

There are three **gates** that only you can pass: approving the design, opening the PR, and
merging. Everything between the gates runs on its own. A task started at Code has only the last two
gates. Explore (step 0) comes before them and is optional: Claude gets the `explore` prompt
(investigate, build POCs, run experiments, push experimental branches if useful), edits aren't
guarded, and review comments from the Changes panel still reach Claude. Its gate, **Move on…**,
opens the move dialog at Design, and you can also click Code or PR & Review in the stage bar; or
you end the session with Archive or Discard. A Review a PR session doesn't enter the pipeline: it
stays in Reviewing PR, on someone else's PR, until you close the review (see
[Reviewing a PR](#reviewing-a-pr)).

In the UI, the one doing the work in these stages is **AgentK** ("AgentK writes a design…",
"Wait for AgentK to finish its turn"). "Claude" is kept for the Claude Code process and its
terminal: starting it, exits, Resume, and the compose box.

```mermaid
stateDiagram-v2
  direction LR
  state "Merge & Close" as MergeClose
  [*] --> Explore : you start here
  [*] --> Design : or here (the default)
  [*] --> Code : or here
  Explore --> Design : you move it on
  Explore --> Code : you move it on
  Explore --> PR : you move it on
  Design --> Code : you approve the design
  Code --> PR : you click Open PR
  PR --> Review : PR is open (both are the PR & Review step)
  Review --> Review : findings fixed, new round requested
  Review --> MergeClose : you click Merge & Close
  Review --> MergeClose : PR merged on GitHub
  MergeClose --> [*] : merged and cleaned up
```

| Stage | Claude | AgentK Local | Moves on when |
|---|---|---|---|
| 0 · Explore (optional) | Investigates the task: reads the code, answers questions, builds POCs and runs experiments, and may commit and push experimental branches. | Guards nothing. Review comments from the Changes panel reach Claude as in Code. | You click **Move on…** (or a stage in the stage bar) and pick Design, Code or PR & Review. |
| 1 · Design | Starts with your task (not in plan mode), reads the code, and designs the change in one place: a complex design is published as a native Artifact with diagrams; a simple one is written as markdown to `~/.agent-k-local/designs/<session>/design.md`. It doesn't touch the repo. | Blocks file edits inside the worktree and main checkout (a `PreToolUse` guard hook). Keeps the published Artifact's page and claude.ai link (from the Artifact tool's `PostToolUse` hook), or reads the markdown file when the turn ends. Saves the design to the project's dedicated datapack as `design-<slug>.md` (or `.html` for an Artifact's page); Meko can't replace a file, so each later changed version is saved as `design-<slug>-v2.md`, `-v3`, and so on, and an unchanged one isn't sent again. A version counts only once it's sent: one that couldn't be (Meko off or failing, no datapack yet) is sent at the next save, Design turn end or approval. Reads the open questions AgentK lists beside the design (see Design panel), where you answer them, for a revision or with the approval. Marks the session *Design ready*, and opens the design panel by itself. | You click **Approve design** (**Answer & approve** with open questions, which takes you to them first), and the server sends the Code prompt ("The design is approved…"), after your answers. With AgentK's mod you can approve mid-turn: the approval goes when the turn ends, unless the turn changed the design. A task started at Code skips this stage. Sessions created before this change still design in plan mode and are approved through Claude's plan dialog. |
| 2 · Code | Implements the design and runs the repo's tests and checks. | Shows the diff size against the base branch in the footer; clicking it opens the Changes panel with a diff per file, where you can leave review comments and send them to Claude. | You click **Open PR**. Mid-turn it reads **Open PR after this turn**, and the PR prompt goes when the turn ends (see [Queued gates](#queued-gates)). |
| 3 · PR & Review: opening the PR (`pr`) | Commits with the repo's own commit conventions, pushes the branch, and runs `gh pr create` with a title that follows those conventions (if the repo squash-merges, the title becomes the commit subject) and a summary taken from the design. With a dedicated datapack (and Meko on), the description ends with `<Meko host>://<datapack id>;<datapack name>` and `meko_conversation_id:<id>` lines, for the session's Meko conversation. | Records the PR number and URL and links them next to the stages. | Automatically, once the PR exists. |
| 3 · PR & Review: review (`review`) | When a review arrives, follows the `review` prompt (AgentK: verifies each finding, fixes the valid ones, pushes, replies on every thread and reacts +1/−1) or the `review-people` prompt. When the PR conflicts with its base, follows the `conflicts` prompt: merges the base into the branch (no rebase or force-push), resolves the conflicts, runs the checks and pushes, or aborts the merge and says why when a conflict needs your decision. When CI fails, follows the `ci` prompt: reads the failing jobs' logs, then fixes and pushes, or reruns the jobs once if the failure isn't caused by the change. | Looks for the PR every 15 seconds in the PR stage (only a PR opened after the session was created counts, since branch names can be reused; a session started on an existing branch also takes that branch's older open PR, and one started on a PR, or whose branch's PR was linked as it started, takes that PR by number while it's open), and checks reviews, CI checks and approvals every 45 seconds and at every turn end. This runs on the server, so waiting costs no tokens. New reviews are sent to Claude according to the project's reviewer mode (below). Only the newest run of each check on the head commit counts. When CI fails on the PR's head, you get a notification and Claude gets the `ci` prompt. That happens once per head commit, after every check has finished (or 10 minutes after the failure appeared), and not while the branch has unpushed commits. After three failing heads in a row, you're asked to take over, and a green run starts the count again. When GitHub reports the PR as conflicting with its base (`mergeable` CONFLICTING; UNKNOWN, while GitHub works it out, doesn't count), you get a notification and Claude gets the `conflicts` prompt, the same way: once per head commit, not while the branch has unpushed commits, and after three conflicting heads in a row it's yours; a PR that merges cleanly again starts the count again, so a later conflict on the same head (the base moved again) gets a prompt too. **Resolve merge conflicts on PRs** in Settings → General (`[pr] resolve_conflicts` in `config.toml`) turns this off, leaving conflicts as a merge blocker for you; a conflicts prompt still queued then is never sent. If Claude is mid-turn, waiting on you, or exited, prompts are queued in `state.db` (so they survive restarts) and go out one at a time when Claude is back at its prompt: reviews, then merge conflicts, then CI. A queued CI prompt is dropped if the head moves or CI stops failing, and a queued conflicts prompt if the head moves, the PR merges cleanly, or the setting is turned off. The findings count as addressed only when the turn that got the prompt ends. | You click **Merge & Close**. It is enabled once CI is green, no review findings are left unaddressed (and, once AgentK is active on the PR, its review is in), the required approvals are in, every local commit is pushed to the PR, the worktree has no uncommitted or untracked files, and Claude is neither working nor waiting on you in the terminal. When the only blockers left clear by themselves (AgentK working, CI running, AgentK's review to come), it reads **Merge when ready** and queues the merge (see [Queued gates](#queued-gates)). |
| 4 · Merge & Close | Nothing. | Runs `gh pr merge` using the project's merge method (the repo's default unless changed in project settings). Once GitHub reports the PR as merged (also when it was merged on GitHub), first runs the [learn step](#the-learn-step) while Claude is still there (Claude gone or busy: it's skipped), then writes a summary of what shipped (PR title, link and description) to the dedicated datapack, ends the tmux session, lets the worktree go idle for the next session (or removes it), deletes the local and remote branch, runs `git fetch --prune` in the session's repo, and archives the session. If Claude is mid-turn when the PR merges, this waits until the turn ends. Branches with commits the merged PR doesn't have are kept (the remote branch is deleted only while it still points at the PR's head). If the server restarts partway, the next poll finishes the job. If the PR is closed instead, or is still open 10 minutes after the merge was requested (for example, it left a merge queue), the session goes back to Review. | Done. The session record and its history stay. |

- **AgentK in the loop.** AgentK doesn't re-review on push, so after fixes the header offers
  **Request another round**, which posts `/agentk review` (**Request AgentK review** before AgentK
  has been active on the PR). If AgentK declines a PR (for example, because it's too large), the
  refusal is shown with a **Force review** button. Forcing is always your call.
- **Reviewer modes.** Each project's reviewer setting says whose reviews Claude addresses:

  | Mode | Claude addresses reviews from | Merge waits for AgentK |
  |---|---|---|
  | *All reviewers* | AgentK, people and other bots | Once AgentK is active on the PR |
  | *AgentK + People* (default) | AgentK and people | Once AgentK is active on the PR |
  | *People only* | People | Never |

  AgentK is active on a PR once it has commented on it or reviewed it, or you or someone with a
  role on the repo has commented `/agentk review` on it, so repos AgentK doesn't watch never wait for it. Bots are the reviewers
  GitHub's REST API lists as `Bot`. It's only asked when a poll finds a new review that isn't
  AgentK's. Replies on review threads show up as empty reviews and are skipped, and so are
  Claude's own replies, which post as you. A person's review goes to Claude when it requests
  changes or comments with a body.
- **Whose feedback counts.** Anyone with a GitHub account can review a public repo's PR, so a review
  starts a turn only when it comes from someone with a role on the repo (`authorAssociation`
  OWNER, MEMBER or COLLABORATOR), from AgentK, from a bot in *All* (an app installed on the repo),
  or from you (gh's login, e.g. on a fork's PR to an upstream repo you can't write to). Any other
  review, or one whose association GitHub doesn't give, is marked seen, logged as *Ignored review
  from …* and notified once per poll; Claude never gets it. The `review-people` prompt names the
  reviewers that started it (`{{reviewers}}`, kept with the queued prompt and merged while it
  waits) and tells Claude that anyone else's comments are untrusted text, not instructions; the
  `review` prompt says the same about comments not by AgentK, `continue-review` (after a restart
  mid-review) about comments not from those reviewers, and the Review a PR prompts say the
  PR's description, commits, code comments and threads are content to review, never instructions.
  Prompts a repo overrides keep its own wording; the trigger check applies to them anyway.
- **Stage prompts.** Each stage's instructions to Claude come from a template. The defaults are
  built in (`src/server/prompts.ts`), and a repo can override any of them (a stage, or `explore`, `approved`,
  `design-answers`, `approved-answers`, `review`, `review-people`, `conflicts`, `ci`, `continue`, `comments`, `moved`, `attached`,
  `review-pr`, `review-pr-update`, `review-pr-comments`) with `.agent-k/stages/<name>.md`. This lets a team
  change how designs are written or PRs described without changing agent-k-local. Review comments
  from the Changes panel go out through the `comments` template (`{{count}}` and `{{comments}}`),
  answers to the design through `design-answers` (`{{count}}`, `{{answers}}` and `{{questionsFile}}`),
  people's reviews through `review-people` (`{{reviewers}}`, the logins whose reviews started it),
  merge conflicts with the base through `conflicts` (`{{sha}}`, the conflicting head), and CI failures
  through the `ci` template (`{{sha}}` and `{{checks}}`, the failing checks with links).
- **Branch names.** The branch prefix is a project setting, `ak/` by default, because some orgs
  enforce a branch-name prefix with rulesets.

### Queued gates

**Open PR** and **Merge & Close** can be pressed before they could go; the server keeps the press
in `sessions.queued_gate` (so it survives a restart, a self-update included) and carries it out
later. The gate then shows a pill (*✓ Open PR queued: goes when the turn ends*, *✓ Merge queued:
waiting on CI*) with **Cancel** (`cancel-queued`), and the activity log says when it was queued,
sent, withdrawn or dropped.

- **Open PR after this turn.** In Code, while AgentK is mid-turn (working, or a question or
  permission prompt during a turn) or Claude is starting, Open PR queues the PR prompt instead of
  being refused. It goes when the turn ends (at the `Stop`, before review comments), or at the next
  poll when Claude is back at its prompt (a turn that ended while the server was down), exactly as a
  press between turns would send it. It waits another turn while Claude holds prompts you sent
  mid-turn, and is dropped when Claude exits (however that's found: its `SessionEnd`, a dead pane at
  startup, a failed restart) or is relaunched, on a move and on archive. A Cancel, a move or an exit
  while the prompt is being prepared (the PR prompt waits on GitHub and Meko first) still stops it. With
  Claude exited, or waiting on you with no turn behind it, Open PR is still refused. It doesn't need
  AgentK's mod.
- **Merge when ready.** In Review, the merge blockers are of two kinds. Some clear by themselves:
  AgentK working or waiting on you mid-turn, CI running, AgentK's review still to come, its findings
  being addressed (while Claude runs), and, mid-turn only, unpushed commits and uncommitted files
  (AgentK's work in progress; mid-turn when they were measured counts too, since a turn can push and
  end during the look at the PR). The others don't: CI failing, changes requested, required approvals
  missing (only on repos that require them, since people's approvals can take days), merge conflicts,
  a branch behind its base, a draft, a PR that isn't open, unpushed or uncommitted work between turns,
  a Needs you with no turn behind it (a parked recovery), and findings left mid-address by a Claude
  that exited. With only
  the first kind left (`Session.mergeQueueable`), the gate reads **Merge when ready** and a press
  queues the merge; the reasons line reads *Merges when these clear*. While it waits, the PR is
  checked at every 15-second pass instead of every 45 seconds. A queued merge also waits for CI to
  show up on a new head: GitHub lists no checks on a fresh push for a moment, so *CI hasn't started on
  the latest push yet* holds it for up to 2 minutes after the head first appeared, after which a head
  with no checks counts as one without CI. At the first look that finds no blocker it merges, as
  Merge & Close does, and you're notified; a blocker of the second kind drops it, mid-turn or not, and you're
  notified to press Merge & Close again once that's fixed. A move out of Review, archive or a merge
  on GitHub clears it too. While the merge is being requested, Cancel is refused.

### Reviewing a PR

The other side of sharing a project: someone the author shared it with (see
[Sharing a project](#sharing-a-project)) opens its datapack as a project, from the invite's
banner, and reviews the PR with their own AgentK. The New session dialog opens by itself for a
project just opened, and starts on **Review a PR** when an open PR names the project's datapack.

**Picking the PR.** Picking **Review a PR** swaps the first prompt for a **Pull request** picker
and **Notes for AgentK** (optional: what to focus on). The picker lists, newest first and at most
20, the open PRs in any repo you can read whose description names one of the project's datapacks
(`gh search prs "<datapack id>" --match body`, once per datapack: the PRs its sessions open end
with that id, followed by `;` and the datapack's name, see the PR stage below). A project's sessions can work in several repos, so the PRs
can be in several repos too, and each row names its repo. **Another PR** takes any other one: a
number (looked up in the repo picked under Advanced), `owner/name#number`, or a PR link. GitHub's
search lags new PRs by a minute or so, so a PR missing from the list can be typed, and
**Refresh** searches again. `GET /api/projects/:id/review-prs` serves both (`src/server/prreview.ts`).
Each PR's repo is matched to a local clone among the scanned repos (and the project's and its
sessions' repos) with the rules the Open project dialog uses for a datapack's remote
(`matchRepo()`): a fork whose `upstream` is the PR's repo first, then a clone whose `origin` is,
never a name alone. A PR whose repo isn't cloned here shows **Clone** (`POST /api/repos/clone`,
which clones into the repos folder as Open project does). A typed PR whose description names a
datapack the project doesn't have offers **Attach it**, as a shared datapack, so its design can be
searched. The name fills in as `Review #21 <title>` until you edit it, and **Advanced** shows the
repo (the PR's clone, changeable to another clone of it); the base and branch come from the PR.

**Starting.** `CreateSessionRequest.pr` names the PR (`{ repo: 'owner/name', number }`), with
`repoPath` its clone. The server checks that the clone's `upstream` or `origin` is the PR's repo
and that the PR is open (400 otherwise, with nothing started). Then, after the dialog has closed
(a fetch that fails is a failed start: see [New session](#new-session); the PR isn't polled for new
commits until the session is created), it fetches `refs/pull/<n>/head` from
that remote into `refs/agentk/pr/<n>` (a fork's PR too) along with the PR's base, and puts the
branch `<prefix>review-pr-<n>` (numbered like any session's, so PR #7 of two repos never collide)
at that commit, in a new or idle worktree, with no upstream and a `pushRemote` naming a remote
that doesn't exist, so a bare `git push` fails whatever the push config (`push.default=current`
and `push.autoSetupRemote` included); an explicit `git push <remote>` still works when the user
asks for one. The base is the PR's base, so the footer diff and the Changes panel show the PR's
diff, and the prompts compare with the remote's branch the review fetched (`upstream/main` in a
fork), never the local one, which may follow the fork, lag behind or not exist. The
session keeps the PR's repo, title, author, the commit checked out (`review_sha`) and the PR's
head and state at the last look (`Session.reviewingPr`). The search at start
([Meko recall](#meko-recall-at-start)) uses the PR's title and description, without its Meko
lines, and the notes.

**AgentK assists; the user reviews.** The `review-pr` prompt tells Claude to get ready without
reviewing: read the PR description, the reviews and threads and CI's state, look at the shape of
the diff, then show a short summary (what the PR does and why, the main changes and where to start
reading, the review so far, what the author asked reviewers to look at) and wait. It lists no
problems and gives no verdict on its own; it answers what the user asks (explain code, trace a
path, check a suspicion, run a test, draft a comment in their words). Every `gh` command names the
PR's repo (`--repo owner/name`), since the clone may be a fork. With datapacks, the prompt starts
with a Meko search for the design and decisions, and a second line (`mekoQuestionLine()`, added in
code like the other Meko lines, and to a repo's own `.agent-k/stages/review-pr.md` too) asks for
a Meko search before every answer about why the code is the way it is, saying which parts came
from Meko and which from the code. The system prompt's workflow line says the same, so it holds
after a resume or `/clear`. Nothing goes to GitHub, and nothing is committed or pushed, until the
user asks; then Claude posts one review with their comments inline
(`gh api repos/<owner>/<name>/pulls/<n>/reviews`), with the verdict they give. Comments left in
the Changes panel go out through the `review-pr-comments` template: they're the user's review
comments, to answer or draft as review comments, never changes to make or comments of Claude's own.

**Keeping up with the PR.** The session is polled like a PR & Review one (every 45 seconds and at
each turn end, `prWatch`), and nothing is sent to Claude by itself, so waiting costs no tokens.
New commits on the PR show in the footer and the stage bar (*3 new commits*, or *PR force-pushed*
when the commit reviewed is no longer in it), with one notification per new head. **Update to
latest** fetches the head, moves the review branch there (`reset --keep`) and sends the
`review-pr-update` prompt: a short summary of what changed since the commit reviewed so far and of
the threads that got replies, again without reviewing. It's refused while AgentK is mid-turn or
waiting on you, while the worktree has uncommitted files, or while the branch has commits of your
own on top of the commit reviewed (move them to another branch first); queued in
`pending_prompts` if Claude is busy. A review
you post is logged ("You requested changes on PR #21"). When the PR is merged or closed, a
notification says so, and **Close review** becomes the main button. It archives the session,
deletes the review branch (kept when it has commits of yours on top of the commit reviewed, or
when git can't tell) and `refs/agentk/pr/<n>`, and is refused while the worktree has uncommitted
files. Discard works as for any session; its warning counts only your own commits, not the
author's, which are on GitHub. A closed review ends in Done with someone else's PR, so the usage
counts never take it for one of your merged PRs. A Review a PR
session can't be moved to another stage, and no session can be moved into one.

### Moving a session

Between turns you can move a task to another stage from the **Move to another stage** dialog. On a
desktop it opens from any other stage in the stage bar (each is a button: *Move back to Code*), and
from Explore's **Move on…** gate, which starts at Design; otherwise it starts at the next step
(`defaultTarget`). On a phone it's in the ⋯ sheet, and the command palette has it too.
`POST /api/sessions/:id/move` (`MoveSessionRequest`: stage, and optionally prompt and task) runs
`Lifecycle.move`, in the same per-session queue as the PR polls and merges. A `kind` sent by a page
from before start stages is ignored. The rules are in `src/shared/stages.ts`, shared with the dialog:

- **Targets.** Explore, Design, Code or PR & Review, other than the step the session is in. Merge &
  Close is reached only through its gate, and a session in Merge & Close or Done can't be moved. A
  step before the current one that the session never went through, like Design for a task started at
  Explore and moved on to Code, shows as skipped.
- **Refused (409)** while AgentK is mid-turn or a gate action is in flight, during a merge, and for a
  session that still designs in plan mode. A move that sends a prompt is also refused while Claude
  is waiting on you (a design waiting for approval aside) or has exited.
- **Prompts.** A move to Explore, Design or Code can send that stage's prompt (`explore`, `design`,
  `code`), after the `moved` line saying which stage the session is in now, since the running
  Claude's system prompt still has the old workflow line (the next launch gets the new one). It's sent by default
  for Design, and for Code from Explore or Design; not when going back to Code from PR & Review, or to
  Explore. A session with no task yet (one started at Explore without a first prompt) is asked for
  one, which becomes its task. If the prompt can't be typed, the move stays and the error says so.
- **Side effects.** Entering Design clears `plan_mode`, so the edit guard is on and the design is
  written as markdown or an Artifact; an earlier design is kept, and can be approved again or
  revised. Leaving Design by hand clears *Design ready* and saves the design to Meko as an approval
  would; straight to Code, it skips the approval ("Design approval skipped"). Leaving PR & Review
  stops polling (only Review and Merge & Close follow the PR, and the footer stops showing its review
  and CI state); the PR stays open and linked. Queued review, conflict and CI prompts wait in `state.db`, and
  reviews that arrive meanwhile are picked up, when the session comes back; a conflict or CI prompt for a head
  that moved on is dropped then, as usual.
- **PR & Review** picks up the branch's open PR (any open PR from this repo) when there's nothing to
  push: the branch tip is in the PR and the worktree is clean. The session goes straight to Review,
  logged as "Picked up PR #7", or "Back on PR #7" for the PR it had before (not counted as another
  PR, and its review state, an AgentK approval say, carries on). Otherwise it goes to the PR stage with the PR prompt, as **Open PR** does, and goes back to
  where it was if the prompt can't be typed. The PR stage never takes the PR the
  session had before if it's been merged or closed since: it waits for the new one. Moving back to
  Code also gets a session out of a PR stage whose turn ended without a PR.

Moves are logged as stage entries with `moved_by = 'you'`. Sessions from before start stages may
still have `kind` events ("Changed type to Quick Code") in their history; no new ones are logged.
The usage stats count a session by where it started (see [Usage stats](#usage-stats)).

## Datapacks

- **Dedicated datapack (one per project).** It's created with the project, named
  `adlc/proj/<repo>/<project_name>` by default, or you can pick an existing one. It is the only
  place session transcripts and extracted memories are written. It can be switched to another
  datapack from the project's home; the old one then stays attached as shared, or is detached, and
  what was written to it stays there.
- **Shared datapacks (zero or more).** These are attached from your `datapack_list`: a team
  knowledge base, org conventions, another project's memory. They are read-only from this
  project's point of view, and one datapack can be attached to many projects.

Each session is launched with this environment:

```sh
MEKO_DATAPACK_ID=dp_7f3…                 # dedicated: capture + memory writes
MEKO_AGENT_ID=claude_code:fix-retry-backoff-3f9c1a2e  # the session: its slug and a hash of its id
MEKO_DATAPACK_IDS=dp_7f3…,dp_a01…,dp_c9e… # everything this session may read
AK_SESSION=billing-api--fix-retry-backoff
```

A short `--append-system-prompt` also lists the attached datapacks by name and id, so Claude
knows to pass the right `datapack_id` when it searches them, and says when to search: before
reading code for a new task, a review or a CI fix. Claude Code hides MCP tools until ToolSearch
loads them, so it names the query that does (`+meko context_search`, which matches the Meko server
under its own name or a plugin's). The first prompt of each task (`explore`, `design`,
`design-plan`, `code`, `review-pr`) and the review, conflict and CI prompts end with one line asking for that search.
A Review a PR session's prompts (`review-pr`, `review-pr-comments`) and system prompt also ask for a
search before every answer about why the code is the way it is (`mekoQuestionLine()`,
`mekoQuestionSystemLine()`). These lines are added in code (`mekoRecallLine()`, `mekoRecallSystemLine()` in `prompts.ts`), so
a repo's own stage prompts get them too, and only when the project has datapacks and Meko is on.

> **No Meko plugin change needed.** The Meko hooks already take `MEKO_AGENT_ID` over the folder
> name, and they read the datapack pin from `~/.claude/meko-capture/pin-<agent id>.json` (with `:`
> as `_`, e.g. `pin-claude_code_fix-retry-backoff-3f9c1a2e.json`). AgentK
> Local gives every session its own `MEKO_AGENT_ID` (`sessionAgentId()` in `meko.ts`: the slug,
> plus 8 hex digits of a hash of the session id, since a slug is only unique within its project)
> and writes that pin each time the session starts, so capture goes into the dedicated datapack.
> The agent id doesn't name the project: Meko reads memories across agents within a datapack, so
> it only says which session wrote a memory, and every session of a project, and the project
> opened again after a close, sees them all. AgentK's own writes for a session (designs, the
> shipped summary) go under the session's Meko conversation with the agent id the session runs
> with (`launchedAgentId()`, recorded in `sessions.meko_agent_id`); the project's conversation
> and its searches (the project home's, and the search at session start) use `agent-k-local`. A session launched before sessions had their own agent id keeps
> `claude_code:<project id>` for all of that until it's launched again. A pin stays for a day after
> its session is archived (the plugin's SessionEnd hook may still read it), then it's removed at
> startup or on the 6-hourly sweep; a restore pins it again. The old project-wide pins
> (`pin-claude_code_<project id>.json`) are never removed: the Meko plugin gives plain Claude Code
> in a repo the agent id `claude_code:<repo folder>`, so for a project named after its repo that
> file may be the user's own pin (one AgentK used to overwrite). The hooks read the pin when a session starts and keep it in the
> session's watermark, so after a switch a running session's capture stays in the old datapack. `MEKO_DATAPACK_ID` is set too, for when the hooks learn to read it.

> **Transcript capture can be turned off.** `[meko] capture_transcripts` in `config.toml`
> defaults to `true`; set it to `false` to stop sending transcripts, for example while Meko can't
> take the load (a restart applies a change). While it's off, the server doesn't run the plugin's
> `capture.js checkpoint` after each turn, at startup or when a session stops, and doesn't create a named conversation per session. (With it on, a
> server that is stopping waits up to 5 seconds for captures still running.) Sessions
> also get `MEKO_CHECKPOINT_INTERVAL=2147483`, so the plugin's checkpoint timer, which its
> SessionStart hook starts for a resumed session, never fires in practice: that's the longest
> interval Node's `setInterval` honours, and a larger one would fire every millisecond. With
> capture on, the launcher clears any inherited `MEKO_CHECKPOINT_INTERVAL`. The
> plugin's own SessionStart, PreCompact and SessionEnd hooks still run, so a running session shows
> up in Meko's conversations only once it compacts or ends. The shipped summary
> written at Merge & Close is a single memory, not a transcript, and is still sent.

> **PRs point back at Meko.** When the project has a dedicated datapack and Meko is on, Open PR
> appends to the PR prompt (after any repo or owner override) an instruction to end the PR
> description with two lines: `<Meko host>://<datapack id>;<datapack name>` and
> `meko_conversation_id:<id>`. The name is for people reading the PR; AgentK reads only the id
> (`datapackIdsIn()`, which also takes the line without a name, as PRs opened before it was added end).
> The host is the Meko web app's (`cloud.mekodata.ai` for Meko Cloud), or the MCP server's when a
> self-hosted Meko's web app isn't set (`mekoHost()` in `config.ts`). The conversation id comes from `conversation_create` keyed by the session's
> Claude session id, the same key the plugin's hooks use, so it names the conversation that holds
> the session (created and titled after the session if it doesn't exist yet, even with capture
> off). If Meko fails or doesn't answer within 10 seconds (connecting included), the PR goes out
> with just the datapack line. Open PR checks again that Claude is idle after that wait, before it
> types the prompt.

> **Folder trust.** Claude Code asks "Do you trust this folder?" the first time it runs somewhere
> new, and fires no hooks until that's answered. For a worktree, Claude keys that decision by the
> main repository, so the server marks the project's repo as trusted in `~/.claude.json` when the
> project is created, and checks again before each launch (other Claude processes can overwrite a
> fresh entry when they start, so it re-checks after a moment). If the prompt still appears (for a
> new, resumed or recovered session), the session shows *needs you* with an explanation.

> **Meko is optional.** Datapacks are how a project keeps track of its work, and sharing a project
> goes through its datapack, but AgentK works without them. Meko is *on* when there's a key and
> `[meko] enabled` isn't false (`MekoStatus.configured`). Off, AgentK doesn't talk to Meko: projects
> get no datapack, and sessions get no datapack, pin or Meko lines in
> their prompt (even a project that has a datapack). Claude Code's own Meko setup, in its settings,
> is the user's and is changed only when they ask (see *Claude Code on AgentK's Meko* below).
>
> - **At install:** the installer reuses `MEKO_API_KEY` from `~/.claude/settings.json` if one is
>   there, and otherwise asks for one, with **Enter to skip** (pointing to the Meko web app to
>   create an account or key). With no key, or `--no-meko` (which also turns Meko off), it goes on
>   with a warning, non-interactively too. A key it's given must work: without anyone to ask it
>   stops, and interactively it asks whether to go on without Meko (and turns it off, so a bad key
>   from the environment isn't used). With a key, it checks that Meko is set up for Claude Code:
>   the `meko-agent-skills` plugin installed and enabled, a `MEKO_API_KEY` in Claude Code's
>   settings (sessions don't inherit the installer's environment), and the `meko` MCP server
>   registered, both on this Meko server, with tokens it accepts (they may differ from AgentK's
>   key; the installer deliberately stops at accepted, and the server checks datapacks once it
>   runs). A key it took from its own environment (`MEKO_API_KEY`, not typed) isn't stored in
>   `config.toml`, so a different key in Claude Code's settings counts too: the service, with no
>   `MEKO_*` environment, would switch to it. If anything is missing or wrong it
>   offers, after showing what will change, to run Meko's own setup (`@yugabytedb/meko-mcp
>   --client claude-code`), fetched from npm, checked against the registry's integrity hash and run
>   with the bundled Node, with the key passed in the environment. Declining it (or, without
>   `--install-deps`, not being asked) is a warning with the command to run yourself; the Meko tab
>   in Settings can run it later. Once Claude Code's settings hold the key, `config.toml` keeps
>   none, so an older key there can't shadow it.
> - **At project creation:** the dedicated datapack is an existing one, a new one, or **No
>   datapack** (`DatapackChoice` `{ mode: 'none' }`), the default while Meko is off.
>   When Meko refuses a new datapack because the plan's datapack limit is reached (HTTP 402,
>   `FREE_TIER_DATAPACK_LIMIT` / `STANDARD_TIER_DATAPACK_LIMIT`), the server answers 402 with code
>   `DATAPACK_LIMIT` and the dialog offers **Open without a datapack**. One can be added later
>   from the project's home.
> - **At session start:** every session gets `MEKO_AGENT_ID`, and `MEKO_DATAPACK_ID` when the
>   project has a dedicated datapack. A project without one starts sessions like any other; design
>   saves, the shipped summary and search skip Meko for it, and while Meko is off, for every project.
> - **If Meko goes down:** AgentK doesn't poll Meko or show a banner for it; being down is rare. An
>   action that needs Meko (creating a datapack, sharing, answering an invite, adding a key) shows
>   Meko's error, the project home shows no datapack counts, and background syncs log a warning and
>   try again later. Running sessions keep going; the Meko hooks capture from the transcript on
>   disk, so they catch up once it's back (when transcript capture is on). A project can still be
>   created with **No datapack** and get one later.
> - **In Settings:** the **Meko** tab turns Meko off (`[meko] enabled = false`; the key is kept, so
>   a key from the environment or Claude Code's settings is ignored too) and back on (checked with
>   Meko first; a new key turns it on as well). It sets the MCP server URL (empty means Meko Cloud, unless the
>   environment or Claude Code's settings name one), the API key, and the web app and API URLs
>   (worked out for Meko Cloud; for any other Meko, datapack links and sharing stay off until the
>   web app URL is set, and the tab and the server log say so). The
>   key never goes to the browser: the tab shows its last four characters and where it came from.
>   With no key from anywhere, the tab offers **Sign up ↗** and **Log in ↗** in the Meko web app
>   (`/signup` and `/signin` with `return_to=/settings`, so they land on Meko's Settings, where
>   keys are made under Security), with the steps and the Settings URL written out (with Copy) for
>   a browser on another device. The links follow the MCP and web app URLs as typed, before
>   they're saved, worked out the way the server will (`mekoWebUrl()` in `src/shared/mekourls.ts`,
>   used by both): an empty field falls back to the environment or Claude Code's settings (the
>   settings carry `fallbackUrl` and `envWebUrl` for that), and Meko Cloud's MCP URL gives its web
>   app. For any other Meko they wait for the web app URL. **Add a Meko API key** on a project's
>   home shows the same links.
>   A new URL or key is checked with a Meko ping before anything is saved; then the MCP connection
>   is dropped (one still being opened with the old ones is dropped once it's up) so the next call
>   reconnects, and the invites waiting in Meko are synced again. An API URL that comes from a typed
>   API or web app URL is checked too, without the key (it must answer with Meko's JSON envelope,
>   `{ data }` or `{ error: { code, message } }`),
>   so a mistyped host never gets the key; a changed API URL also syncs invites again. A save
>   writes only the fields it changed, so a change that landed while its checks ran (Claude Code's
>   setup clearing the key) isn't undone.
>   Switching to another Meko host asks you to confirm, because existing projects' datapacks stay
>   on the old one. The tab also says whether Claude Code's sessions reach this Meko (the same
>   list as the banner, below), and when they don't, **Set up Claude Code with this Meko** runs Meko's
>   setup package as the installer does (without blocking the server; its output goes to the log).
>   New sessions pick that up; running ones keep what they started with.
> - **Claude Code on AgentK's Meko:** Claude Code keeps its own copy of the Meko server and token
>   in two places: its settings' `env` (`MEKO_MCP_URL`, unset meaning Meko Cloud, and
>   `MEKO_API_KEY`), which the Meko hooks read, and the user-scope `meko` MCP server in
>   `~/.claude.json` (its URL and `Authorization: Bearer` header, `${VAR}` expanded with the
>   settings' `env` over AgentK's environment, as a session sees it), which the tools call. A
>   variable set in neither (a session may get it from its login shell) leaves that token
>   unchecked, not missing. Local- and project-scope registrations aren't checked. `MekoStatus.claude`
>   (`mekoclaude.ts`) lists what keeps sessions off AgentK's Meko: the plugin, a missing key or
>   token, either place on another server (named by host only), and each token that isn't AgentK's
>   own failing on AgentK's server: turned down (HTTP 401 or 403), or unable to see a project
>   datapack that AgentK's own token sees (another account's token). Tokens are only tried on
>   AgentK's server, and only where Claude Code is set up for it. A token that couldn't be checked
>   (Meko slow or down) is listed apart and never raises the banner. The file checks are cheap;
>   token results (and AgentK's own datapack list, and `<[claude] command> mcp get meko` when
>   `~/.claude.json` has no user-scope server: exit 1 means none, and a command that couldn't be
>   asked is unchecked) are reused for 10 minutes. It's refreshed at startup, on a config
>   change, when Settings opens, at each session launch, and after Claude Code's setup (which
>   starts over). While the list isn't empty, a banner says what's wrong, with **Set up Claude Code
>   with this Meko** and **Details** (the Meko tab). AgentK never fixes it on its own: not at
>   startup, and not after a Meko tab change, which only checks again.

### Sharing a project

A project is shared by sharing its dedicated datapack. Meko shares a datapack as an **invite**: the
owner (or a maintainer) invites an email address with a role, Meko emails them, and they accept or
decline it. Roles are `contributor` (runs sessions and writes their own memories; the default),
`maintainer` (can also promote memories and invite others) and `viewer` (read only). Memories are
per user in Meko, so a member sees their own memories plus the datapack's shared knowledge
(promoted memories and knowledge-base files), not the owner's raw memories.

Meko's share routes (`POST /datapacks/:id/share`, `GET …/member-list`, `GET …/outgoing-shares`,
`POST …/invite/:id/resend`, `DELETE …/invite/:id`, `GET /datapacks/incoming-shares`,
`POST /datapacks/invite-action`) are REST, not MCP. `src/server/mekoapi.ts` calls them with the
same API key (a Meko personal access token), at `[meko] api_url` (by default the web app URL plus
`/api/v1`). `AppState.meko.sharing` is true when Meko is on and that API URL is known; it's worked
out from the config, not by asking Meko. Without an API URL the share routes answer 409, saying to
set the web app URL. Meko's own refusals of a share call (a bad email, an invite already pending,
no permission) come back with their status and message; a refused key (401) is a 502 that says to
check the key on the Meko tab; anything else is a 502. The invite sync keeps asking every five
minutes, but logs a failure to list the invites waiting (a refused key, say) once, until the error
or API URL changes or a sync works.

| | |
|---|---|
| Share | The Share dialog: the reviewers of the project's open PRs (below), emails, role (Viewer, Contributor, Maintainer, least to most; Contributor by default), the people with access and pending invites (Resend, Revoke). The Emails field takes a list separated by commas, semicolons or whitespace (new lines and tabs pasted from a spreadsheet become commas); `Jane Doe <jane@example.com>` counts as the address in `<…>`, and repeats are sent once. Anything that isn't an address stops the invite and is named. Addresses the dialog's lists show as members or pending invites are left out and named (while the lists load, or when they can't be read, those go to Meko, which refuses a repeat invite). The server invites up to 50 one after another (`emails` in `ShareDatapackRequest`) and reads the lists once: when some fail it answers with who was invited and who wasn't, and why (`ShareDatapackResult`), and the failed ones stay in the field; when none go through it fails with the first error. A refused key or an unreachable Meko stops the list, so a Meko that times out costs one 30-second wait, not one per address. Meko has no route to remove someone who has accepted; that's done in Meko. When an invite goes through but the lists can't be read again, it still says the invite was sent (`refreshError`). |
| An invite for you | A banner (and a notification) from `incoming-shares`: **Open as project** (creating the project accepts it), **Decline**, **Later**. A viewer invite gets **Accept** instead of Open as project. |
| A share you accepted, not yet a project here | A banner from `datapack_list`: **Open as project** and **Later** (a viewer share only explains, with **OK**). |

**Sharing with a PR's reviewers.** Once a session has a PR, its PR & Review step has **Share with
reviewers** (on a phone, in the ⋯ sheet), while the project has a dedicated datapack and AgentK
can share (`AppState.meko.sharing`). It opens the Share dialog with a **Reviewers on the
project's open PRs** section above the email form: for each open PR of the project's running sessions (in PR & Review or Merge & Close),
each in its own session's repo, the people asked to review and the people who have reviewed,
without bots, the PR's author or you, and the teams asked (whose members are invited by email);
the session's own PR comes first, and a person on several PRs is listed once
(`GET /api/projects/:id/pr-reviewers`, from GitHub's REST API, which says which accounts are
bots). Each person's email is the one you shared with for their GitHub login before (kept in `kv`
as `reviewer-email:<login>`, written when a share names `login`), else the public email on their
GitHub profile (read once an hour at most), else typed. Each row has the access (Contributor by
default: a viewer can't open the datapack as a project of their own) and **Share**, and shows
**✓ has access** when the email is a member's or has a pending invite. The project home's
**Share** opens the same dialog. The reviewer then opens it as a project and starts a
[Review a PR](#reviewing-a-pr) session.

When AgentK creates a datapack for a new project, it sets the datapack's description (MCP
`datapack_update`) to `AgentK Local project "<name>" · repo <remote>`, so whoever it's shared
with can find the repo. The remote is the repo's `upstream` when it's a fork with a network
upstream, else its `origin`: people share the upstream repo, not your fork. Both are read with one
`git config --get-regexp` call, which the repo scan uses too. Only a network remote goes in: a local path (`/home/me/src/x.git`, `../x`)
is left out, and credentials in the URL (`https://user:token@…`) are removed, again when a
description is read. The update runs in the background, so it never holds up the project; a
failure is logged. Meko shows the
description only once an invite is accepted, so a pending invite is matched to a repo by name.

`InviteService` (`src/server/invites.ts`) keeps the shares in the `datapack_invites` table, one row
per datapack, synced at startup, every 5 minutes, and whenever the Open project dialog opens or you
tap **Refresh**. The dialog makes one call, `POST /api/invites/check`, which lists the datapacks
once and syncs with that same list. Only project datapacks count, and only ones no live project
here uses. States: `invited` (waiting in Meko), `shared` (accepted, not yet a project), `later`
(hidden here; an invite stays in Meko), `used` (a project here has used it; never offered again,
even after that project is closed, unless it's invited again), and `baseline`. The first sync
records the shares that already exist as `baseline`, without banners or notifications, so an
upgrade doesn't bring a burst of them; they can still be picked in the dialog. Invites always show.
An invite that disappears from Meko (revoked, or answered elsewhere) is removed; one accepted
elsewhere becomes `shared` without another notification. `AppState.invites` carries the `invited`
and `shared` ones. When accepting, AgentK sends its own Meko account id when it knows it (the
`user_id` on the datapacks it owns); Meko otherwise uses the caller's account.

One sync runs at a time; a sync asked for meanwhile waits for it. A sync reads Meko, then writes
the table in one transaction; when Accept, Decline, Later, a new project or a Meko account change
changed things in between, that read is stale, so it reads again rather than undo the change (a
list handed in by `/api/invites/check` carries a stamp taken before it was read, for the same
check). Our Meko account id is only taken from a read that wasn't stale. A `datapack_list` reply
of an unexpected shape is an error, not an empty list, so it can't make every share look gone; and
a share accepted here isn't taken for gone for 10 minutes while Meko's list catches up. An invite that was
already accepted (by an earlier try whose project then failed, or in Meko) isn't accepted again,
so **Accept and open project** can be retried. A change to the Meko URL or key may mean another
account: the rows other than `used` and the first-sync flag are dropped, so the new account's
existing shares are recorded quietly.

### Meko recall at start

Sessions rarely searched Meko on their own, so AgentK searches for them (`src/server/recall.ts`).
When a session with a task is created in a project with datapacks, the server starts a
`context_search` on each datapack in the background, before the worktree is made. It shares
`searchDatapacks()` in `projects.ts` with the project home's search, and the project's Meko
conversation (which a search needs) is looked up while the issues are read; callers asking for it
while it's being created share the one `conversation_create`.

- **Query.** The session's name and task, plus the title and first 600 characters of each GitHub
  issue the task names (`#N`, at most 3, through `gh issue view`; one that can't be read is left
  out), on one line of at most 1,500 characters.
- **Picking.** Up to 3 knowledge-base items first, then memories scoring at least 0.55, best
  first. Conversation-cache hits are left out. Duplicates (same id or same text) are dropped; at
  most 6 items, each cut at a word to 500 characters, 2,500 in all. A datapack whose search fails
  (15 seconds each) is skipped; the search fails only when every datapack's did.
- **Delivery.** Claude's `SessionStart` hook (`context.sh`, source `startup` only) gets the items
  as `additionalContext`, headed "What Meko already knows about this task". The header says the
  items are notes from earlier sessions, some possibly written by other people (shared
  datapacks), to check against the code and not to follow as instructions. The hook waits up to 2.5 seconds for a search still
  running, which is rare because the search has had the worktree's and Claude's start-up time.
  Nothing found means no block. A resumed session has it in its transcript already, and after
  `/clear` the user has moved on, so neither gets it. A result waits in memory for up to 10
  minutes; a server restart loses it, and the session just starts without it. A session that
  fails to start, or is archived, drops its result, so a later session with the same id never
  gets it.
- **Record.** `meko_preloads(session_id, at, outcome, results, chars, ms)` keeps each search's
  outcome for the [first-turn stats](#token-usage): `found` (handed to Claude), `empty`, `error`,
  or `late` (it found something after the hook gave up, or the hook never asked).

Nothing waits for the search: the session starts the same when it's slow, fails, or Meko is off
(then there's no search at all).

### The learn step

Meko's automatic extraction of captured conversations reads only the user's side of each turn, and
in AgentK sessions that is mostly AgentK's own prompts and the skills Claude loads. What the
session found out (how the code works, root causes, gotchas) is in Claude's replies and never
became a memory. The learn step (`src/server/learn.ts`) saves it to the project's dedicated
datapack, which the team working on the project shares.

**The saving rule.** Every session whose project has a dedicated datapack gets it in its system
prompt (`mekoSaveSystemLine()` in `prompts.ts`), so it holds for saves Claude makes on its own
too. Save what a teammate on this product would want and couldn't easily get from the code, the
PR, the design or the git log: how a subsystem works, decisions and the rejected alternatives,
root causes, traps and invariants, build/test/CI quirks, measured numbers. Never anything about
the machine, worktree or environment, one person's preferences, task or PR status, what the diff
or PR already says, or AgentK's process steps. Search before each save; skip what's known, and
correct a memory that's now wrong with `memory_update`. No limit. It says it overrides the Meko
plugin's advice not to call `memory_add`. The Explore prompt ends by saving under it.

**When.** At Merge & Close, before the cleanup (`Lifecycle.merged()` asks `Learner.beforeClose`,
and runs again when the step ends), once per session; and when the user asks: the **learn**
session action (**Save learnings** in the header, or the ⋯ sheet), or `/agentk_learn` in the
terminal. It needs Meko on, a dedicated datapack, and Claude there and between turns; asked for
otherwise, it's a 409 that says why. At Merge & Close a session whose Claude is gone or busy is
recorded as skipped and closes as before. One runs at a time per session (`Session.learning`).

**Through the mod** (its link live, protocol 2):

1. The server records a `meko_learns` row and puts the job on the session's next poll.
2. The mod asks `$.model.fork` (the session's own transcript as the main thread last sent it,
   served from the prompt cache, every tool denied, nothing added to the transcript) for the facts
   as JSON (`EXTRACT_PROMPT`), and posts the reply, the model and the usage to `/mod/learn`.
3. The server parses the facts (the first JSON object in the reply), drops any that name the home
   folder, AgentK's home, the worktree or the repo path, and finds the 3 saved memories most like
   each (`MekoClient.similar`: `context_search`, memories only, in parallel). It answers with the
   duplicate check's prompt (`dedupPrompt()`): each fact numbered, with those memories and their ids.
4. The mod runs it on a small model (`$.model.complete`, `DEDUP_MODEL`, Haiku) and posts the reply.
5. For each fact the server adds it (`memory_add`, as "Gotcha (component): …", with metadata
   `{ source, kind: "learned", fact, component, session, learn }`), skips it, or updates the memory
   it corrects (`memory_update`, as the agent that wrote it; an update naming a memory that wasn't
   offered becomes an add). A fact the check left out is added.
6. The row closes with its counts, the session's timeline says "Saved N memories and corrected M in
   <datapack>", and the mod shows the same as a toast.

The whole step has 3 minutes (`LEARN_TIMEOUT_MS`); then, or when the fork or the check gives no
usable reply, it closes as `failed` with the step it reached and why, nothing more is written, and
Merge & Close goes on. A relaunch, exit or archive closes a running step as `interrupted`, and so
does a server restart (the job lived in memory). Each model call's usage is stored as a
`usage_messages` row (`message_id` `learn:<id>:extract|dedup`, agent `agentk:learn`), so the
session's tokens and cost include it, though it's never in the transcript.

**Without the mod** (older Claude Code, the rollout switch off, `[claude] mod` off, or a mod from
before protocol 2), AgentK types the `learn` stage prompt instead, and Claude searches and saves
under the saving rule itself. The step ends with that turn (`Lifecycle.onTurnEnd`), counted from
the Meko calls in the transcript during it.

**Record.** `meko_learns(id, session_id, trigger, path, started_at, ended_at, outcome, failed_step,
candidates, saves, updates, skips, lookups, errors, sent_chars)`, with `path` `mod`, `turn` or
`none` (skipped). The step's own Meko calls are in `meko_agentk_calls` with its id.

## Token usage

Claude Code writes every model response to the session transcript with its token counts. The
server reads new lines from each session's transcript, and from any subagent transcripts under
`<session>/subagents/`, after every turn (on the `Stop` hook), when a session ends or is archived,
and once a minute. It remembers the byte offset it reached, so each line is read once.

One response spans several transcript lines with the same message id, so usage is stored once per
message: `usage_messages(session_id, message_id, agent, model, at, input, output, cache_read,
cache_write, cache_write_1h)`. Totals per session, project, model and day are sums over that table, and archived
sessions keep counting toward their project. `cache_write_1h` is the 1-hour share of the cache
writes (the transcript's `cache_creation.ephemeral_1h_input_tokens`; without that split, every write
counts as 1-hour, which is what Claude Code uses).

**Cost.** Tokens are priced at Anthropic's API list prices (`src/shared/pricing.ts`: input, output
and cache read per model, with cache writes at 1.25× input for the 5-minute cache and 2× for the
1-hour one), from per-model sums. It's shown as **Est. cost**, what the tokens would cost on the
API: on a Pro or Max plan nobody pays per token, but raw token totals overstate cheap cache reads.
A model with no known price adds tokens but no cost; the cost then reads `≥ $…` (**Est. cost,
partial**), and the tooltip says how many responses were left out. Model ids match with a date
(`-20251001`), `[1m]` or `@…` suffix, and in Bedrock's form (`us.anthropic.claude-…-v1:0`).

**Meko calls.** The same transcript pass finds Claude's calls to the Meko MCP server
(`mcp__meko__<tool>`, or a plugin's `mcp__plugin_…_meko__<tool>`) and their results, in the main
transcript and subagents: `meko_calls(session_id, tool_use_id, agent, tool, kind, at, outcome,
results, input_chars, result_chars, model, context_tokens)`. A **lookup** searches or fetches
(`memory_search`, `knowledgebase_search`, `context_search`, `memory_get_all`, `memory_get_by_id`,
`conversation_get`, `artifact_get`); a **save** writes (`memory_add`, `memory_update`,
`memory_promote`, `artifact_put`, `conversation_add_message`); anything else (datapacks,
conversations) is counted as a call only. A result can come in a later read than its call, so a call
waits with no outcome until its `tool_result` is read. Meko wraps its JSON in `{"result": "…"}`; a
lookup **found something** when its items (`results`, `memories`, context_search's buckets, or a
single record; a plain-text reply unless it says nothing was found) aren't empty, and it failed on a
tool error or a reply with a non-empty `error`. A result too large to show, which Claude Code saved to
a file, counts as found at the size of its notice, since only the notice entered the context (reading
the file is Claude's own tool call); a call moved to the background found nothing. `model` and `context_tokens` (input, cache read and write)
come from the response that made the call. Sizes are characters ÷ 4. Apart from AgentK's own
search at start (`meko_preloads`, see [Meko recall at start](#meko-recall-at-start)), the Meko
plugin's hooks inject no recalled memory today, so the transcript is the rest of the record of what
Meko gave a session. The migration that added the table cleared the read offsets again, so older
sessions got their calls and 1-hour writes.

**Meko in the first turn.** The same pass keeps when each session's first turn ended:
`sessions.first_turn_end_at`, set once from the main transcript (subagents don't count). It's the
first of: a response with `stop_reason: "end_turn"`, an API error Claude Code wrote in the model's
place (model `<synthetic>`), or, once there has been a response, anything the user sent (a prompt,
a command, "[Request interrupted by user]"; meta lines, compaction summaries and tool results
don't count). So an interrupted or failed first turn ends there, not at a later turn's end. After
a `/clear`, a transcript read again from the start is only the current conversation's. Among
sessions whose first turn has ended, in projects with datapacks (the others had no Meko to
search), AgentK adds up about how many tokens Meko brought into that turn (characters ÷ 4, like
the tokens found): from lookups by Claude or a subagent that found something at or before that
time (`searchedTokens`), and from the search at start when it was handed over (the seed, `seedTokens`).
It also counts the sessions the search at start ran in at all (`startSearches`, whatever it
found), since older sessions predate it and a session started with no task skips it. They're `mekoFirstTurn` on the project and overview
usage, beside `meko`. The migration that added the column cleared the read offsets once more, so
sessions from before AgentK searched at start give the numbers to compare with.

**What Meko saved (an estimate).** Nobody sees the run where Meko wasn't there, so savings come
from a fixed formula (`src/server/mekosavings.ts`), not a comparison. Only searches that found
something count (`memory_search`, `knowledgebase_search`, `context_search`), plus each **seed**:
what AgentK's search at session start passed to Claude (`meko_preloads` with outcome `found`). A
seed is credited like a search made by the session's first main-agent response: *C* is that
response's context less the seed (a search's context doesn't hold its result yet), *k* counts the
responses after it, and its price is that response's model. A seed waits for that first response,
and it counts only in the savings, not in the lookups or tokens found. Without Meko, Claude
would have found the same thing by exploring: several tool calls, each re-reading the whole context
and adding file contents. The constants come from this host's transcripts (2026-09-29):

| Constant | Value | From |
|---|---:|---|
| `EXPLORE_CALLS` | 3 | Calls one search stands in for (a search of the code and two reads); sessions make a median of 14 exploring calls before their first edit |
| `EXPLORE_RESULT_TOKENS` | 2,000 | Tokens each adds: median Read ≈ 5,800, median Bash ≈ 150, so one read and two shell calls ≈ 6,000 |
| `CALL_OUTPUT_TOKENS` | 240 | Median output of a response that makes a tool call |
| `CARRY_TURNS` | 10 | Later responses counted as re-reading the extra content (the median is 211, but compaction cuts the context) |

For a search with a result of *R* tokens at a context of *C* tokens, followed by *k* =
min(10, later responses in the same conversation or subagent), with *X* = 3 × 2,000:

```
re-reads  (3 − 1) × C       cache-read price
output    (3 − 1) × 240     output price
content   X − R             1-hour cache-write price (2× input)
carry     (X − R) × k       cache-read price
```

The saving is the sum, in tokens and in USD at the calling model's price. A search (or seed) on a
model with no known price counts in the tokens but not the dollars, and the tile says how many
searches and seeds were left out.
An index on `usage_messages(session_id, agent, at)` keeps counting *k* cheap. A result over 6,000
tokens makes the content and carry terms negative, and they're kept. Session, project and overall
figures are plain sums, so the pages agree. With a context of 216k, a 1,420-token result and 10
later responses, one search saves about 483k tokens, almost all of them cache reads, so the dollar
figure is much smaller than the token count. The estimate can't show that Meko didn't help, which is
why it's labelled **Est.** A comparison of sessions with and without Meko finds was tried and
dropped: too few sessions to compare, no chance deciding which sessions used Meko, and nothing to
compare with once every session does.

**What shipped.** A merged PR is the session's `merged` event, or a Done session with a PR (Merge &
Close only ends in Done after a merge), counted once per session. A push
straight to the base branch is found in the same transcript pass: a Bash tool result that is a
push (git push's `To <remote>` line, or Claude Code's `gitOperation.push` marker, which also covers
output cut short with `| tail`) with a fast-forward `old..new  src -> <base>` ref update is stored
in `pushes(session_id, range, ref, at, commits, file_types)`, and `git rev-list --count old..new` in the
project's repo fills in `commits` (`-1` when git can't tell; the push still counts), and
`git diff --numstat -z old new` fills in `file_types`, what it changed per file extension, for the
[usage stats](#usage-stats) (NULL when git can't tell, and for pushes found before it was added). Fetch and pull
output (`main -> origin/main`, under `From`), forced updates and new refs don't match. The
migration that added the table cleared the read offsets, so every transcript was read once more
(usage rows are upserts) and older pushes were found; transcripts of archived sessions are read at
startup, one session per tick.

**Plan limits.** On Pro and Max plans, Claude Code passes the account's 5-hour and weekly usage
(`rate_limits.five_hour` / `seven_day`: percent used and reset time) to its status line command.
Sessions are launched with AgentK Local's own status line (`run/statusline.sh`, set in the
`--settings` file). It sends inputs that carry limits to the server over the hook socket, in the
background, and then runs the user's own status line command with the same input, so the terminal
looks unchanged. That command is found at launch (local project settings, then project, then user
settings) and passed as `AK_USER_STATUSLINE`; sessions launched before this existed use the
user-level command saved in `run/user-statusline`; its `padding` and `refreshInterval` are copied.
If that command is Meko's `meko-statusline.sh`, only the command it wraps (`--inner-cmd`, or
`MEKO_INNER_STATUSLINE`) runs: Meko's "Meko: <datapack>" suffix would repeat what the session
screen already shows, since sessions always use the project's datapack.
AgentK's mod reports the same windows after each turn (`session.measure`), whatever the status line does.
All sessions share one account, so reports are merged per window: a window that has reset is
ignored, a later window replaces an earlier one, the higher use wins within a window, and a report
missing a window keeps the last one, so an idle session re-running its status line with an old
reading changes nothing. The reading is kept in the `kv` table, and the rail shows each window with a bar, its reset time and how old the reading is,
turning amber at 80% and red at 95%. A window disappears once its reset time passes. The numbers
only move when a session gets a reply, so the rail says when they were last reported.

- **Where it shows.** The session footer and tab tooltip show the session's total and output
  tokens. The rail tooltip shows the project total. The project home has a **Token usage**
  section: headline numbers with **Est. cost**, a 14-day chart with a table view, the Meko section,
  and breakdowns by session (with a **Meko** column, found / lookups, when any session called Meko)
  and by model. **Usage** in the rail (and the command palette) opens `/usage`: totals with Est.
  cost, merged PRs and commits pushed straight to a base branch, a 14-day chart, the Meko section
  for every project, and breakdowns by project (current ones first, then deleted ones, which keep
  their sessions and usage; each row shows its repo folder and when it was last active, and has
  **Cost**, **Meko** and **Est. saved** columns, which a phone leaves out) and by model. Every
  column header sorts its table; tapping it again reverses the order.
- **The Meko section** is the same on both pages (`MekoUsageSection`): the Meko logo (black in the
  light theme, white in the dark) and tiles for lookups, lookups that found something, saves,
  **1st-turn tokens** (the tokens Meko brought into first turns, `≈ 15.3k`, with the split on one
  more line, `Seeded ≈ 3.3k · Searched ≈ 12k`, or `Seeded none yet` before AgentK's search at
  start has run in any of them; a dash before any first turn has ended; the
  tooltip says how many sessions the search at start ran in; `firstTurnText` in
  `src/web/util.ts`), tokens found, and **Est. saved** in USD and tokens (searches and seeds), green for a saving
  (a negative estimate is in the normal text color, and a dash means no search has found
  anything and no session was seeded). It shows once there's any
  Meko call or seed, or before that while Meko is on (and, on the project home, the project has a datapack).
- **What's counted.** Four numbers, kept separate: uncached input, cache writes, cache reads and
  output. Cache reads dominate long sessions, so the headline total is shown next to output. The
  cost is an estimate at API list prices, not a bill.
- **In Meko.** When a task is cleaned up, its token totals are included in the "shipped" summary
  written to the project's dedicated datapack.

## Usage stats

Each install sends a small report of counts once a day (every hour in its first day) to a
collector run by the AgentK Local team, so we know how many installs are active and how much work
goes through them. The collector is a
separate service (`services/ak-local-call-home` in yugabyte/agent-k). What it receives is
`CallHomeReport` in `src/shared/types.ts`, and [`docs/call-home-report.schema.json`](call-home-report.schema.json)
is the same contract as a JSON Schema. The collector keeps an identical copy
(`internal/report/contract/call-home-report.schema.json`), and its tests tie that copy to its
types, enums, patterns and caps; change both together. A unit test here checks the reports the
server builds against the schema. `schema` in the report goes up when a
field changes meaning or is removed; adding a field doesn't change it. The collector stores an
enum value, model id, extension or version it doesn't know as `other`, and drops (only counting)
fields it doesn't know, rather than refusing the report, so an install that is ahead of it still
reports.

**What's in it** (`src/server/callhome.ts`, `buildReport`):

- *The install:* a random `installId` (made on first use, kept in `kv`; one per AgentK home, so
  hosts sharing a home count once), the version and update channel, platform, CPU, Node, tmux and
  Claude Code versions (`tmux -V`, `claude --version`; `''` when they can't be read; our version or
  Node's, when the collector wouldn't keep it, such as a `git describe` build or a Node nightly, is
  cut to its leading numbers, else `0.0.0`), local or remote access, the service manager, whether Meko is on (and where its key comes from, and the
  sharing: `api` when AgentK can share through Meko's API, `none` when Meko is off or its API URL
  isn't known), and the UTC day of the first
  report.
- *All-time totals:* projects (current, deleted, with a dedicated datapack, shared datapacks
  attached, distinct repos), sessions (by kind, open ones by stage; a task counts as `explore`,
  `plan` or `quick` by the stage it started in, Explore, Design or Code), what shipped (merged PRs,
  direct pushes and their commits as on the Usage page, plus the commits and lines of merged PRs),
  file types, tokens, idle worktrees, Meko use (below), and the tokens Meko brought into first
  turns (the Usage page's numbers).
- *Each UTC day* of the last 7 (30 in an install's first report, as far back as session events are
  kept): active projects and sessions, responses, projects opened (new, or opened again) and closed
  (from `project_events`, so a close a reopen undid still counts), sessions created
  by kind and archived, stage entries, PRs opened and merged with their commits and lines, direct
  pushes and commits, reviews handed to Claude, CI failures, review comment batches, recoveries,
  resumes, reused worktrees, file types, tokens by model, and Meko use (left out of a day with none).
- *File types* are counts per file extension: files changed, lines added and deleted, from merged
  PRs and direct pushes. When a PR is merged, one `gh pr view <url> --json
  commits,additions,deletions,files` call adds its size to the `merged` event's payload; for a
  direct push it's `git diff --numstat -z`. Paths are reduced to extensions right away
  (`src/server/filetypes.ts`): the last dot-suffix, lowercased; `none` for no suffix or a
  dotfile; `other` for a suffix that isn't 1 to 10 letters and digits. Each list keeps the 25
  largest and sums the rest into `other`. GitHub lists up to 100 files per PR.
- *Meko use* is the Usage page's Meko numbers (`src/server/mekousage.ts`): calls, lookups that
  found something, came back empty or failed, saves, the tokens found lookups brought in, and the
  estimated savings (searches and seeds credited, tokens, and dollars in whole cents). It adds calls
  by tool, naming only the lookup and save tools (every other Meko tool counts as `other`), and
  AgentK's searches at session start by outcome with the milliseconds they took in all. Calls go on
  the day they were made, seeds and searches at start on the day the search ran. Added since
  (optional, so older installs' reports still check): about how many tokens Claude's calls sent to
  Meko (their input, characters ÷ 4), also by tool; [learn steps](#the-learn-step) by trigger,
  path and outcome, the facts they proposed, added, corrected and skipped, their Meko searches,
  failed calls and tokens sent, and their model calls' tokens (the fork, the duplicate check, and
  typed turns) with their price in whole cents; AgentK's own Meko calls (`meko_agentk_calls`,
  recorded in `MekoClient.call`: searches at start, what shipped, designs, the learn step's) with
  failures, tokens sent, time taken and the 9 most used tools (the rest as `other`); and the Meko
  plugin's transcript captures AgentK ran (`meko_capture_runs`: runs, failures, time). What a
  capture sends isn't counted: AgentK can't see it. Each goes on the day it began, and is left out
  when there was none.

Never sent: project, session, repo, branch, datapack or file names, paths, prompts, designs,
transcripts or code, PR titles, URLs or numbers, user names, emails, GitHub logins, host names and
the server name, Meko keys or ids, and Meko queries, results and tool inputs. Model ids that aren't plain ids (a Bedrock ARN carries an
account number) are sent as `other`.

**Sending.** An hourly check (the first one 10 to 60 minutes after start, at random, so a release
doesn't make every install report at once) sends a report when the last one went out 23 hours ago
or more (`kv` `call_home_last_sent`), so restarts and updates don't send again. A new install
reports sooner and more often, so the collector sees it on the day it starts: until a day after
its first report (`kv` `call_home_first_sent`, the time of the check that sent it), the first
check comes 1 to 5 minutes after start (or when the next report is due, if that's later, so a
restart doesn't skip one), and a report goes out when the last one is 55 minutes old, so at every
hourly check. Reports are stamped with the check's time, so the time a send takes doesn't push
the next one past its check. Installs that reported before `call_home_first_sent` was
kept have none and stay daily. A report over the collector's 64 KB (a busy install's 30-day first report, with every day's file types and models
full) drops its oldest days until it fits, since the collector would refuse it every hour from then
on. It POSTs the JSON
to the collector's report URL, as given, with a 15-second timeout; a failure is logged once and tried again an hour
later, and nothing else waits on it. The collector upserts each day on `(installId, day)`, so a
resend changes nothing and a missed day is filled in by the next report. The URL is
`[call_home] url`, or else the one release builds carry: a `CALL_HOME_URL` file next to `VERSION`,
written by `scripts/package-release.sh` from the `CALL_HOME_URL` repo variable, so it never
appears in the repo. Dev servers and tests have neither, so they send nothing. Node's `fetch`
ignores `HTTPS_PROXY`, so an install behind a proxy doesn't report.

**Turning it off.** It's on by default. The **Send anonymous usage stats to YugabyteDB** checkbox
in Settings → Account (under the password forms, or the local-access note) writes
`[call_home] enabled = false`; from then on no report is built or sent, without a restart.

**Downloads** aren't sent from here: the collector reads every release asset's download count
from the GitHub API each hour, so a pruned release loses at most an hour of downloads.

**The collector's side of the contract.** `POST /v1/report` takes the report with no auth and
answers 2xx (anything else counts as a failure); it should cap the body at 64 KB, limit reports per
`installId` per day, check the schema, and store no IP addresses.

## Login and security

The terminal is a shell running as your Unix user, so the web login protects the same thing as
your SSH key. Each install serves exactly one owner. Sharing the app means other people install
their own copy.

- **Local or remote access.** The installer asks which you want. *Local* listens on 127.0.0.1
  only, over plain HTTP with no login: a request needs no cookie when the server listens only on
  loopback, the connection comes from loopback, and its `Host` header is `localhost`, `127.0.0.1`
  or `[::1]` (so a DNS-rebound page, which sends its own host name, gets nothing), with no
  `Forwarded`/`X-Forwarded-*`/`X-Real-IP` header from a proxy, and the browser doesn't mark it as
  sent by another site (`Sec-Fetch-Site` other than `same-origin` or `none`, or an `Origin` that
  isn't this one). That last check does the SameSite=Strict cookie's job: an `<img>` on another
  site gets a 401, not an answer it could time. An existing login is kept for a later switch to
  remote; the server's startup log says it isn't needed meanwhile. *Remote*
  listens on every address (`0.0.0.0`), over HTTPS with a login, and then the login is required
  even from localhost, so an SSH tunnel or a reverse proxy on the host can't skip it. The default
  is remote over SSH or on Linux with no display, and local otherwise. Local access trusts
  everything that can connect to 127.0.0.1, every user of the computer and any reverse proxy on it
  included: on a shared host, or behind a proxy, choose remote.
- **First run.** With remote access, the installer prints a one-time setup link:
  `https://host:7777/setup?token=…`. You open it, choose a username and password, and can
  optionally enrol TOTP. The token stops working after it is used. With local access it prints
  the address instead; `agent-k-local setup-link` still makes a login for a later switch to remote.
- **Forgot password.** The sign-in screen's **Forgot password?** says how to get back in: run
  `agent-k-local setup-link` on the host and open the link it prints. There is no email or
  self-service reset in the browser, on purpose: the one login guards a shell on the host, so a
  reset needs someone who can already run commands there. While a link is outstanding the setup
  screen shows (`AuthStatus.reset` says it replaces an existing login, so it reads **Reset your
  login**); using it replaces the username and password, turns off TOTP and signs out every
  browser. A valid link works even while sign-in is rate limited, so a locked-out owner can use
  it at once. A browser that is still signed in keeps working until then and doesn't see the setup
  screen. Projects and sessions are untouched.
- **Sessions.** Passwords are hashed with scrypt. After login you get an `HttpOnly`,
  `SameSite=Strict` cookie (`Secure` over HTTPS) that lasts 14 days, renewed as you use the app.
  Changing the password signs out every other browser. Access checks key on the route a request
  matched, so an encoded path (`/%61pi/…`) gets the same checks as `/api/…`. The API only takes
  JSON bodies, so a plain HTML form can't post to it. Every WebSocket upgrade checks the cookie (or
  local access) and refuses a cross-origin `Origin` header; open sockets close (code 4401) when their login is signed
  out, and the server pings them every 30 seconds to drop ones whose network went away. The page
  checks its events socket too, since a phone often never hears that the server dropped it: after
  30 seconds without a message, and whenever the page is shown again or comes back online, it sends
  a `ping`, and opens a new socket (which starts with the current state) if no `pong` comes back
  within 5 seconds, or at once if the socket is already closing. A new socket gets 10 seconds to
  open and 30 more for its first state. Each of these deadlines that passes doubles the next ones
  (up to 8 times), so a slow link whose snapshots take longer still gets through; a quick answer
  to a ping sets them back (`src/web/events.ts`). A browser
  that falls behind doesn't make the server buffer without limit: its terminal stops reading from
  `tmux attach` until it catches up (tmux copes with a slow client), and it skips state snapshots
  until it has drained, then gets the current one.
- **TLS is mandatory.** Any address other than localhost requires HTTPS. You can point it at your
  own certificate from the corp CA, or let it generate a self-signed pair; `agent-k-local status`
  shows its fingerprint so you can trust it once on your laptop. The self-signed pair lasts 825 days and is made again
  at startup once it's within 30 days of expiring (trust the new fingerprint then). No HSTS header
  is sent: it would apply to every port of the host, including your other dev servers.
- **Guard rails.** Failed logins are rate-limited: 5 per 15 minutes from one address (one /64 for
  IPv6, since a client usually holds a whole one), and 50 across all clients, after which everyone
  waits (`agent-k-local setup-link` on the host still gets you in). A wrong username takes as long
  to refuse as a wrong password. A TOTP code works once. There is an
  audit log of logins, failed logins, account changes and Meko settings changes. The Meko key is
  write-only from the browser's side: no response carries it. Hooks use the local unix socket with a per-session
  token, never the network port. The Design-stage edit guard asks the server over the same socket;
  if it gets no answer it lets the edit go ahead, so Claude is never stuck.

*Later: passkeys (WebAuthn), and single sign-on with Google Workspace or another OIDC provider.*

## Surviving crashes, restarts and updates

Two systemd user units run with lingering enabled, so they start at boot without anyone logging
in:

- `agent-k-local-tmux.service` owns the tmux server on socket `agentk`. It is never restarted by
  updates.
- `agent-k-local.service` is the web server, with `Restart=always`. Updates restart only this unit.

Keeping tmux in its own unit is the key point. If tmux were a child of the server's unit, systemd
would kill every Claude session on each update.

**On macOS** the same two services are LaunchAgents in your `gui/<uid>` domain
(`~/Library/LaunchAgents/com.yugabyte.agent-k-local.plist` and `….tmux.plist`), with `RunAtLoad`
and `KeepAlive`. They start when you log in, not at boot: Claude Code keeps its login in the
Keychain, which is only unlocked during a desktop login, so the installer refuses to run without
one. launchd wants a job's main process to stay in the foreground, so the tmux job runs `tmux -D`
(tmux 3.2 or newer), which also keeps running with no sessions; the server adds the `_ak`
keepalive session with `tmux -N`, which never starts a second server. The server job sets
`AbandonProcessGroup`, so stopping it never takes a tmux down with it. Both jobs set
`LANG=en_US.UTF-8`, because launchd sets no locale and tmux clients need a UTF-8 one. Updates
restart the server with `launchctl kickstart -k`, or by exiting and letting `KeepAlive` bring it
back; reinstalling never reloads a loaded tmux job.

All of this sits behind one interface in `src/server/service.ts` (install, start, state, remove,
boot check), with a systemd and a launchd implementation picked by platform.

### One server per home

Two servers on one `~/.agent-k-local` break each other: each binds the hook socket over the
other's (a Unix socket bound on one host can't be reached from another, so hooks are lost and
sessions stay on Starting), and both write the same SQLite file. This happens when the home is on
a file system other hosts mount, like a home directory shared over NFS: every host you log in to
sees the same `~/.config/systemd/user`, so its systemd starts the enabled units too.

- **The units run only on the install host.** Both systemd units carry `ConditionHost=` with the
  host name at install time. On any other host systemd skips them: nothing starts and nothing is
  logged. Renaming the host means running `agent-k-local install` again, which rewrites the units
  (the one-line installer stops early on an install it finds set up).
- **A lease backs it up** (`src/server/instance.ts`). Before `serve` opens the database or binds
  anything it takes `server.lock`, which holds the host, pid, version, port and a heartbeat it
  rewrites every 10 seconds; a lease not rewritten for 45 seconds is free. A lease from another
  host that is still beating refuses the start at once. On this host a live pid proves nothing
  (after a crash the pid may belong to another process), and an old server may still be exiting
  (an update's restart), so the start waits until the lease is released, its pid exits or it
  goes stale, and is refused only if the lease beats meanwhile. A refused start says which host
  holds the home, writes nothing to the shared log, and exits with status 75. The server unit has
  `RestartPreventExitStatus=75`, and a refused start is taken back off an update's start count, so
  it can't cause a rollback. A server
  that finds another one named in the lease (it stalled for longer than 45 seconds) stops. File
  locks aren't used: they aren't reliable across NFS clients.
- **`install` refuses** while a live server on another host holds the home, since it would pin the
  units to the wrong host.

| Event | What survives | What happens |
|---|---|---|
| A phone's page goes to the background, or its network changes | Everything | The server drops the page's sockets once they stop answering its pings. When the page is shown again, it pings its events socket and opens a new one if there's no answer, so it shows the current state instead of the one it had when it was hidden. After 30 seconds or more hidden, it also lets go of its terminals and reconnects the ones on screen, even if the phone kept its timers from running. |
| Server crash or update | Everything | systemd (or launchd) restarts the server. It matches `state.db` against the running tmux sessions, and browsers reconnect automatically. An open page that finds the server on another version reloads itself (see [Releases and auto-update](#releases-and-auto-update)). |
| A second server starts on the same home (another host that mounts it) | Everything | The units don't start on other hosts, and a server started by hand refuses while the lease is held. If the hook socket goes missing anyway, the running server binds it again within 10 seconds. |
| tmux crash | Worktrees, transcripts, state | The unit restarts tmux, and the server relaunches every session with `desired: running` using `claude --resume`. |
| Host reboot or crash | Worktrees, transcripts, state | Both units start at boot (on macOS, when you log in) and the same recovery runs. Sessions show as *recovered* until you interact. |

```mermaid
sequenceDiagram
  participant SD as systemd user
  participant T as tmux unit
  participant S as server
  participant C as claude
  SD->>T: start at boot (linger)
  SD->>S: start
  S->>S: open state.db
  S->>T: list sessions
  loop each session with desired = running and no pane (not one still being created, launched or stopped)
    S->>S: verify worktree, recreate from its branch (or origin/branch) if missing; fail if the branch is gone
    S->>T: new-session in worktree
    alt session was mid-turn
      T->>C: claude --resume session-id "continue prompt"
    else idle or waiting on you
      T->>C: claude --resume session-id
    end
    C-->>S: SessionStart hook
    S->>S: mark as recovered
  end
```

A turn that was in progress when the host died is lost, but the conversation up to that point
comes back. **Auto-continue is on by default**, with guards:

- **Only sessions that were working.** The session's status is written to `state.db` on every hook
  event, with full sync. A session whose last status was *working* is resumed with a continue
  prompt. Sessions that were idle or waiting on you come back exactly as they were.
- **Check before continuing.** The prompt says the host restarted mid-turn. It asks Claude to check
  the worktree and the last commands (and, in the Review stage, the PR threads) before continuing,
  so finished steps aren't repeated and replies aren't posted twice.
- **No crash loops.** A session is auto-continued at most once every 15 minutes. Once it has been
  auto-continued twice within an hour, the next mid-turn recovery parks it as *needs you* instead. Each project can turn
  auto-continue off.
- **Failed restarts back off.** If a session can't be restarted (for example, its worktree can't
  be recreated), it shows *exited* with the reason and is retried every 5 minutes; if its branch
  is gone, it waits for you to resume or discard it. A tmux error other than "no server running"
  skips that check rather than being read as every pane being gone, and a live pane is never
  replaced.

## Releases and auto-update

- **CI (on every PR).** Typecheck, lint and unit tests, on Linux and macOS. Integration tests run
  against real tmux and git, with a stub `claude` binary that fakes the TUI and fires the hooks.
  A separate `PR title` workflow checks the title with `scripts/check-title.sh` and reruns when
  the PR is edited. Changes reach `main` only through PRs, squash-merged once CI and the title check pass.
- **Versioning (on push to main).** Versions are `0.YYYYMMDD.N`: the UTC date, and `N` counting
  from 0 for each release that day. Commit subjects are `<type>[(<component>)][!]: <summary>`
  with one of nine types (`scripts/check-title.sh`), and PRs are squash-merged with the PR title
  as the subject. A push with any `feat`, `fix`, `perf` or `deps` commit since the last release
  runs the tests and cuts the next version, and those subjects become the release notes; `ci`,
  `test`, `docs`, `nit` and `chore` don't release.
- **Build matrix (after the tests).** Self-contained tarballs are built for `linux-x64` and
  `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`. Each bundles the Node 22 runtime,
  node-pty (built from source: gcc-toolset-13 on Linux, Xcode's clang on macOS) and the web assets.
  The installer and the updater pick the tarball for their platform. The updater skips releases
  with no build for its platform; the installer takes the latest release (or `--version`) and stops
  with a clear message if it has none. Releases since macOS support always include the macOS
  build: publishing waits for it, and only a rebuild of an older tag goes without. Each release also includes `SHA256SUMS`, a build provenance attestation
  and `install.sh`. Release tarballs carry the usage stats collector's URL (`CALL_HOME_URL`, from the repo variable of that name) when it's set ([Usage stats](#usage-stats)). Afterwards, releases beyond the newest 5 in each lane (`stable`, or the
  prerelease identifier such as `beta`) are deleted; their tags stay.

> **glibc.** RHEL 8 devservers ship glibc 2.28, and node-pty is a native module. Linux builds run
> in an AlmaLinux 8 container rather than on `ubuntu-latest`, so the binary loads on older hosts.

```sh
# install (one line, no root). The repo is internal for now, so this uses gh.
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh

# what it does
#  0. stops, saying so, if AgentK Local is already installed in AK_HOME and set up: app/current,
#     config.toml, ~/.local/bin/agent-k-local, and both services enabled (systemd) or loaded
#     (launchd). Updates are offered in the app, and `agent-k-local install` redoes the setup.
#     --reinstall, --deps-only, a --version other than the installed one, or options for
#     `agent-k-local install` (other than --non-interactive) go on
#  1. checks for tmux (≥ 2.7 on Linux, ≥ 3.2 on macOS), git, curl, gh and Claude Code; offers to
#     install whatever is missing (it shows the commands and asks first): brew on macOS, the
#     package manager with sudo on Linux (apt-get, dnf, yum, zypper, pacman or apk), gh on Linux
#     from its GitHub release (checksum-verified) into ~/.local/bin, and Claude Code with
#     Anthropic's installer (claude.ai/install.sh, which publishes no checksum) into ~/.local/bin;
#     with GITHUB_TOKEN set, a missing gh is only installed with --install-deps (downloads use
#     curl and the token). It checks the tools themselves afterwards: Homebrew exiting with an
#     error once they work (it builds from source on a macOS it no longer supports) is a warning,
#     while a failed Linux package manager stops it; then offers `gh auth login` and `claude auth
#     login` for whichever isn't signed in. Sessions add ~/.local/bin to PATH after the login profile runs. --install-deps installs without asking,
#     --no-install-deps never installs, --deps-only stops after this step
#  2. unpacks to ~/.agent-k-local/app/<ver>, points app/current at it, then runs `agent-k-local install`:
#  3. asks for your repos folder (default ~/code), access (local: 127.0.0.1, no login; remote:
#     0.0.0.0, HTTPS and a login; remote by default over SSH or with no display) and port
#  4. Meko (optional): verifies the API key (reuses MEKO_API_KEY from ~/.claude/settings.json, or
#     asks; Enter or --no-meko skips it) and offers to run Meko's setup for Claude Code if its
#     plugin, MCP server or key is missing (declining is a warning)
#  5. picks the database location (local disk if ~/.agent-k-local is on NFS), writes config.toml,
#     sets up TLS
#  6. installs both background services: systemd user units with linger if allowed,
#     or LaunchAgents on macOS (needs a desktop login)
#  7. prints the one-time setup link (remote access), or the address (local access)
```

**Update loop.** The server checks the GitHub Releases API at startup and every 6 hours
(`update.interval_hours`). The channel can be *stable*, *prerelease* or *off*. When a newer version appears, the rail shows an
"Update to vX" banner (and Settings an **Update** button); a version that was rolled back is never
offered again. When you click it, the server:

1. downloads the tarball and verifies it against `SHA256SUMS`, and against its build provenance
   attestation (with `gh`) when `[update] require_attestation = true`;
2. unpacks it next to the current version and runs its self-test;
3. snapshots the database with `VACUUM INTO state.db.bak-<old-ver>`, lets the new version run its
   migrations on start, and points `app/current` at the new version;
4. restarts only the web server unit. Sessions keep running;
5. if the new version fails to come up three times in a row (the service manager restarts it) before
   it has been healthy for 5 minutes, points back to the previous version and marks the new one as
   bad. A deliberate stop (SIGTERM, as for a service restart) takes its start back off the count,
   so restarting by hand in that window isn't mistaken for a crash loop.

Once a version has been healthy for 5 minutes, the server deletes every installed version under
`app/` except the running one and one to roll back to by hand: the version it updated from, or
(after a manual switch) the newest older one. It also deletes the database snapshots of the versions
it removes. Nothing is removed while an update is being installed or switched to, and never the
version `app/current` points at. Each version is about 180 MB, most of it the bundled Node runtime.
The "Updated to" notice shows from the new version's first boot, without waiting for the healthy
check.

The rail shows "Updated to v0.4.1" with a link to the changelog. Updates never need a maintenance
window, because they never touch tmux.

**Open pages reload themselves.** The server stamps its version into every page it serves
(`<meta name="ak-version">`, and the page is sent with `Cache-Control: no-cache`). When a page's
event socket reconnects to a server on another version (an update, a reinstall or a rollback), the
page reloads, keeping its route, so it never keeps running the old UI (`src/web/reload.ts`,
`hooks/useReload.ts`). It waits while a reload could lose something: a dialog, sheet or popover is
open, a session's terminal holds images for Claude (`hasHeldImages()` in `terminals.ts`, whether
or not that session is on screen), a text field has focus, or a textarea holds unsent text. Fields whose text survives a reload
are marked `data-reload-safe` and don't count (the terminal compose box keeps its draft in
`sessionStorage`), and neither does xterm's input. While it waits, a banner offers **Reload**, and
it checks again when focus leaves a field, the tab is shown or hidden, and every 5 seconds. A tab
reloads at most once for a given version (remembered in `sessionStorage`), so a mismatch a reload
can't fix shows the banner instead of looping. Without the meta tag (the Vite dev server), the
first version the server reports counts as the page's.

## Tech stack

| Layer | Choice | Why |
|---|---|---|
| Server | Node 22 + TypeScript, Fastify, `ws`, `node-pty` | node-pty builds on the target host. Using one language across server and web keeps it simple. |
| Web | Vite + React, xterm.js with the fit and web-links addons | xterm.js is the standard browser terminal. React handles the rail, tabs and dialogs. |
| State | SQLite via the built-in `node:sqlite`, WAL, `user_version` migrations | A synchronous API suits a single-writer server, and it needs no native module. |
| Processes | tmux on a private socket, in its own background service (a systemd user unit on Linux, a LaunchAgent on macOS) | tmux keeps sessions alive when the server restarts, and you can still attach from iTerm. |
| Integrations | Meko MCP over HTTP (optional) and its REST API for sharing, Claude Code hooks, git worktrees | Meko holds projects' datapacks when it's on. The hooks and git are thin adapters. |
| Release | GitHub Actions, date-based versions, artifact attestations | Versioning and supply-chain checks with no extra tooling. |

*Escape hatch:* `ssh host -t tmux -L agentk attach -t ak-billing-api--fix-retry-backoff` attaches
to any session from iTerm.

## Milestones

- **M0 · Repo.** Git repo, the commit skill without agent attribution, CLAUDE.md, a license, the CI
  skeleton, and publishing to GitHub.
- **M1 · Core.** The server with login and TLS, a Meko connection check, the New project repo
  picker with a mandatory dedicated datapack, worktree-backed sessions in tmux launched with the
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
