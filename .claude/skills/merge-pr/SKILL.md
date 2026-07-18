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

## Step 3 — merge

Once CI is green and the PR is mergeable, merge it. Match the repo's merge style
(check recent history / branch-protection settings if unsure; `--squash` is a
common default):

```
gh pr merge <number> --squash --delete-branch
```

- `--delete-branch` deletes the **remote** branch and, when you're on it locally,
  the local one too.

Do not add any agent/assistant signature to the merge commit message.

## Step 4 — delete the local branch

After a successful merge, get off the merged branch and delete it locally so it
doesn't linger as a stale pointer:

```
git checkout main && git pull --ff-only origin main
git branch -d <headRefName> 2>/dev/null || true
```

The `2>/dev/null || true` keeps the step idempotent: `--delete-branch` in Step 3
usually already removed the local branch, and a plain `git branch -d` on a
missing branch exits non-zero and would abort a script.

- `git branch -d` (safe delete) refuses if the branch isn't merged — if it
  refuses after a squash-merge (squash leaves no merge ancestry) rather than
  because it's already gone, verify the PR really did merge, then
  `git branch -D <headRefName>` to force-delete the now-merged branch.
- Confirm it's gone either way — `git branch --list <headRefName>` prints
  nothing.

## Step 5 — report

Confirm: PR merged (with the merge commit / squash SHA), any linked issue closed,
and the local branch deleted. State the branch you're now on (`main`).

## Anti-patterns

- **Merging with pending or failing CI.** The whole point of Step 2. A green
  `main` depends on never merging past red.
- **Force-merging with admin bypass** to skip a gate. If the gate is wrong, fix
  the gate; don't route around it.
- **Leaving the merged branch checked out and undeleted.** Stale local branches
  pile up and confuse the next `git branch` — delete after merge.
