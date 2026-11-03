# REVIEW.md

What to check when reviewing a change to **AgentK**, whether you're a person or an agent
(AgentK). Background and invariants are in [AGENTS.md](AGENTS.md); the design is in
[docs/design.md](docs/design.md).

Order findings by severity: **blocker** (breaks an invariant, loses work, or opens a security
hole), **major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

## 1. Sessions and recovery

- [ ] Nothing makes the session host or a session's Claude a child of the server process or its
      service (systemd unit or launchd job). One-shot `claude` commands that end with their request
      (`--version`, `update`, Ask AgentK's `claude -p`) are the server's own. Code that manages the
      services goes through `src/server/service.ts`.
- [ ] Server restarts and updates leave running sessions untouched. The one exception is the
      one-time move of an older home into Meko's data folder (`src/server/movehome.ts`, AGENTS.md
      invariant 1), and nothing else may start it or end sessions the same way.
- [ ] For a home already in a Meko data folder, `uninstall`, `start`, `stop`, `restart`, `status`
      and `_handoff` refuse a `MEKO_HOME` that names another folder (`refuseOtherMeko`, checked in
      `main()`), so a Meko for another folder can't stop or uninstall it, or serve it and take its
      old login. `samePath` compares folders, not paths: the same folder under a spelling links
      don't explain (a bind mount) isn't another. A home that hasn't moved there yet isn't
      covered: another Meko's commands still act on it.
- [ ] `recover()` / `reconcile()` stay idempotent: live panes are left alone, missing panes are resumed,
      and sessions still being created or launched are skipped (so Claude never starts twice).
- [ ] Nothing respawns a live pane, and a session host error other than "no host" is never read as
      every pane being gone.
- [ ] Creating a session answers once its row is in (`begin()`): only the checks that must fail in
      the New session dialog run before that, never a worktree checkout. A failure after the answer
      is a failed start or an exited session with its error, never silent.
- [ ] Values in the launcher are set after the login shell's profile runs, and inherited
      `AK_HOME`/`MEKO_CHECKPOINT_INTERVAL` are cleared.
- [ ] Auto-continue guards hold (once per 15 minutes; after two auto-continues in an hour the next
      mid-turn recovery is parked).
- [ ] Status changes come from hook events or pane checks, not guesses (the one fallback: a starting
      session whose hooks never came follows its transcript). Deliberate stops (archive,
      cleanup) don't show up as "exited".
- [ ] The home may be seen by other hosts (a home shared over NFS). Only the server holding the
      `server.lock` lease opens the database or binds the hook socket, the systemd units keep
      `ConditionHost=`, and nothing removes a socket or file another server may own.
- [ ] The status line relay stays fast and quiet: it reports in the background, always runs the
      user's own status line with the same input, and never prints anything of its own.
- [ ] AgentK's mod (`src/mod/agentk`) never gets in Claude's way: every failure is swallowed, and
      sessions work without it (Claude Code too old, `[claude] mod = false`, hooks modules off). A
      prompt runs once at most: the mod submits it only on the server's 204 to its ack, a send it
      doesn't ack in time fails as a failed paste does (and a late ack gets 410), and a prompt
      handed to the mod is never also typed into the terminal. Held polls are answered before the
      hook socket closes, on a rebind as well as a stop. A question Claude asks (`AskUserQuestion`)
      is answered once: the terminal's dialog always opens too, the first answer wins, and any
      failure leaves it to the terminal; the server keeps an ask only while the mod keeps asking.
      Answers and approvals sent mid-turn (a turn actually running, not any Needs you) need a live
      mod: answers wait in Claude (the status is left alone, and the mod reports which it still
      holds), and an approval waits on the server for the turn's end and is dropped if the turn
      changed the design or its questions; the stage never moves to Code mid-turn or while Claude
      holds answers. Status details can quote Claude's own words (`AgentK asks: …`), so code that
      recognizes a state matches the server's fixed text exactly, not a pattern. Changes to the mod pass `claude plugin validate` and
      `claude plugin test src/mod/agentk`, and keep to the API of the oldest Claude Code
      `MIN_MOD_CLAUDE` allows.
- [ ] A session's agent is its harness (`Session.harness`): code that launches, reads hooks,
      answers the guard or context hooks, reads a transcript, usage or conversation id, or runs Ask
      AgentK goes through its adapter (`Harness` in `src/server/harness/types.ts`), never straight to
      Claude Code's, OpenCode's or Kiro's files or flags, and what only some harnesses have (the mod, Artifacts,
      usage as tokens or credits, a minimum version) is read from `HARNESS_CAPS` or an optional
      adapter method, not from the harness's name (no new `harness === 'kiro'` outside the adapters;
      `docs/design/harnesses.md` lists the few that stay on purpose). Text about the process and its
      terminal names the session's agent (`agentName()`), not "Claude", in error messages and
      screen-reader labels too. Anything AgentK
      writes in the user's Kiro home (the `agentk` agent) is marked as AgentK's, inert without
      AgentK's variables, grants nothing the user's own agent doesn't (trust goes on AgentK's
      launch), and is removed by `uninstall`. A Kiro session reads and resumes only its own
      conversation: worktrees are pooled, and Kiro keeps earlier sessions' under the same folder.
      Nothing is written to the user's OpenCode config: AgentK's plugin and Meko server reach only
      AgentK's sessions, through their environment (`OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG`), and
      a secret (Meko's key) goes in an owner-only file, never the session's environment. Every
      OpenCode command AgentK runs that needs a server (the session's TUI, `run`, `session`, `auth`)
      passes `--standalone`: OpenCode 2's shared background service has another environment, and
      must never be started with a session's. Only `--version` and `upgrade` go without it: they're
      one-shot commands in the server's own environment that neither start nor reach a server. A plugin folder a session
      loaded is never written again (OpenCode reloads a plugin whose files change).
      No harness is assumed installed, Claude Code included: AgentK starts, and every feature that
      needs an agent asks the registry for an available one (`Harnesses.defaultFor`/`pick`) and
      says what to install when there's none, rather than running `claude` regardless.
- [ ] A change to `src/server/host/` that an older host or server can't read bumps `PROTOCOL`, and the
      server still works with hosts back to `MIN_PROTOCOL` (a running host keeps its code across
      updates). Nothing restarts the host by itself (the home's one-time move aside), and the program
      never waits on a viewer. `serve` and `host` keep going through Meko's launcher
      (`packaging/agentk`: `meko _launch`), which runs an update's probation and locks the host's
      version folder against Meko's pruning; nothing in AgentK writes Meko's update records
      (`app/update-pending.json`, `bad-versions`).
- [ ] The prerequisites check (`scripts/deps.sh`) never installs a tool (or runs Meko's setup) without first showing what it
      will run and asking (or `--install-deps`), verifies downloads by checksum or integrity hash
      wherever one is published (Claude Code's installer script from claude.ai, OpenCode's from
      opencode.ai and Kiro's from cli.kiro.dev publish none, so they're only fetched over HTTPS), and checks the result rather than trusting the package manager.
- [ ] Shell commands and scripts work with macOS's BSD tools as well as GNU ones (no `mv -T`,
      `stat -c`, `sha256sum`-only, `sleep infinity`, `sed -i` without a suffix, `readlink -f`).

## 2. Security

- [ ] The server listens only on its unix socket and has no login of its own: Meko decides who gets in
      and serves AgentK at `/ext/agentk/` (AGENTS.md invariant 3). Nothing adds a TCP listener, a
      login, or trust in a forwarding header. Access checks key on the matched route, never the raw
      URL (the router decodes `/%61pi/`).
- [ ] Request bodies are type-checked before use (a wrong type is a 4xx, never a 500), and nothing
      re-adds a `text/plain` or form body parser (HTML forms must not be able to post to the API).
- [ ] Writes and WebSocket upgrades still pass the origin check. The page builds every URL of
      AgentK's own under its base (`src/web/base.ts`), and calls Meko's routes (`/api/v1/auth/*`)
      root-absolute.
- [ ] Nothing from a session's worktree that its repo's author controls (settings, scripts, prompts,
      agent or MCP config) runs or is used unless `repoConfigTrusted(row)` says so (`headtrust.ts`):
      on a public repo's PR head, anyone can have written it. A PR found by its branch's name must
      have its head where the branch is pushed (`pickPr`).
- [ ] A saved secret (the Meko key) is never sent to a URL the browser just set without the secret
      being typed again with it.
- [ ] No user input reaches a shell. `execFile`/`spawn` take argument arrays; launcher values go
      through `shq()`.
- [ ] Secrets never land in logs, `config.toml` (unless the user typed them), the database, or the repo.
      The Meko key never goes to the browser (Settings shows only its last four characters), a
      Meko URL or key change is checked against Meko before it's saved, and a typed API URL is
      checked without the key. Remote URLs lose their credentials before they go into a datapack
      description (`withoutCredentials`).
- [ ] Nothing sent to a browser can queue in memory without limit (terminal output pauses the pty;
      state snapshots are skipped). A socket that closes abnormally (1006: Meko refused it, or closed
      it as its web listener stopped) asks Meko about the sign-in alongside its retry, throttled.
- [ ] The web socket and the hook socket stay owner-only (directory 0700, socket 0600; bound under a
      temporary name and renamed, `listenPrivate`), and hook events are matched by per-session token.
- [ ] Nothing internal (hostnames, mount paths, tokens) is committed; the repo will be public.
- [ ] No secrets or personal data in the diff, the PR title or its description (the repo root's
      AGENTS.md): no passwords, tokens, keys or cookies, no real person's name, email, handle or
      IP address, no customer names, nothing a user wrote or stored (prompts, transcripts,
      memories), and no unscrubbed logs or screenshots. Tests and docs use made-up values
      (`alice@example.com`, `github.com/acme/demo-repo`). Any found is a **blocker**. The
      description's Meko lines from AgentK's PR prompt (`<Meko host>://<datapack id>;<name>`,
      `meko_design:`, `meko_conversation_id:`) are allowed and stay as written.
- [ ] Pasted images (`POST /api/sessions/:id/images`) are checked by their bytes, size-capped, and
      saved owner-only under the session's `pastes/` folder with a name the server picks; the
      browser only gets the path back. The route's `image/*` parser stays in its own scope, so no
      other route takes a non-JSON body. Images staged in the New session dialog
      (`POST /api/projects/:id/images`) are named by id only: `CreateSessionRequest.images` never
      takes a path, and an id is matched to the server's name pattern inside that project's
      staging folder. Pasted files of other types (`POST /api/sessions/:id/files`) are size-capped
      and saved owner-only under a name the server picks, keeping only the safe part of the
      browser's name (`safeFileName()`), and are never served back.
- [ ] Knowledge-base uploads (`POST /api/sessions/:id/knowledge`) go through the server, so the
      Meko key never reaches the browser; the `application/octet-stream` parser (it and `/files`)
      stays in its own scope, the name is a base name only, and nothing is sent to Meko before the
      user confirms Upload to Meko's question, which names the datapack.
- [ ] Repo files served to the browser (image previews) are limited to image types inside the
      worktree, the base commit, or one of the branch's listed commits and its parent, and carry
      the sandboxing content policy.
- [ ] Agent-generated HTML (Artifact designs) only renders in the sandboxed design frame: no
      `allow-same-origin`, and the frame's content policy still blocks network requests and comes
      first in the frame's document. Markdown designs are sanitized with no forms or styles, by
      their own DOMPurify instance (Mermaid adds hooks to the shared one). Their Mermaid diagrams
      draw in strict mode, which is what keeps them out of the app (Mermaid draws in the app's
      page), and show as `<img>` SVGs, never inline SVG or a page of their own (a `blob:`/`data:`
      SVG opened as a page could run script). A
      design's questions (from Claude's `questions.json` or its `design_questions` tool) and the
      questions Claude asks (`AskUserQuestion`, previews included) render as plain text, never
      HTML, and are size-capped and normalized on the server.
- [ ] A reviewed PR's design (`prdesign.ts`) is another person's content: it's used only when its
      bytes hash to the PR line's hash, size-capped like any design, rendered by the same sandboxed
      frame or sanitizer, never sent to Meko again, and the prompts call it content to review, not
      instructions. AgentK's edits to a PR description change only its own `meko_design:` line.
- [ ] The Design-stage guard denies edits inside the worktree and main checkout, by its stored
      path and by the spelling Claude is given (`claudePath()`), and fails open (no answer means
      allow) so a slow or restarting server never blocks Claude.
- [ ] Ask AgentK (`askagentk.ts`) only proposes: its `claude -p` keeps `--tools ""`,
      `--strict-mcp-config` and hooks off, the answer's actions are checked against the state
      (`checkActions()`), and the browser runs one only when the user picks it (a lone `go`
      too), through the route the UI already uses. Names, branches and PR titles in its context stay
      data in the prompt, and nothing of what's asked goes into logs or the usage report.
- [ ] The work queue's Slack read (`slack.ts`) is a `claude -p` that loads the user's MCP servers
      and connectors and reads messages other people wrote, so it keeps `--tools ToolSearch`,
      `--permission-mode default`, `--setting-sources project` in its empty folder with only the
      sign-in part of the user's settings (`signInSettings()`: no allow rules or default mode),
      hooks off, only `[work_queue] slack_tools` allowed and the Slack connector's writes denied; its
      prompt calls the list's rows data. What it returns only adds items to the queue.
- [ ] AgentK's session tools (`actions.ts`, `mcp.ts`) act only through `runActions()`, the same core
      calls the UI's routes make, after `checkActions()`: the hook socket's `/tools` names the caller
      by its token, refuses a PR head `repoConfigTrusted()` doesn't trust (not checked yet included)
      and the tools turned off, never opens the caller's own PR, holds the caller's own move for its
      turn's end, answers calls still running when the hook socket closes (`SessionTools.pause`), and
      leaves a `tools` event on both sessions. Only read-only tools (`readOnly` in `tooldefs.ts`) are
      pre-allowed, and never in the user's Kiro agent; `agentk mcp` checks nothing itself and loads
      nothing of the server's.
- [ ] Every path AgentK hands Claude (launcher arguments, `HOME`, `AK_HOOK_SOCK`, allow rules,
      file paths in prompts) goes through `claudePath()`, so a home under `/net` doesn't make
      Claude Code ask before every file or refuse the mod's socket; what AgentK stores keeps its
      own spelling.

## 3. Lifecycle and GitHub

- [ ] Gates (approve design, open PR, merge) are only passed by the user, never automatically, except
      Open PR and merge while the user has Run to merge on (`sessions.auto_run`), which only the user (or a
      start the user asked a session tool for, at Code) turns on, and which never approves a design. A gate
      pressed early (`sessions.queued_gate`) goes only as that press would have, shows with Cancel (honored
      until the prompt is typed or the merge requested), and is dropped (logged) when what it waited for is
      gone: a queued Open PR when Claude exits by any path (`setStatus` to exited, a relaunch), either one on
      a move or archive, and a queued merge when a blocker appears that won't clear by itself (not Run to
      merge's, which waits and tells the user, and stops when GitHub refuses the merge). Run to merge stops
      on an uncheck, a Cancel, a move and an interrupt where the harness reports one (Claude Code's mod,
      OpenCode), so the user can always take the gates back, and stopping drops only the gate it queued
      (`queued_auto`), never one the user pressed. The one exception to "dropped when Claude exits": a
      PR prompt Claude took but exited before running (`lostPrs`) is that press carried over. It goes
      only when Claude is back with no prompt of its own (`claudeBack`), and is dropped (logged) on
      anything first: another prompt's turn, a move, archive, Run to merge stopping (its own), or a
      newer Open PR press. Nothing resends it at a later turn's end.
- [ ] Each session kind keeps to its stages (`KIND_STAGES`): a task starts at Explore, Design or
      Code and leaves a stage only through a gate or a move by the user (`Lifecycle.move`, rules in
      `src/shared/stages.ts`), and a Review a PR stays in Reviewing PR. Code that switches on `stage`
      handles `explore`, and the stage bar maps `pr` to the Review & PR step with `stepOf`.
- [ ] `sessions.kind` and `Session.kind` keep the old type names for tasks, so a rolled-back release
      and a page still on the older UI can index their stage lists with every value: new tasks are
      stored and sent as `plan` (`STORED_TASK_KIND`), never `task`, and code that needs a session's
      kind reads it through `kindOf()` (comparing with `review-pr` directly is fine).
- [ ] A move by hand leaves the session consistent: the Review
      queue dropped when leaving Review & PR, the edit guard on when entering Design, and no move
      out of Merge & Close or while AgentK is mid-turn. A move's prompt carries on from the conversation
      (`move-design`, `move-code`): it doesn't restate the session's first prompt, which the work may have moved on from. The
      exceptions name a task on purpose: a move that gives a session without one its task sends the stage's start prompt, and
      an older page's `prompt: true` on a move to Explore sends `explore`.
- [ ] Prompts (stage, review, review-comment and design-answer prompts) are only typed into a session when Claude is
      idle (or showing a design waiting for approval), never into a permission or question prompt; otherwise they're queued (the review prompt
      in `state.db`, so it survives a restart), except answers to a design, which aren't kept on the server and are refused instead
      (with a live mod they go through it, and Claude holds them until the turn ends).
      Prompts to one session go one at a time.
- [ ] Merge is refused while `mergeBlockers` isn't empty; blockers are phrased for users. Only
      blockers that clear by themselves let it be queued (`MergeInfo.queueable`); any other one drops
      a queued merge, and the user is told (Run to merge's merge waits instead, and the user is told
      once per new set of blockers only they can clear). Under Run to merge, notifications are only
      for what needs the user and for the merge; progress AgentK handles itself goes through
      `notifyProgress`, which skips it. What only the user can clear is decided once, in
      `mergeInfo()` (`MergeInfo.yours`), which both that notification and the Needs you dot in Review &
      PR (`Session.waitsOnYou`) read; a blocker AgentK still works on is never in it.
- [ ] Cleanup never force-removes a worktree with uncommitted changes, and never deletes the
      user's main checkout or repo. Trimming idle worktrees never uses `--force`.
- [ ] The only changes made in the user's main checkout are fast-forwarding the base branch when a
      session starts (`merge --ff-only` with autostash off in the main checkout, or a non-forced
      `git fetch . refs/remotes/origin/<b>:refs/heads/<b>`; never inside another worktree), the same
      fast-forward of an existing branch a session starts on (from its remote, a fork's for a fork's
      PR), setting that branch's `branch.<b>.remote`/`.merge` to a fork's PR head, and an
      empty first commit on HEAD's branch of a repo with no commits and no files
      (`ensureFirstCommit()`, at project open or session start; a repo with files is refused, and an
      empty folder typed under Other folder is made a repo only after the name check), never a
      reset or force.
- [ ] Nothing from someone without a role on the repo (`authorAssociation` other than OWNER,
      MEMBER or COLLABORATOR, and not an allowed bot or the user) starts a turn: such a review is
      only held, and reaches Claude only when the user sends it, marked as from someone with no
      role. Nothing waits for, or asks for, a GitHub reviewer of AgentK's own. Prompts that put PR text in front of
      Claude call it untrusted, not instructions. Where a review goes in each mode is
      `reviewRoute()` in `heldreviews.ts`; don't route reviews anywhere else.
- [ ] CI-fix prompts go out once per PR head, never ahead of a queued review prompt, and stop after
      three failing heads in a row; a queued one is dropped when the head moves or CI stops failing.
      Merge-conflict prompts follow the same rules (between reviews and CI in the queue), act only on
      `mergeable` CONFLICTING (never UNKNOWN), have Claude merge the base rather than rebase or
      force-push, and are never sent once `[pr] resolve_conflicts` is off (`deliverNext` drops a queued one, even when
      the poll fails).
- [ ] A Review a PR session only gets ready and sums up on its own: its prompts and workflow line
      never have Claude review, post on GitHub, commit or push unless the user asks, and nothing
      is sent to it by itself (new commits wait for **Update to latest**). Its review branch has
      no upstream, Update to latest and Close review never throw away the user's uncommitted
      files or own commits, and its `gh` calls name the PR's repo (`--repo`), not the clone's.
- [ ] Anything about a session's PR (reviewers, a review's PR) uses that session's repo and the
      PR's own `owner/name`, never the project's repo: a project's PRs can be in several repos.

## 4. Data and Meko

- [ ] Schema changes are a new migration; shipped migrations are unchanged.
- [ ] Every project still has exactly one dedicated datapack; each session gets its own `MEKO_AGENT_ID` and pin (`sessionAgentId()`), never one keyed on the project id.
- [ ] Several projects can share a repo, and many can run sessions on it at once: per-project state
      (pane names, lookups) is keyed by project id, not repo path, and git work on the repo only
      touches the session's own branch. The exceptions are a repo's empty first commit (§3), and idle worktrees, which belong to the repo
      (keyed by its real path) and are shared by all its projects: a claim is one SQLite write, so
      two sessions never get the same worktree, and closing a project leaves idle worktrees another
      project on the repo can use. A project's `repo_path` is the repo's real path (`repoKey()`), so
      repo matches are plain equality on it; no new code compares repos by the path as typed.
- [ ] A session may work in another repo than its project's (`sessions.repo_path`). Git and `gh` work
      for a session uses `sessionRepo()`, never the project's `repo_path`, and anything that decides
      whether a repo is still in use counts the repos of a live project's sessions too. A branch the
      user names goes through `checkBranchName()` and is refused while a running session has it.
- [ ] Worktree reuse never hands over a worktree with uncommitted changes or untracked files (it's
      checked when the worktree goes idle and again when it's claimed), never touches a worktree
      that is another session's (check `ownsWorktree()` before reading or removing an archived
      session's worktree), and a failed reuse falls back to a new worktree instead of failing the start.
      Anything that removes idle worktrees forgets the row with a write conditional on it still being
      idle (`session_id IS NULL`) before running git, so it can't remove one a session just claimed.
- [ ] Meko isn't polled. A Meko failure shows as the error of the action that needed it (or a
      logged warning for background work) and never kills a running session or blocks work that
      doesn't need Meko (a project with No datapack, sessions). No Meko (`MekoStatus.configured`
      false: no Meko Cloud link, and no Meko running on this computer) means no Meko calls at all; the one
      thing done then is looking for the Meko on this computer every minute (`meko token --json`
      in `Core.refreshMeko`, a local command with a 5 s timeout, one at a time, never a Meko
      call, and only while there's no Meko, or no `meko` on this computer: `lookForLocalMeko`, so
      the install banner goes once Meko is installed). Reading the local Meko again otherwise happens only on
      events (startup, Settings open, Meko's `POST /_meko/changed`, a failed connection at most
      every 30 s), and a change on the same server reconnects without forgetting the account's
      invites (`mekoChanged` is for switching servers).
- [ ] Meko owns the Meko Cloud link. AgentK has no route or UI that sets or shows a Meko key, and
      reads its own old sources (`ownMekoKey`: `[meko]` keys, `MEKO_*`, Claude Code's settings) only
      to hand a key to Meko once (`mekohandover.ts`, the key on stdin) or under a Meko whose `meko
      token --json` has no `cloud`. Never a copy of the local Meko's token, and never falling back to
      the local Meko while a key of its own hasn't reached Meko. Its `[meko]` keys in
      `config.toml` are never cleared on handover (a rollback restores only `state.db`), and the
      handover never holds up a start.
      The one poll is following a knowledge-base upload's
      indexing (`KnowledgeService` in `knowledge.ts`): only after the file was posted to Meko's
      storage under the key `upload-url` gave (never for one Meko already had), every 5 s for a
      minute then every 30 s, at most 15 minutes per upload, never
      without a Meko, stopped at an error waiting can't fix, and dropped on a Meko settings change.
      The one check that calls Meko by itself, trying Claude
      Code's tokens (`mekoclaude.ts`), runs only on events (startup, config change, a change of the
      Meko on this computer, Settings open, session launch, after Claude Code's setup, Claude Code becoming available or not after a
      version read), only while Claude Code is available, in the background, only for a token that isn't
      AgentK's own, and reuses results for 10 minutes; one it couldn't check never raises the banner.
- [ ] A new place AgentK calls Meko for a session wraps the call in `withMekoScope(session)`, so the
      session's Meko call log (`mekolog.ts`) shows it; a call that belongs to no session isn't wrapped.
      The log never holds the API key, is written only while the project has Log Meko calls on and
      the session isn't archived, and is deleted wherever the session's pastes are (archive, failed
      start, closing the project). Writing it never fails the call. Claude's calls reach it only
      from the mod's `/mod/meko` end, never change what Claude gets back, and a reply read from a
      file Claude Code saved comes only from the transcript's own session's `tool-results` folder.
      Kiro's and OpenCode's reach it only from the usage read of their own files, once per call
      (when its `meko_calls` row is new).
- [ ] The learn step (`learn.ts`) writes only to the session's dedicated datapack, never a shared
      one, and on the mod path never a fact that names this machine (home, AgentK's home,
      worktree, repo path, by the stored spelling and by `claudePath()`'s); the typed path relies
      on the saving rule in its prompt. It
      never holds Merge & Close or Discard past its timeout on the mod path (writes included), and
      a Discard waiting for it always ends (step done, failed, interrupted, or Discard now; a
      restart finishes it), and skipping it (`skipLearn`: **Merge & Close without saving learnings**,
      or Discard's **Don't save learnings**) skips it entirely; it never holds a
      hook-socket request while it works with Meko (`/mod/learn` answers at once; what follows goes
      on a poll), a failure at any step closes it and lets the close go on, and one runs at a time
      per session. Its model calls' usage is
      recorded (`usage_messages`, agent `agentk:learn`), and the usage report carries counts and
      tokens only, never a fact's text.
- [ ] Meko owns the searches it couldn't answer (its Context Gaps): AgentK never judges a search
      or keeps a query again. It reaches them only through `MekoQuestions` (`mekoquestions.ts`),
      on the Meko on this computer with the client token, only while AgentK uses that Meko (the
      local token goes nowhere else), and a 404, a failure or no Meko is no list: never a reason a
      project home, a learn step, a move or a close fails or waits long. A learn step marks only
      questions still open in Meko when it writes, in the session's own conversation, for the
      datapack it wrote to; a learn session's seeding asks are forgotten once, when it leaves
      Explore. AgentK's own MCP client keeps sending `X-Meko-Client: agentk`, so its own searches
      (at start, the learn step's lookups) never become questions.
- [ ] Nothing changes Claude Code's Meko setup (its settings, `~/.claude.json`, the plugin) unless
      the user asked: the installer's question (or `--install-deps`), **Connect to Meko** or **Set
      up Claude Code with this Meko**. A Claude Code token is only ever sent to AgentK's server, and
      only when Claude Code's settings name that server; messages name hosts, never full URLs or tokens.
- [ ] Meko owns the agents' connections (AGENTS.md invariant 7). AgentK's server only reads `GET
      /api/v1/agents` with the client token (`mekoagents.ts`), on events (startup, Settings open,
      Check again, after an agent's setup, a change of the local Meko or its link), never on a timer,
      and never posts to Meko's agent routes; Connect is the page's own call to Meko, with the
      owner's sign-in. No new check of an agent's `meko` MCP server in AgentK: that's Meko's list.
      Only new sessions are gated (`Harnesses.statusOf`, through `pick` and `defaultFor`), never a
      running or resumed one; a Meko that doesn't answer leaves the last list read in force (kept
      in `state.db`), and with none ever read, or a 404, nothing is gated; nor is an agent Meko
      didn't find (`missing`: Meko never sees AgentK's `[<id>] command`). Before refusing one, `pick`
      reads Meko's list again. A refusal names the fix (`notConnectedText`).
- [ ] The Meko search at session start (`recall.ts`) never holds up a session: `create()` doesn't
      wait for it, the SessionStart context hook answers within its limit and prints nothing when
      there's no result or no server, and there's no search with Meko off or no datapacks. What it
      gives Claude stays short (capped items and total).
- [ ] Tests don't touch the real `~/.claude.json`, Meko, or GitHub. Meko's REST API goes through
      `fakeMekoApi()` in `test/helpers.ts`, Meko's setup for the agents through `stubMekoTools()`, and the key handover through `makeEnv`'s `mekoHandOver` stub (`handOvers`).
- [ ] Share invites: a project opened from an invite accepts it in Meko before writing anything
      locally, the first invite sync doesn't notify about shares that already existed, and each
      share notifies once (one joined here by its share link gets no notification or banner). While Meko's API URL isn't
      known the share and invite routes answer 409 saying to set the web app URL, and the invite sync
      skips Meko's invites. A share link's token is never logged or stored, and joining by a pasted
      link sends its token to Meko only for a link on the configured web app, and only when picked.
- [ ] Nothing that identifies a person or their work goes into the usage stats report
      (`callhome.ts`): only counts, enums, versions, plain model ids and file extensions. A new
      field is added to `CallHomeReport` and `docs/call-home-report.schema.json` together, and
      paths are reduced to extensions where they're read (`filetypes.ts`), never stored.
- [ ] Token usage stays incremental (byte offsets) and deduplicated by message id; subagent
      transcripts are included; archived sessions still count toward their project.

## 5. UI

- [ ] The UI only calls routes in `src/shared/types.ts`, and types match on both sides.
- [ ] Both light and dark themes work; state is shown with shape as well as color.
- [ ] Copy is plain and specific; errors say how to fix the problem.
- [ ] The product is called **AgentK** in visible text (never AgentK Local, its old name), and so
      is the agent doing the work. Where one sentence needs both, name the part of the product
      (the AgentK UI, the AgentK server). **Claude** is only for the Claude Code process and its
      terminal.
- [ ] The change works on a phone: see [src/web/REVIEW.md](src/web/REVIEW.md) for the phone and touch checks.

## 6. Tests, docs, release

- [ ] `npm run typecheck`, `npm run lint` and `npm test` pass; new behavior has a test.
- [ ] **No redundant or needless tests.** A test earns its place by catching a bug no other test
      would. Flag (minor) a test that repeats a case another test already covers (the same path
      reached through another entry point, or the same assertion with different values), one
      that checks a library, the language, a type or a constant rather than our code, one that
      asserts internals (call counts, private state) where the result a caller sees would do, and
      one that only restates copy or a config value. Cases of one rule go in one table-driven test
      (`it.each`), and a fix extends the nearest existing test rather than adding a new one or a
      new file. Name which tests to drop or merge. Doc-only, comment-only and pure-refactor
      changes need no new tests.
- [ ] **Docs are updated in the same change** (see the table in AGENTS.md): the part of
      `docs/design/` that covers the change (and the contents table in `docs/design.md` for a new
      part), `README.md`, `AGENTS.md` (AgentK's,
      `src/server/AGENTS.md` or `src/web/AGENTS.md`, or the repo root's for the repo layout), `REVIEW.md` and `src/shared/types.ts`. Stale
      docs are a major finding. Screenshots in `docs/screenshots/` are retaken separately, not
      with every UI change, so don't flag a change for leaving them out of date.
- [ ] The PR title (the squash-merge commit subject) passes the repo root's `scripts/check-title.sh`, its component
      names where the change is (`agentk` for AgentK, `repo` for root-only changes, a list such as
      `agentk,meko` only when the change ships in each, since it releases each), and its type
      fits the whole change: `feat`/`fix`/`perf`/`deps` ship a release, `ci`/`test`/`docs`/`nit`/
      `chore` don't. Comment-only changes are `nit`. The `PR title` workflow checks the format, not the type or which component fits.
