---
name: merge-pr
description: >-
  Merge a pull request in this repo — but only after confirming its CI checks
  have **passed** (not pending, not failing). After a successful merge, switch
  off the merged branch and delete the local branch. Use for "merge the PR",
  "merge this", `gh pr merge`, or any flow that ends in merging a PR. Never
  merge with red or still-pending CI.
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

## Step 3 — detach, then merge

Get **off** the PR branch before merging, then merge with `--delete-branch`.
With the branch unchecked-out, `--delete-branch` removes the remote *and* local
branch in one shot — nothing left to clean up:

```
git checkout --detach                          # leave the branch (no-op if already detached)
gh pr merge <number> --squash --delete-branch
```

Why detach first: git won't delete a branch that's checked out, and `gh`'s
fallback — switching your worktree to `main` — **fails** when `main` is already
checked out in another worktree (the normal multi-worktree setup), which is what
leaves the local branch behind and forces a manual mop-up. Detaching sidesteps
both. (`--squash` is the common default; match the repo's merge style.)

Do not add any agent/assistant signature to the merge commit message.

## Step 4 — land on the updated default

Point the worktree at the freshly-merged default — one command:

```
git fetch origin main -q && git checkout --detach origin/main
```

Use `--detach origin/main`, **not** `git checkout main`: the detach form always
works, including when `main` is checked out in another worktree (git refuses the
same branch in two worktrees). Step 3 already deleted the branch; the one-liner
`git branch -D <headRefName>` clears it only in the rare case it survived.
That's the whole cleanup.

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
