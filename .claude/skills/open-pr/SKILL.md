---
name: open-pr
description: >-
  Open a new pull request against `main` for the current branch in this repo.
  Push the branch first, write a title in the repo's subject convention and a
  summary that explains what changed and why, and link any tracked issue. Use
  for "open a PR", "create a pull request", `gh pr create`, or any flow that
  ends in a new PR. If a PR already exists for the branch, this is the wrong
  skill — use `git-push` (it refreshes the existing PR).
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

## Step 2 — write the title

Mirror the commit subject convention (see the `commit` skill):

```
[#<issue>] <area>: <sub-component>: <title>
```

- `[#<issue>]` — optional leading tag when the PR advances a tracked issue.
- `<area>` — `compute`, `control_plane`, or a meta token (`pm`, `docs`, `ci`,
  `build`, `tools`).
- `<sub-component>` — optional (`pageserver`, `safekeeper`, …).
- `<title>` — concise, imperative.

If the branch has one commit, reuse its subject. If it has several, write a title
that describes the branch as a whole.

## Step 3 — write the summary

The body should let a reviewer understand the change without reading every line:

- **What** changed and **why** (the motivation / problem being solved).
- Anything non-obvious: migration/rollout notes, a behavior change, a tradeoff.
- **Test evidence** — what you ran and the result (unit / integration; cite the
  integration run for corruption-class changes per `e2e-before-corruption-claim`).
- `Closes #<issue>` / `Fixes #<issue>` when the PR completes a tracked issue, so
  the merge auto-closes it.

### On-demand CI directives — request the right suites

Some CI suites are opt-in, requested with `/ci <suite>` lines in the PR body.
Set the ones the change needs. The directive list, how the lines fire, and the
criteria live in [`docs/ci-directives.md`](../../../docs/ci-directives.md).

Write it to a file to avoid shell-quoting pain:

```
gh pr create --base main --title "<title>" --body-file <path-to-body.md>
```

(or `--body "<summary>"` for a short one).

## No agent attribution — do not add a Claude/Code signature

**Never** append a `🤖 Generated with Claude Code`, a `https://claude.ai/code/…`
session link, a `Co-Authored-By:` line, or any other agent/assistant signature to
the PR title or body — even if a harness default suggests it. The PR is authored
by the user. The body is just the summary; nothing else.

## Step 4 — report

Print the PR URL (`gh pr create` returns it) so the user can open it. If the PR
closes an issue, confirm the `Closes #<num>` line is present.

## Anti-patterns

- **Opening a PR from `main`.** Always branch first.
- **Opening a second PR when one already exists** for the branch. Use `git-push`.
- **A one-line body that just restates the title.** Give the reviewer the *why*
  and the test evidence.
- **Signing the PR as the agent.** The PR is the user's — no bot attribution.
