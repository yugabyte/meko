# Projects, worktrees and sessions

Part of the [AgentK design](../design.md).

A **project** is an effort to build or improve a feature, product or service. It works in a git
repository on the host, picked from your repos folder (`~/code`), cloned into it, or made new there at creation.
The project only points at the repo. It changes two things there, and two more for a session on
an existing branch (below). A repo with no commits and
no files gets an empty first commit on `HEAD`'s branch when its project opens or a session starts
in it (see [Open project](open-project-and-new-session.md#open-project)), since a worktree needs a commit to branch from. And when a
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
# layout on the host: AgentK's home, in Meko's data folder (see "Where AgentK lives" below)
~/.meko/extensions/agentk/
  config.toml              # host settings: address and port, TLS paths, repo roots, Meko
  state.db                 # SQLite: projects, sessions, events (WAL); an older install's login, for Meko's handoff
  hook.sock                # Claude hooks (and AgentK's mod) → server
  web.sock                 # the server's only listener; Meko serves it at /ext/agentk/
  run/                     # launchers, the hooks settings file and scripts, run/mod/agentk (AgentK's mod)
  server.lock              # the running server's lease on this home (one server per home)
  certs/                   # an older install's self-signed pair, for Meko's handoff
  app/0.4.1/  app/current → 0.4.1   # Meko installs, updates and prunes these (update-pending.json, bad-versions beside them)
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

[sessions]
idle_stop_hours = 24      # a session's agent idle this long is stopped, and resumed when needed (0 to 720; 0 never stops one)

[meko]
# The Meko Cloud link is Meko's (its web UI's Settings › Meko Cloud, or meko cloud_link). url, api_key,
# web_url and api_url from before are handed to Meko once, then never read again (kept for a rollback).
# home = "…"                 # Meko's data folder, which AgentK's home is in; set by the installer and from Meko's MEKO_HOME
# capture_transcripts = true   # send session transcripts to Meko conversations; off by default (MEKO-710)

[claude]
# command = "claude"         # how Claude Code is run
# enabled = false            # don't offer Claude Code for new sessions; on by default (offered once `claude --version` answers)
# mod = false                # don't load AgentK's mod into sessions (prompts are then typed into the terminal); on by default

[agents]
# default = "kiro"           # the agent new sessions and Ask AgentK run on (claude, opencode or kiro; Settings → Harness); claude by default,
                             # and the first available one (in that order) while the one named isn't available

[opencode]
# command = "opencode"       # how OpenCode is run; the default is also found in ~/.opencode/bin
# enabled = false            # don't offer OpenCode for new sessions; on by default (offered once `opencode --version` answers 2.0.0 or later)
# disable_tracking = true    # the OpenCode AgentK runs sends no telemetry, checks for no update and offers no free models; off by default

[kiro]
# command = "kiro-cli"       # how Kiro CLI is run
# enabled = false            # don't offer Kiro for new sessions; on by default (offered once `kiro-cli --version` answers 2.32.0 or later)
# trust_tools = true         # Kiro sessions start with every tool trusted (--trust-tools=*), so Kiro never asks; off by default

[call_home]
# enabled = false            # stop sending anonymous usage stats (Settings › General); on by default
# url = "https://…/v1/report" # where reports go (the collector's report URL); unset uses the one release builds carry
# diagnostics_url = "http://127.0.0.1:8080/" # where the YugabyteDB copy goes; unset is YugabyteDB's call home service from release installs only

[pr]
# resolve_conflicts = false  # don't have AgentK resolve a PR's merge conflicts with its base (Settings → General); on by default

[code_review]                # the code review before a PR opens (docs/design/code-review.md), also in Settings › Code review
# strategy = "off"           # off, sirius-1.0, or sagittarius-3.0 (the default); a project's and a session's own go over it
# depth = 3                  # times a review runs before AgentK gets the findings to fix (1 to 5; 1 by default); more than one is a deep review: each sweep looks past what the earlier ones found, up to this count, and from the second on, one that finds nothing new ends it; costs more tokens
# rounds = 1                 # times the code review runs again after AgentK fixes the last review's findings (0 to 5; 0 by default); a review that finds nothing ends it early
# check = false              # each round's findings wait for you to add notes or dismiss some before AgentK gets them (true by default); Run to merge never waits

[code_review.models.opencode] # the model each tier of review step runs on; Claude Code's are opus, sonnet and haiku,
# large = "anthropic/…"      # OpenCode's and Kiro's empty (the session's own model)
# mid = "anthropic/…"
# small = "anthropic/…"

[ask_agentk]
# enabled = false            # no Ask AgentK in the command palette; on by default
# model = "sonnet"           # the model that answers asks (a Claude Code alias or a full model name; empty uses Claude Code's default)
# timeout_seconds = 45       # an ask that takes longer is stopped (5 to 300)

[tools]
# enabled = false            # sessions get no session tools (agentk_sessions MCP server and its skill: open projects,
#                            # start and move sessions); on by default, and also in Settings → General

[welcome]
# tour = "agentk-101"        # written only by a fresh install: the server's start runs the AgentK tour once while
#                            # there are no projects (docs/design/workshops.md), then removes it

[work_queue]
# github = true              # Sync pulls in GitHub review requests (Settings › Work queue); off by default
# slack = true               # Sync reads the messages on your Slack list; off by default (both off, the default: no sync)
# slack_list = "https://<workspace>.slack.com/lists/<team>/<F…>" # that list's link (its ⋯ › Copy link)
# sync_minutes = 15          # how often the work queue syncs by itself (0: only when you press Sync, up to 1440)
# slack_model = "sonnet"     # the model of the claude -p that reads that list through your Slack connector
# slack_timeout_seconds = 180 # a Slack read that takes longer is stopped (30 to 600)
# slack_tools = ["mcp__claude_ai_Slack__slack_read_file", "mcp__claude_ai_Slack__slack_read_user_profile"] # the only tools that read may call, besides ToolSearch
```

| Action | What the server does |
|---|---|
| New session | `git fetch origin <base>`, fast-forward the local `<base>` to `origin/<base>` if safe, then in the repo's most recently idled worktree `git checkout --no-track -b <prefix><slug> <base>` (`<prefix>` is the project's branch prefix, `ak_<login>/` by default; or `origin/<base>` if the fast-forward was skipped), or with none `git worktree add … -b <prefix><slug> <base>`, then a `spawn` of pane `ak-<proj>--<slug>` in the session host, in the worktree, running `claude --session-id <uuid> --settings <hooks> -n "<name>" [--plugin-dir <AgentK's mod>] [-- prompt]` (on Kiro, `kiro-cli chat --agent agentk [prompt]`) through your login shell, with the session's variables (Meko, hook token) set after your profile runs so it can't override them. |
| Open tab | Attaches to the pane in the session host, one attach per viewer. The pane takes the size of the viewer that attached or was used last (see *Sizing* in [Architecture](architecture.md)). A phone lets go of its terminals after its page has been hidden for 30 seconds, and of a session's terminal 5 seconds after you switch away from it, so it holds no connections it doesn't show, and reconnects when it is shown again. The 30 seconds are measured by the clock: a phone that suspended the page (so its timers didn't run) lets go of the terminals when the page is shown again, then reconnects the ones on screen. The browser opens the socket at its terminal size (`?cols=&rows=`). A dropped connection reconnects with backoff (reset once a connection has stayed up 5 seconds); a close for a lost login (4401) goes to the login screen, a missing or archived session stops with a **Try again** button, and an attach that couldn't start retries with backoff. |
| Close tab | Ends that attach only. The session host and Claude keep running. |
| Claude exits | The pane dies and the status becomes *exited*. **Resume** relaunches `claude --resume <uuid>` in the same worktree (a Kiro session: `kiro-cli chat --agent agentk --resume-id <Kiro's id>`, see [Agents](harnesses.md)). It sends no prompt, except a PR prompt the last Claude exited before taking ([AgentK's mod](mod.md)). |
| Archive | Refused (409) while the worktree has uncommitted or untracked files, which would keep the branch checked out there: commit them, or Discard. The Archive menu says so and greys out **Archive** (it waits for the check), and uncommitted files are a Merge & Close blocker too. Otherwise ends the session's pane. The clean worktree goes idle for the next session (HEAD detached, so the branch is free), or is removed when *Idle worktrees per repo* is 0; the branch is always kept. The record stays, so the session can be restored later. If the session is still starting, Archive (and Discard) waits up to a minute for Claude to start first, then refuses (409) until it has. |
| Discard | Chosen in Archive's confirmation step (on a desktop, **Archive or discard…** in the session's ⋯ menu opens it in the stage bar). First runs the [learn step](recall-and-learn.md#the-learn-step) and waits for it, unless **Don't save learnings** is checked (it starts unchecked); then ends the session's pane, throws away uncommitted changes and untracked files (`git reset --hard`, `git clean -fd`: ignored build outputs stay), lets the worktree go idle (or force-removes it when it can't be cleaned or reuse is off), and deletes the local branch, unless the session started on an existing branch or a PR's (`branch_reused`), which it keeps. It first warns about uncommitted files and commits that no remote has (none, for a kept branch). The remote branch and any PR are left alone. The record and its token usage stay, as with Archive. |
| Close project | After confirmation (none on the session view of a project with no open sessions, which has nothing to stop and is the only time that view offers it), stops the project's sessions and lets go of their worktrees as Archive does, keeping any worktree with uncommitted changes, and marks the project closed (`deleted_at`). Idle worktrees stay for the repo's other projects; the last project on a repo removes them. That goes for every repo the project's sessions used. Its sessions and token usage are kept for the Usage page. Never deletes the repo, its branches or the datapacks, so opening a project with its datapack (picking it under Project name in Open project) brings the project back (see [Opening a closed project again](open-project-and-new-session.md#open-project)). |

```sql
-- state.db (abridged). One writer: the server. Migrations keyed on PRAGMA user_version.
CREATE TABLE projects (
  id TEXT PRIMARY KEY,            -- slug of the repo, e.g. billing-api; later projects on it add their name: billing-api-release-tooling
  name TEXT NOT NULL, repo_path TEXT NOT NULL, default_branch TEXT NOT NULL,   -- the repo's real path; several projects may share a repo
  position INTEGER, settings_json TEXT, created_at INTEGER, deleted_at INTEGER,
  workshop TEXT);                 -- the bundled workshop that opened it (docs/design/workshops.md); it opens no PR
CREATE TABLE project_datapacks (
  project_id TEXT REFERENCES projects, datapack_id TEXT, name TEXT,
  role TEXT CHECK (role IN ('dedicated','shared')),
  PRIMARY KEY (project_id, datapack_id));
CREATE UNIQUE INDEX one_dedicated ON project_datapacks(project_id) WHERE role = 'dedicated';
CREATE TABLE project_events (  -- closes and reopens, for the usage stats (a reopen clears deleted_at)
  project_id TEXT REFERENCES projects, kind TEXT CHECK (kind IN ('closed','reopened')), at INTEGER);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects, name TEXT,
  harness TEXT NOT NULL DEFAULT 'claude',  -- the agent it runs on: claude | kiro (src/shared/harnesses.ts)
  claude_session_id TEXT NOT NULL, branch TEXT, worktree_path TEXT,  -- claude_session_id: Claude Code's session id, and every
                                  --   harness's key for its Meko conversation
  agent_session_id TEXT,          -- the conversation id a harness picks itself (Kiro's), once a hook has said it; NULL for Claude Code
  tmux_name TEXT,                 -- the pane's name in the session host (named when panes were tmux sessions)
  repo_path TEXT,                 -- the repo's real path: the project's, or another one picked in Advanced
  branch_reused INTEGER,          -- 1: the branch existed when the session started
  desired TEXT CHECK (desired IN ('running','archived')),   -- what recovery restores
  status TEXT,                    -- starting | working | needs_input | idle | exited | recovered | archived
  kind TEXT,                      -- review-pr, code-review, or a task: plan for new ones, explore | plan | quick from before tasks had start
                                  -- stages (older releases and pages index their stage lists with it); kindOf() reads all three as a task
  stage TEXT,                     -- explore | design | code | pr | review | merge (Merge & Close) | done | reviewing-pr | reviewing-code
  parent_id TEXT,                 -- a code-review session's task (code-review.md); NULL for the rest
  review_json TEXT,               -- the session's own code review settings (ReviewOverrides); NULL: its project's
  pr_number INTEGER, pr_url TEXT, review_state TEXT, ci_state TEXT,
  pr_repo TEXT, pr_title TEXT, pr_author TEXT,  -- Review a PR only: the PR's owner/name on GitHub, title and author
  review_sha TEXT, pr_head_sha TEXT,  -- Review a PR: the commit checked out; the PR's head at the last look
  pr_new_commits INTEGER, pr_state TEXT,  -- commits on the PR since review_sha (NULL: rewritten); OPEN | CLOSED | MERGED
  pr_remote TEXT,                  -- Review a PR: the clone's remote the PR is fetched from (origin, or upstream for a fork)
  review_notes TEXT,               -- Review a PR: the user's notes for AgentK
  push_remote TEXT, push_branch TEXT,  -- a task on a PR whose head isn't origin's branch of its name (a fork's):
                                   --   the remote and branch it pushes to (NULL: origin, the session's branch)
  plan_mode INTEGER,               -- 1: designs in Claude's plan mode (sessions created before designs moved out of it)
  pending_prompts TEXT,            -- prompts queued for Claude: JSON map of 'review' | 'review-people' | 'review-held' | 'local-review' | 'conflicts' | 'ci' | 'review-pr-update' | 'review-pr-design' to
                                   --   { state: 'queued' | 'sent' (until that turn ends), vars }; replaces pending_review
  ci_fix_head TEXT, ci_fixes INTEGER,  -- the PR head that last got a CI-fix prompt; how many heads in a row did
  conflict_head TEXT, conflict_fixes INTEGER,  -- the PR head that last got a prompt to resolve merge conflicts; how many
                                   --   heads in a row did (both cleared once the PR merges cleanly)
  queued_gate TEXT,                -- a gate pressed before it could go: 'open-pr' (at the turn's end) | 'merge' (once
                                   --   nothing blocks it); NULL: none
  queued_instructions TEXT,        -- the user's instructions for a queued Open PR (OpenPrRequest); cleared with queued_gate
  auto_run INTEGER,                -- 1 while Run to merge is on: AgentK keeps Open PR, then the merge, queued (lifecycle.md)
  queued_auto INTEGER,             -- 1 when Run to merge queued queued_gate, not the user's press (stopping it drops only its own)
  skip_learn INTEGER,              -- 1: the last Merge & Close press asked to skip the learn step (kept until the PR merges)
  discarding INTEGER,              -- 1: Discard was asked for and waits for the learn step (cleared by Archive)
  agentk_seen INTEGER,             -- unused since migration 47 (the AgentK GitHub reviewer, removed); kept for a rollback
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
  md_hash TEXT, html_hash TEXT,               -- the latest version's Meko content hash, which the PR's meko_design line names
  source TEXT, source_told TEXT,              -- Review a PR: the PR line its copy came from ({name, sha256}), and the hash Claude was last told of
  questions TEXT, questions_source TEXT,      -- unused: a design's questions are read from questions.json and
  open_questions INTEGER, updated_at INTEGER);  -- kept in memory; dropped once the release before that has aged out
CREATE TABLE design_answers (  -- unused: answers aren't stored, the browser sends them; dropped with the columns above
  id TEXT PRIMARY KEY, session_id TEXT, question_id TEXT, question TEXT, choice INTEGER, choice_label TEXT,
  your_call INTEGER, text TEXT, state TEXT, created_at INTEGER, updated_at INTEGER, sent_at INTEGER);
CREATE TABLE review_comments ( -- comments on a session's diff: draft → queued → sent to Claude
  id TEXT PRIMARY KEY, session_id TEXT, path TEXT, side TEXT, line INTEGER, end_line INTEGER,
  excerpt TEXT, body TEXT, state TEXT, created_at INTEGER, updated_at INTEGER, sent_at INTEGER);
CREATE TABLE held_reviews (    -- reviews on a session's PR held for the user: held → sent | dismissed
  session_id TEXT, review_id TEXT, pr_number INTEGER, db_id INTEGER,  -- GitHub's node id; the REST id the prompt names
  author TEXT, kind TEXT, state TEXT, body TEXT, url TEXT,  -- kind: 'person' | 'bot' | 'outsider'
  comments_json TEXT, more_comments INTEGER, submitted_at TEXT, held_at INTEGER,  -- the first 50 inline comments
  status TEXT, decided_at INTEGER, PRIMARY KEY (session_id, review_id));
CREATE TABLE session_stages (  -- stage history: who moved it, and when
  session_id TEXT, stage TEXT, entered_at INTEGER, left_at INTEGER, moved_by TEXT);
CREATE TABLE session_events (  -- every hook call; feeds badges + project activity
  id INTEGER PRIMARY KEY, session_id TEXT, kind TEXT, payload_json TEXT, at INTEGER);
CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1), username TEXT, pw_scrypt TEXT, totp_secret TEXT);
CREATE TABLE auth_sessions (token_sha256 TEXT PRIMARY KEY, created_at INTEGER, expires_at INTEGER, user_agent TEXT, ip TEXT);
CREATE TABLE audit_log (id INTEGER PRIMARY KEY, at INTEGER, action TEXT, detail_json TEXT);
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT);   -- setup token, update state, plan limits
CREATE TABLE work_items (      -- the work queue (workqueue.ts, docs/design/work-queue.md)
  id TEXT PRIMARY KEY, title TEXT, notes TEXT, position INTEGER, status TEXT,  -- open | closed
  source TEXT,                 -- manual | github | slack
  source_key TEXT UNIQUE,      -- a synced item's link key: a sync never adds it again
  session_id TEXT UNIQUE REFERENCES sessions(id) ON DELETE SET NULL,  -- one session per item, one item per session
  unplaced INTEGER, github_done_at INTEGER, created_at INTEGER, closed_at INTEGER);
CREATE TABLE work_item_links (item_id TEXT, ord INTEGER, key TEXT, kind TEXT, url TEXT, label TEXT,
  PRIMARY KEY (item_id, key));  -- indexed on key, for the palette's lookup by link
CREATE TABLE code_reviews (    -- a task's code reviews (review/runner.ts, docs/design/code-review.md), one per Open PR, move to Review & PR or review-run
  id TEXT PRIMARY KEY, task_id TEXT, session_id TEXT,  -- the task, and its code-review session
  strategy TEXT, rounds INTEGER, depth INTEGER,        -- what it runs with, stamped when it starts
  state TEXT,                  -- describing | preparing | sweeping | scoring | fixing | done | failed | cancelled
  round INTEGER, sweep INTEGER, asked INTEGER,         -- where it is; asked: steps asked for again this phase
  candidates_json TEXT, findings_json TEXT, notes_json TEXT, datapacks_json TEXT,  -- this round's candidates, every round's
                               --   findings (the panel), the passes' notes, the MEKO.md datapacks
  counts_json TEXT, last_kept INTEGER,                 -- every round's kept findings by severity (the PR line)
  instructions TEXT, by TEXT,  -- the Open PR's instructions it holds, and who pressed it (you | auto)
  lead TEXT,                   -- the `moved` line of a move to Review & PR from Explore or Design, sent before the
                               --   review's first prompt to the task (its fixes or the PR prompt), cleared once that goes
  purpose TEXT, stage TEXT,    -- pr (an Open PR's) | demand (the user ran it: no PR prompt); code | review: where it runs
  stale INTEGER,               -- 1 once the task left Code after it finished with its PR prompt unsent (Open PR reviews again)
  hand_off INTEGER,            -- 0: a first look (a workshop's), whose findings stay with it, never sent to the task; 1 for the rest
  detail TEXT, created_at INTEGER, updated_at INTEGER, finished_at INTEGER);  -- finished_at: its PR prompt went
CREATE TABLE code_review_steps (review_id TEXT, step TEXT, report_json TEXT, at INTEGER,  -- each step's report, as
  PRIMARY KEY (review_id, step));  --   review_report or review_score took it (r1s1:guide, r1c3)
```

- **Why SQLite.** You get transactions across projects, sessions and datapacks, an event history
  for the activity feed, and crash safety without hand-rolled atomic file writes. It's still one
  file you can back up or inspect with the `sqlite3` CLI.
- **Settings.** `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON` and
  `busy_timeout=5000`. With WAL, NORMAL keeps the database consistent through any crash. A crash
  of the server loses nothing. An OS crash or power cut can lose only the last commits before it.
  FULL was dropped because it fsyncs on every commit, and `node:sqlite` runs that on the event
  loop. On a dev box whose disk other builds kept busy, a commit took up to 405 ms under FULL
  (0.2 ms under NORMAL), and every request waited behind it. Old `session_events` are pruned
  after 30 days, except `merged` events, which the Usage page counts.
- **Not on NFS.** SQLite locking is unreliable on network filesystems. A home in Meko's data
  folder is on a local disk (below), so `state.db` is in it. Only a home from before the move can
  be on NFS: for one, `me_koder install` puts the database on local disk (`[state] db_path` in
  `config.toml`, `$XDG_STATE_HOME/agent-k-local` or `/var/tmp/$USER/agent-k-local`), and the move
  brings it into the new home and clears the setting. A folder in `/var/tmp` must be a real directory owned by you that others can't write
  to, or AgentK stops rather than put its data (hook tokens, an older install's login) somewhere
  another user controls.

### Where AgentK lives

AgentK's home is `<Meko's data folder>/extensions/agentk`, and users don't pick another place.
Meko's data folder is found by Meko's own rule (`Resolve` in `backend/internal/paths/paths.go`;
`mekoHome()` in `src/server/paths.ts` repeats it, both tested against
`backend/internal/paths/testdata/resolve-cases.json`): `MEKO_HOME`, refused on
a network filesystem; else `~/.meko` on a local disk; else, for a home folder on a network
filesystem, `$XDG_STATE_HOME/meko` when that's local, else `/var/tmp/<user>/meko` (made private as
above). Meko sets `MEKO_HOME` on every AgentK command it runs, and AgentK keeps it as `[meko]
home`, since its own services don't have it. For `meko start` and `meko stop` it also sets
`MEKO_SUMMARY=1`, and AgentK's `start` and `stop` then print one row of Meko's summary
(`src/server/summary.ts`, the format in Meko's `backend/docs/install.md`) in place of their usual
line and its `me_koder status` hint. A home already in a Meko data folder refuses
`uninstall`, `start`, `stop`, `restart`, `status` and `_handoff` when `MEKO_HOME` names another
folder (`refuseOtherMeko` in `src/server/movehome.ts`; `samePath` compares the folders, not their
paths, so a second spelling of the same folder that links don't explain, such as a bind mount,
isn't another folder): a release acts on its own home whatever
`MEKO_HOME` says, so a Meko that found this AgentK for another data folder (a second Meko, Meko's
own tests) would otherwise stop or uninstall it, ending every session, and repoint its `[meko]
home`. A home that hasn't moved into a Meko data folder yet isn't protected: it takes Meko's
folder from `MEKO_HOME`, so another Meko's commands still act on it and its `[meko] home` (where
it moves next) follows the last one. `install` isn't refused either: Meko's `extension install`
sets the existing home up again in place, and `me_koder install` keeps the Meko data folder its home is in as `[meko]
home`, over `MEKO_HOME`. The server takes `MEKO_HOME` and `XDG_STATE_HOME` from the login shell
(as it does `PATH`), so it finds the same folder `me_koder install` did. A relative `XDG_STATE_HOME`
is ignored, as the XDG spec says.

- **A release's home is its own folder** (`<home>/app/<version>`, `AK_APP_DIR`), whatever
  `AK_HOME` says; the CLI run through `~/.local/bin/me_koder` finds it that way, and so does the
  launcher. `AK_HOME` names a home only without a release (`npm run dev`, tests).
- **Otherwise** (`akHome()`): `<Meko's data folder>/extensions/agentk` when it's there, else
  `~/.agent-k-local` when that's there (an install the move hasn't reached), else the new one.
- **In `/var/tmp`**, which many Linux systems clean by age (systemd-tmpfiles' default is 30 days on
  Fedora and RHEL), the server holds a shared `flock` on the home while it runs, which the cleanup
  skips on systemd versions that honor it, and the installer and the server warn and suggest a
  local `XDG_STATE_HOME`.

Homes from before this (`~/.agent-k-local`, or one an install named with `AK_HOME`) move there by
themselves: [Moving the home into Meko's data folder](recovery.md#moving-the-home-into-mekos-data-folder).

`config.toml` holds host settings: the server's name, repo folders, the Meko URL and key (only when typed at install), transcript capture, the
`claude` command, Meko's data folder as Meko last named it, how many idle worktrees each repo keeps, and whether AgentK resolves PRs' merge conflicts. The Settings dialog
writes its fields (repo folders, scan depth, clone folder, idle worktrees per repo and the days to
keep an unused one, server name, resolving merge conflicts) there too. An `[update]` table is
ignored, with a warning, and goes at the next save: Meko updates AgentK, by its own `[update]`. The server reads the file at
start, so an edit made while it runs takes effect after a restart, and a save from Settings meanwhile writes over it. Per-project settings, sessions and everything else live in `state.db`. Only values that
differ from the defaults are written, so a changed default reaches existing installs; a value of the wrong type is ignored with a warning. An older
install's `[server] bind` and `port` and `[tls]` stay in the file for Meko's handoff
([security.md](security.md)), and nothing else reads them. The Meko
URL and key are Meko's: the Meko running on this computer with its own URL and token, linked to Meko
Cloud or not (while linked its `/mcp` and `/api/v1` serve its own datapacks and pass any other id on
to Meko Cloud; one from before that passes REST on under `/api/v1/cloud`), or under an older Meko that still hands out its link's key, that link's URL and key;
all read from Meko (`meko token --json`) and never written here. `[meko] url`, `api_key`,
`web_url` and `api_url`, `MEKO_MCP_URL`/`MEKO_API_KEY` and the `env` block of Claude Code's user
`settings.json` are read only to hand a key from before to Meko, once, and under a Meko too old to
keep the link ([datapacks.md](datapacks.md)). An `enabled` key left in `[meko]` by older versions is ignored and
dropped at the next save: Meko can't be turned off.

## Worktree reuse

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
