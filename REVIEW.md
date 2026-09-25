# REVIEW.md

What to check when reviewing a change to **AgentK Local**, whether you're a person or an agent
(AgentK). Background and invariants are in [AGENTS.md](AGENTS.md); the design is in
[docs/design.html](docs/design.html).

Order findings by severity: **blocker** (breaks an invariant, loses work, or opens a security
hole), **major** (wrong behavior users will hit), **minor** (clarity, style, small gaps).

## 1. Sessions and recovery

- [ ] Nothing makes tmux or Claude a child of the server process or its systemd unit.
- [ ] Server restarts and updates leave running sessions untouched.
- [ ] `recover()` / `reconcile()` stay idempotent: live panes are left alone, missing panes are resumed.
- [ ] Auto-continue guards hold (once per 15 minutes; parked after two recoveries in an hour).
- [ ] Status changes come from hook events or pane checks, not guesses. Deliberate stops (archive,
      cleanup) don't show up as "exited".
- [ ] tmux commands work on tmux 2.7 (no `new-session -e`; window options via `set-window-option`).

## 2. Security

- [ ] New `/api/*` routes require login unless they're one of the three public auth routes.
- [ ] Writes and WebSocket upgrades still pass the origin check; cookies stay `HttpOnly`,
      `SameSite=Strict`, and `Secure` under TLS.
- [ ] No user input reaches a shell. `execFile`/`spawn` take argument arrays; launcher values go
      through `shq()`.
- [ ] Secrets never land in logs, `config.toml` (unless the user typed them), the database, or the repo.
- [ ] The hook socket stays owner-only (directory 0700, socket 0600), and events are matched by
      per-session token.
- [ ] Nothing internal (hostnames, mount paths, tokens) is committed; the repo will be public.

## 3. Lifecycle and GitHub

- [ ] Gates (approve design, open PR, merge) are only passed by the user, never automatically.
- [ ] Prompts are only typed into a session when Claude is idle; otherwise they're queued.
- [ ] Merge is refused while `mergeBlockers` isn't empty; blockers are phrased for users.
- [ ] Cleanup never force-removes a worktree with uncommitted changes, and never deletes the
      user's main checkout or repo.
- [ ] `/agentk review force` is only posted when the user asks.

## 4. Data and Meko

- [ ] Schema changes are a new migration; shipped migrations are unchanged.
- [ ] Every project still has exactly one dedicated datapack; sessions get `MEKO_AGENT_ID` and the pin.
- [ ] Meko failures degrade gracefully (banner, logged warning) and never kill running sessions.
- [ ] Tests don't touch the real `~/.claude.json`, Meko, or GitHub.
- [ ] Token usage stays incremental (byte offsets) and deduplicated by message id; subagent
      transcripts are included; archived sessions still count toward their project.

## 5. UI

- [ ] The UI only calls routes in `src/shared/types.ts`, and types match on both sides.
- [ ] Both light and dark themes work; state is shown with shape as well as color.
- [ ] Copy is plain and specific; errors say how to fix the problem.
- [ ] The product is called **AgentK Local** in visible text.

## 6. Tests, docs, release

- [ ] `npm run typecheck`, `npm run lint` and `npm test` pass; new behavior has a test.
- [ ] **Docs are updated in the same change** (see the table in AGENTS.md): `docs/design.html`,
      `README.md`, `CONTRIBUTING.md`, `AGENTS.md`, `REVIEW.md`, `src/shared/types.ts`, and the
      README screenshots for visible UI changes. Stale docs are a major finding.
- [ ] Commit subjects follow Conventional Commits, and there's no agent attribution.
      `feat`/`fix`/`perf` will ship a release; make sure that's intended.
