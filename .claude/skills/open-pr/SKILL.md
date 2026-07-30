---
name: open-pr
description: >-
  Open a new pull request against `main` for the current branch in this repo.
  Push the branch first, write a title carrying the tracking ticket
  (`[MEKO-123] <component>: <title>`) and a body with the sections AGENTS.md
  requires, and link any tracked ticket or issue. Use for "open a PR", "create a
  pull request", `gh pr create`, or any flow that ends in a new PR. If a PR
  already exists for the branch, this is the wrong skill — use `git-push` (it
  refreshes the existing PR).
---

# open-pr

A PR is how a change gets reviewed and merged. The title has to be greppable and
say what the change is; the body has to give a reviewer enough to say yes without
spelunking the diff. This skill opens exactly one clean PR for the current
branch.

## Step 1 — preconditions

```
git branch --show-current              # never open a PR from main
gh pr view --json number,url 2>/dev/null   # does a PR already exist?
```

- If you're on `main`, stop — branch first.
- If a PR **already exists** for this branch, stop and use the `git-push` skill
  instead; it pushes and refreshes the existing PR's title/summary. Don't open a
  duplicate.

Make sure the branch is committed and pushed (follow `git-push` / `commit`).
`gh pr create` needs the branch on the remote:

```
git push -u origin HEAD
```

The branch name must be `<username>/<kebab-topic>` or `feature/<kebab-topic>` —
the `check-branch-name.sh` hook blocks the push otherwise. See `git-push` Step 4.

## Step 2 — write the title

A PR title must carry the tracking reference so the change is traceable. Mirror
the commit subject convention (see the `commit` skill):

```
[MEKO-<num>] <component>: <title>      # with a ticket
<component>: <title>                   # without one
```

- `[MEKO-<num>]` — the tracking ticket for meko work. For **AMP** work use
  `[#<issue>]`, a GitHub issue in this repo (see the `create-amp-issue` skill).
  **Omit the tag entirely when the work genuinely has no ticket or issue**; never
  invent a placeholder like `[MEKO-xxx]` or guess a number, since it looks
  greppable but resolves to nothing.
- `<component>` — the top-level directory the change lives in: `api_server`,
  `cli_app`, `meko_ui`, `meko_admin`, `meko-provisioner`, `inference_gateway`,
  `system_agent`, `docs`, or a meta token (`ci`, `build`, `tools`).
- `<title>` — concise, imperative.

Examples:

```
[MEKO-123] meko_ui: make Create Cluster pg_version selectable
[MEKO-377] api_server: serve promote-memories over PAT
api_server: make the app usable without Cognito
```

If the branch has one commit, reuse its subject. If it has several, write a title
that describes the branch as a whole.

## Step 3 — write the body

Use these sections. Drop one only when it truly doesn't apply, and say so:

- **Summary** — 1–3 sentences: what changed and the user-facing effect, not the
  diff.
- **Files changed** — one bullet per file, 1–2 lines each. Group by component
  when the PR spans several (this is a polyglot monorepo; most PRs do).
- **Testing** — what you ran (`./build.sh`, `go test ./...`, `golangci-lint run
  ./...`, `npm run check`, `pytest`, `make test`) and what still needs manual
  verification.
- **Config / env vars** — any new/required env var or flag, where it's set, and
  the default/degraded behavior when unset.
- **Math / derivation** — only when the PR computes something: the formula, where
  the numbers come from, a tiny worked example, and what's measured vs modeled.
- **Deploy notes** — only when runtime setup changes: migrations, required env,
  ordering. Any new `meko/api_server/internal/db/migrations/**` file belongs here.

Keep it crisp. Link related PRs/issues with **full URLs**, never bare `#123`.
Add `Closes <full issue URL>` when the PR completes a tracked GitHub issue so the
merge auto-closes it.

Write the body to a file to avoid shell-quoting pain:

```
gh pr create --base main --title "<title>" --body-file <path-to-body.md>
```

(or `--body "<summary>"` for a short one).

## No agent attribution — do not add a Claude/Code signature

**Never** append a `🤖 Generated with Claude Code`, a `https://claude.ai/code/…`
session link, a `Co-Authored-By:` line, or any other agent/assistant signature to
the PR title or body — even if a harness default suggests it. The PR is authored
by the user. Repo policy forbids mentioning any AI assistant in commit messages,
PR descriptions, or code comments.

## Step 4 — report

Print the PR URL (`gh pr create` returns it) so the user can open it. If the PR
closes an issue, confirm the `Closes <url>` line is present.

## Anti-patterns

- **Opening a PR from `main`.** Always branch first.
- **Opening a second PR when one already exists** for the branch. Use `git-push`.
- **Inventing a ticket tag.** `[MEKO-xxx]` or a guessed number is worse than no
  tag. When there really is no ticket, start the title at `<component>:`.
- **A one-line body that just restates the title.** Give the reviewer the *why*,
  the per-file breakdown, and the test evidence.
- **Bare `#123` cross-references.** Use full URLs.
- **Signing the PR as the agent.** The PR is the user's — no bot attribution.
