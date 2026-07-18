---
name: merge-pr
description: >-
  Merge a pull request in this repo — but only with the user's **explicit
  approval** and after confirming its CI checks have **passed** (not pending,
  not failing). After a successful merge, switch off the merged branch and
  delete the local branch. Use for "merge the PR", "merge this", `gh pr merge`,
  or any flow that ends in merging a PR. Never merge on your own initiative,
  and never with red or still-pending CI.
---

# merge-pr

Merging is the irreversible step — once it's on `main`, everyone builds on it.
So the bar is: **CI is green first**, then merge, then clean up the local branch
so a stale pointer doesn't linger. Skipping the CI check is how a broken `main`
happens.

## Step 1 — identify the PR

```
gh pr view --json number,title,url,state,headRefName 2>/dev/null
```

If run from the PR's branch, `gh pr view` finds it. Otherwise pass the number:
`gh pr view <number> ...`. Confirm it's `OPEN`. Note the `headRefName` — you'll
delete that local branch at the end.

## Step 2 — confirm CI passed (hard gate)

Do **not** merge until every required check is a pass. Check the rollup:

```
gh pr checks <number>
```

- **All passing** → proceed to Step 3.
- **Any still pending / in progress** → wait and re-check; do not merge yet.
  (Poll, e.g. `gh pr checks <number> --watch`, or re-run after a bit.)
- **Any failing** → stop. Report which check failed and its log. The fix is a new
  commit on the branch (push it, let CI re-run), not merging past red.

Also confirm it's actually mergeable (no conflicts, review state satisfied):

```
gh pr view <number> --json mergeable,mergeStateStatus,reviewDecision
```

`mergeStateStatus` of `BLOCKED` / `DIRTY` / `BEHIND` means it's not ready — resolve
(rebase on `main`, get the approval) before merging. **Never bypass a failing or
pending gate** (no admin override) to force a merge.

## Step 3 — merge (stay on the branch)

**First, confirm the user explicitly approved this merge.** Merging is never
proactive — not to "validate" a change, not because CI just went green. If the
user hasn't said to merge, stop here and ask; report the PR is green and ready
and wait for their go-ahead.

Merge from the PR's branch — where you already are. **Do not detach first**:
`gh pr merge` reads the current branch and errors (`could not determine current
branch`) on a detached HEAD.

```
gh pr merge <number> --squash --delete-branch
```

`--delete-branch` reliably deletes the **remote** branch. It also tries to delete
the *local* branch, but in a multi-worktree checkout that step can fail — it tries
to switch this worktree to `main`, which git refuses when `main` is checked out in
another worktree. That error is **harmless**: the merge and the remote-branch
delete already succeeded, and Step 4 does the local cleanup deterministically, so
don't react to it. (`--squash` is the common default; match the repo's merge
style.)

Do not add any agent/assistant signature to the merge commit message.

## Step 4 — land on the default + drop the local branch

Two commands, worktree-safe no matter what Step 3's local step did:

```
git fetch origin main -q && git checkout --detach origin/main
git branch -D <headRefName> 2>/dev/null || true
```

- `git checkout --detach origin/main` — **not** `git checkout main` — lands the
  worktree on the merged tip and always works, even when `main` is checked out in
  another worktree (git refuses the same branch in two worktrees).
- `git branch -D <headRefName>` removes the local branch: force (`-D`) because a
  squash merge leaves no merge ancestry, and idempotent (`|| true`) because
  Step 3 may already have deleted it. Don't rely on `--delete-branch` for the
  local branch — always run this.

## Step 5 — report

Confirm: PR merged (with the merge commit / squash SHA), any linked issue closed,
and the local branch deleted. Note where the worktree now sits (detached at the
merged `main` tip).

## Anti-patterns

- **Merging with pending or failing CI.** The whole point of Step 2. A green
  `main` depends on never merging past red.
- **Force-merging with admin bypass** to skip a gate. If the gate is wrong, fix
  the gate; don't route around it.
- **Leaving the merged branch checked out and undeleted.** Stale local branches
  pile up and confuse the next `git branch` — delete after merge.
