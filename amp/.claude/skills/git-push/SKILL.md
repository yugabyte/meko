---
name: git-push
description: >-
  Push the current branch to `origin` in this repo. On a first push, check the
  branch name matches `<username>/<kebab-topic>` and set the upstream.
  **Before pushing, verify the tree is formatted (`cargo fmt
  --check` and `gofmt -l`) and that no outgoing commit is co-authored by or attributed to
  Claude or Cursor** (fix formatting and scrub any agent trailer first). **If a PR
  already exists for the branch, refresh its title and summary from the
  branch's landed commits after pushing** so the PR always reflects what's
  actually on it. Use for `git push`, "push my changes",
  "push and update the PR", or any flow that ends in pushing the current branch.
---

# git-push

Pushing isn't just moving commits — if the branch already has an open PR, the
PR's title and body are part of what you're publishing. A branch whose commits
have moved on but whose PR still describes the old plan is a lie to every
reviewer. So this skill couples the push with keeping the PR description honest.

## Step 1 — know what you're pushing

```
git branch --show-current
git status --short          # anything uncommitted you meant to include?
git log --oneline origin/main..HEAD   # the commits this branch adds
```

If there are uncommitted changes that belong in this push, commit them first
(follow the `commit` skill). Don't push a half-finished tree.

## Step 2 — verify the tree is formatted (hard gate)

Nothing gates formatting in CI, so push is the last place to catch it. Both
checks print the offenders and stay silent when clean:

```
cargo fmt --check                 # Rust
gofmt -l controller test/functional   # Go — `make fmt` does NOT cover this
```

If both are silent, move on. Otherwise **do not push** — fix and re-check:

```
make fmt                          # cargo fmt + ruff, in place
gofmt -w <the files gofmt -l listed>
git diff                          # review what it rewrote
```

Formatting can land as its own commit — `git commit -am "<area>: cargo fmt"` —
since PRs merge squashed, so there's no need to amend or rebase it back into the
commit that introduced the drift. Format only what you're landing: if the
formatter rewrites files unrelated to your change, leave those out.

Re-run both checks until they're silent, then continue.

## Step 3 — verify no commit is agent-attributed (hard gate)

Commits are authored by the user alone — both the **author/committer identity**
and the **message** must be free of any agent/assistant attribution (a
`Co-Authored-By:` naming Claude or Cursor, a `Claude-Session:` / session-link
trailer, an `_automated · …` signature, a `🤖 Generated with/by …` line, or an
anthropic/cursor `noreply@` address). The `commit` skill is where this is
supposed to be gotten right, per-commit, at commit time — but push is the last
line of defense, and it has to catch commits that never went through that skill:
cherry-picks, rebases, amends, and anything other tools or harness defaults
authored. So re-scan **all** outgoing commits here before anything leaves the
machine. Each scan falls back to `main..HEAD` when `origin/main` isn't fetched
(fresh clone), so the check never silently passes on missing input:

```
# 1. Author + committer identity must be the user — never Claude or Cursor.
#    Word boundaries so "precursor" / "claudette" don't false-positive, and
#    only anthropic/cursor bot noreply@ addresses (not GitHub's user noreply).
(git log origin/main..HEAD --format='%an <%ae>%n%cn <%ce>' 2>/dev/null \
  || git log main..HEAD --format='%an <%ae>%n%cn <%ce>') | \
  grep -iE '\b(claude|cursor|anthropic)\b|noreply@(anthropic|cursor)\b' \
  && echo "FOUND agent author/committer — reset authorship before pushing" \
  || echo "author clean"

# 2. Commit messages must carry no agent trailer/signature.
(git log origin/main..HEAD --format='%H%n%B' 2>/dev/null \
  || git log main..HEAD --format='%H%n%B') | \
  grep -iEn 'co-authored-by:.*\b(claude|cursor|anthropic)\b|claude-session|generated (with|by) (claude|cursor)|_automated|noreply@(anthropic|cursor)\b' \
  && echo "FOUND agent attribution — scrub before pushing" \
  || echo "message clean"
```

If both print `clean`, proceed. If either finds something, **do not push** — fix
it first:

- **Message trailer/signature** — strip the offending lines. Most recent commit
  only → `git commit --amend` and delete the lines. Older / several commits →
  rewrite them (`git rebase -i origin/main`, mark each `reword`, remove the
  lines), or reset and re-commit with a clean message.
- **Author/committer identity** — first make sure your git identity is the user's
  (`git config user.name` / `user.email`), then re-stamp authorship. Most recent
  commit → `git commit --amend --reset-author --no-edit`. Several commits →
  `git rebase origin/main --exec 'git commit --amend --reset-author --no-edit'`.

Re-run both scans until each prints `clean`, then push. This gate is not optional
just because a harness default set the author or added the trailer — a pushed
commit carries the attribution permanently into history. (Cheapest to avoid
entirely by getting authorship right at commit time — see the `commit` skill.)

## Step 4 — push

**First push only — check the branch name.** Branches here are
`<username>/<kebab-topic>`, where `<username>` is the author's short handle
(`hsunder/`, `kannan/`, `spilshchikov/`) and the topic is lowercase and
hyphenated: `hsunder/sk-drop-table-branch-discovery`,
`spilshchikov/release-workflow-fallback`. When the work tracks a GitHub issue,
lead the topic with its number — `arpang/361-gc-trim-unify`.

The prefix is **enforced by the remote**: the `branch-names-require-prefix`
ruleset restricts creation on `refs/heads/*`, and GitHub's `*` doesn't span `/`,
so a bare name (`fix-wal`) is rejected at push time. Convention, not enforced:
don't use someone else's prefix, and keep the topic `kebab-case`, not
`snake_case`.

Check the name *before* the first push anyway — once it's public, renaming means
deleting the remote branch and re-pushing, which breaks any open PR and anyone
who fetched it:

```
git branch --show-current                      # does it match <username>/<kebab-topic>?
git branch -m <username>/<kebab-topic>         # rename in place if not
```

If the branch has no upstream yet (first push):

```
git push -u origin HEAD
```

Otherwise:

```
git push
```

If the push is rejected because the remote moved, **do not** force-push a shared
branch blindly. Rebase or merge `origin/<branch>` first, resolve, then push. A
`--force-with-lease` is acceptable only on a branch that's yours alone and only
after you've reconciled with the remote.

## Step 5 — if a PR exists, refresh its title and summary

Check whether this branch already has an open PR:

```
gh pr view --json number,title,url 2>/dev/null
```

If that fails (no PR), you're done — this skill does not open PRs (use the
`open-pr` skill for that).

If a PR **does** exist, re-derive its title and body from the commits now on the
branch and update it so it matches reality:

```
git log --oneline origin/main..HEAD    # re-read the full set of commits
```

- **Title** — mirror the primary commit's subject style
  (`[#<issue>] <area>: <sub-component>: <title>`). If the branch grew from one
  commit to several, the title should describe the branch as a whole, not just
  the first commit.
- **Summary** — update the body so it describes what the branch actually does
  now: what changed, why, and anything a reviewer needs (test evidence, migration
  notes, `Closes #<issue>`). Drop stale bullets for work that's no longer on the
  branch; add bullets for work that arrived since the PR was opened.

Apply the update:

```
gh pr edit <number> --title "<refreshed title>" --body "<refreshed summary>"
```

Only touch the title/body when the branch content actually changed what they
should say. A one-commit follow-up that the existing summary already covers
needs no edit — don't churn the description for its own sake.

### If the refresh ADDS a `/ci` directive, it will not fire on its own

Editing the description does not trigger CI — directives are read when the PR
opens and on every push. So a `/ci <suite>` line you add while refreshing the
summary requests a suite that never runs, and the PR still reads green. Follow
it with a commit:

```
git commit --allow-empty -m "ci: rerun" && git push
```

Then confirm the suite actually reported (`gh pr checks <number>`): a `skipping`
bucket for it means it did not run, and its coverage is not yours to claim.

## No agent attribution

Never add a `🤖 Generated with Claude Code`, session link, `Co-Authored-By`, or
any other agent/assistant signature to the PR title or body. The PR is the
user's. (See the `open-pr` skill for the same rule at creation time.)

## Anti-patterns

- **Publishing an off-convention branch name on the first push.** Renaming after
  the fact means deleting the remote branch and breaking any open PR — check the
  name in Step 4 while it's still local-only.
- **Force-pushing a shared branch to "fix" a rejected push.** Reconcile with the
  remote first; a blind force-push destroys others' commits.
- **Pushing new commits but leaving a now-wrong PR summary.** The whole point of
  Step 5 — a stale summary misleads every reviewer who trusts it.
- **Rewriting the PR body on every trivial push.** Only refresh when the branch's
  actual content changed what the description should say.
