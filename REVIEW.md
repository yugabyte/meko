# REVIEW.md

What to check when reviewing a change to **AgentK Local**, whether you're a person or an agent
(AgentK). Background and invariants are in [AGENTS.md](AGENTS.md); the design is in
[docs/design.md](docs/design.md).

Order findings by severity: **blocker** (breaks an invariant, loses work, or opens a security
hole), **major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

## 1. Sessions and recovery

- [ ] Nothing makes tmux or Claude a child of the server process or its service (systemd unit or
      launchd job). Code that manages the services goes through `src/server/service.ts`.
- [ ] Server restarts and updates leave running sessions untouched.
- [ ] `recover()` / `reconcile()` stay idempotent: live panes are left alone, missing panes are resumed,
      and sessions still being created or launched are skipped (so Claude never starts twice).
- [ ] Nothing respawns a live pane, and a tmux error other than "no server" is never read as every
      pane being gone.
- [ ] Values in the launcher are set after the login shell's profile runs, and inherited
      `AK_HOME`/`MEKO_CHECKPOINT_INTERVAL` are cleared.
- [ ] Auto-continue guards hold (once per 15 minutes; after two auto-continues in an hour the next
      mid-turn recovery is parked).
- [ ] Status changes come from hook events or pane checks, not guesses. Deliberate stops (archive,
      cleanup) don't show up as "exited".
- [ ] The status line relay stays fast and quiet: it reports in the background, always runs the
      user's own status line with the same input, and never prints anything of its own.
- [ ] tmux commands work on tmux 2.7 (no `new-session -e`; window options via `set-window-option`).
- [ ] The installer never installs a tool (or runs Meko's setup) without first showing what it
      will run and asking (or `--install-deps`), verifies downloads by checksum or integrity hash
      wherever one is published (Claude Code's installer script from claude.ai publishes none, so
      it's only fetched over HTTPS), and checks the result rather than trusting the package manager.
- [ ] Shell commands and scripts work with macOS's BSD tools as well as GNU ones (no `mv -T`,
      `stat -c`, `sha256sum`-only, `sleep infinity`, `sed -i` without a suffix, `readlink -f`).

## 2. Security

- [ ] New `/api/*` routes require login unless they're one of the three public auth routes, or the
      request has local access (`localAccess()`: loopback bind, loopback peer, localhost `Host`).
      Nothing widens local access to a network bind, another `Host`, or a forwarded address. Access
      checks key on the matched route, never the raw URL (the router decodes `/%61pi/`).
- [ ] Request bodies are type-checked before use (a wrong type is a 4xx, never a 500), and nothing
      re-adds a `text/plain` or form body parser (HTML forms must not be able to post to the API).
- [ ] Writes and WebSocket upgrades still pass the origin check; cookies stay `HttpOnly`,
      `SameSite=Strict`, and `Secure` under TLS.
- [ ] No user input reaches a shell. `execFile`/`spawn` take argument arrays; launcher values go
      through `shq()`.
- [ ] Secrets never land in logs, `config.toml` (unless the user typed them), the database, or the repo.
      The Meko key never goes to the browser (Settings shows only its last four characters), a
      Meko URL or key change is checked against Meko before it's saved, and a typed API URL is
      checked without the key. Remote URLs lose their credentials before they go into a datapack
      description (`withoutCredentials`).
- [ ] Login limits hold (per address or IPv6 /64, and site-wide), and nothing sent to a browser can
      queue in memory without limit (terminal output pauses the pty; state snapshots are skipped).
- [ ] The hook socket stays owner-only (directory 0700, socket 0600), and events are matched by
      per-session token.
- [ ] Nothing internal (hostnames, mount paths, tokens) is committed; the repo will be public.
- [ ] Repo files served to the browser (image previews) are limited to image types inside the
      worktree or base commit, and carry the sandboxing content policy.
- [ ] Agent-generated HTML (Artifact designs) only renders in the sandboxed design frame: no
      `allow-same-origin`, and the frame's content policy still blocks network requests and comes
      first in the frame's document. Markdown designs are sanitized with no forms or styles.
- [ ] The Design-stage guard denies edits inside the worktree and main checkout, and fails open
      (no answer means allow) so a slow or restarting server never blocks Claude.

## 3. Lifecycle and GitHub

- [ ] Gates (approve design, open PR, merge) are only passed by the user, never automatically.
- [ ] Each session kind keeps to its stages (`KIND_STAGES`): an Exploration session never leaves
      Explore, and a Quick Code session never enters Design. Code that switches on `stage` handles
      `explore`.
- [ ] Prompts (stage, review and review-comment prompts) are only typed into a session when Claude is
      idle, never into a permission or question prompt; otherwise they're queued (the review prompt
      in `state.db`, so it survives a restart). Prompts to one session go one at a time.
- [ ] Merge is refused while `mergeBlockers` isn't empty; blockers are phrased for users.
- [ ] Cleanup never force-removes a worktree with uncommitted changes, and never deletes the
      user's main checkout or repo.
- [ ] The only change made in the user's main checkout is fast-forwarding the base branch when a
      session starts (`merge --ff-only` with autostash off in the main checkout, or a non-forced
      `git fetch . refs/remotes/origin/<b>:refs/heads/<b>`; never inside another worktree), never a reset or force.
- [ ] `/agentk review force` is only posted when the user asks.
- [ ] CI-fix prompts go out once per PR head, never ahead of a queued review prompt, and stop after
      three failing heads in a row; a queued one is dropped when the head moves or CI stops failing.
- [ ] Merge waits for AgentK only once AgentK is active on the PR, and never in *People only*.

## 4. Data and Meko

- [ ] Schema changes are a new migration; shipped migrations are unchanged.
- [ ] Every project still has exactly one dedicated datapack; sessions get `MEKO_AGENT_ID` and the pin.
- [ ] Several projects can share a repo: per-project state (worktrees, tmux names, pins, lookups) is
      keyed by project id, not repo path, and git work on the repo only touches the session's own branch.
- [ ] Meko failures degrade gracefully (banner, logged warning) and never kill running sessions.
- [ ] Tests don't touch the real `~/.claude.json`, Meko, or GitHub. Meko's REST API goes through
      `fakeMekoApi()` in `test/helpers.ts`, and the Meko tab's checks through `stubMekoTools()`.
- [ ] Share invites: a project created from an invite accepts it in Meko before writing anything
      locally, the first invite sync doesn't notify about shares that already existed, and each
      share notifies once. Sharing features work in both API and link mode.
- [ ] Token usage stays incremental (byte offsets) and deduplicated by message id; subagent
      transcripts are included; archived sessions still count toward their project.

## 5. UI

- [ ] The UI only calls routes in `src/shared/types.ts`, and types match on both sides.
- [ ] Both light and dark themes work; state is shown with shape as well as color.
- [ ] Copy is plain and specific; errors say how to fix the problem.
- [ ] The product is called **AgentK Local** in visible text. Text about the work names **AgentK**;
      **Claude** is only for the Claude Code process and its terminal.
- [ ] The change works on a phone: see [src/web/REVIEW.md](src/web/REVIEW.md) for the phone and touch checks.

## 6. Tests, docs, release

- [ ] `npm run typecheck`, `npm run lint` and `npm test` pass; new behavior has a test.
- [ ] **Docs are updated in the same change** (see the table in AGENTS.md): `docs/design.md`,
      `README.md`, `CONTRIBUTING.md`, `AGENTS.md`, `REVIEW.md` and `src/shared/types.ts`. Stale
      docs are a major finding. Screenshots in `docs/screenshots/` are retaken separately, not
      with every UI change, so don't flag a change for leaving them out of date.
- [ ] Commit subjects follow Conventional Commits, and there's no agent attribution.
      `feat`/`fix`/`perf` will ship a release; make sure that's intended.
