---
name: merge-pr
description: >-
  Merge a pull request in this repo — but only with the user's **explicit
  approval** and after confirming its CI checks have **passed** (not pending,
  not failing). Switch this checkout to a keep-branch **before** deleting the
  merged branch — never let cleanup strand the worktree on shared `main`. Use
  for "merge the PR", "merge this", `gh pr merge`, or any flow that ends in
  merging a PR. Never merge on your own initiative, and never with red or
  still-pending CI.
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
`gh pr view <number> ...`. Confirm it's `OPEN`. Note the `headRefName` — the
branch you'll move off of in Step 3.

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

## Step 3 — get the user's go-ahead, then move off the PR branch

**First, confirm the user explicitly approved this merge.** Merging is never
proactive — not to "validate" a change, not because CI just went green. If the
user hasn't said to merge, stop here and ask; report the PR is green and ready
and wait for their go-ahead.

**Then switch this checkout off the PR branch — before the merge.** This is the
step that keeps cleanup safe. `gh pr merge --delete-branch`, run *from* the PR
branch, switches the checkout to the default branch so it can delete the branch
out from under you. Where you want to land depends on whether this is a linked
worktree or the main working tree:

```
git fetch origin main -q
if [ "$(git rev-parse --git-dir)" = "$(git rev-parse --git-common-dir)" ]; then
  git switch main                                       # main working tree → main
else
  keep="$(basename "$(git rev-parse --show-toplevel)")" # linked worktree → its own branch, e.g. amp3
  git switch "$keep" 2>/dev/null || git switch -c "$keep" origin/main
fi
```

- **Main working tree** (`.git` is a directory; git-dir == git-common-dir): switch
  to `main` — that's this checkout's home, and no other worktree holds it.
- **Linked worktree** (git-dir != git-common-dir): switch to a long-lived
  per-worktree keep-branch (named after the worktree directory), **never** shared
  `main` — git refuses `main` in two worktrees, and landing there strands the
  worktree. Create it at `origin/main` if it doesn't exist.
- Either way, land on a real branch — **not** the PR branch (about to be deleted)
  and **not** a detached HEAD (`gh pr merge` errors `could not determine current
  branch` when detached).

## Step 4 — merge and delete (now safe)

```
gh pr merge <number> --squash --delete-branch
```

Because the checkout is no longer on the PR branch, `--delete-branch` deletes the
**remote** and **local** PR branch cleanly and has no reason to switch this
worktree anywhere — it stays on the keep-branch. (`--squash` is the common
default; match the repo's merge style.) Passing `<number>` lets `gh` find the PR
without being on its branch.

Do not add any agent/assistant signature to the merge commit message.

## Step 5 — report

Confirm: PR merged (with the merge commit / squash SHA), any linked issue closed,
and the local PR branch deleted. Note where the checkout now sits — `main` in the
main working tree, or the worktree's keep-branch (e.g. `amp3`) in a linked
worktree. A keep-branch created before the merge sits one commit behind — `git
pull` / rebase onto the new `main` when starting the next task.

## Anti-patterns

- **Merging with pending or failing CI.** The whole point of Step 2. A green
  `main` depends on never merging past red.
- **Force-merging with admin bypass** to skip a gate. If the gate is wrong, fix
  the gate; don't route around it.
- **Leaving the merged branch checked out and undeleted.** Stale local branches
  pile up and confuse the next `git branch` — delete after merge.
- **Deleting the PR branch while still on it in a worktree.** `--delete-branch`
  then switches the worktree to shared `main`, stranding it there. Move to the
  keep-branch first (Step 3).
