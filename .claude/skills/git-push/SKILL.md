---
name: git-push
description: >-
  Push the current branch to `origin` in this repo. On a first push, check the
  branch name matches `<username>/<kebab-topic>` (or `feature/<kebab-topic>`)
  and set the upstream. **Before pushing, verify the components you touched are
  formatted (`.github/scripts/format.sh --check`, `npm run check`,
  `pycodestyle`) and that no outgoing commit is co-authored by or attributed to
  Claude or Cursor** (fix formatting and scrub any agent trailer first). **If a PR already exists for the branch,
  refresh its title and summary from the branch's landed commits after pushing**
  so the PR always reflects what's actually on it. Use for `git push`, "push my
  changes", "push and update the PR", or any flow that ends in pushing the
  current branch.
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

Meko is a polyglot monorepo, so check only the components this branch actually
touches. Start from the changed paths:

```
git diff --name-only origin/main..HEAD | cut -d/ -f1 | sort -u
```

Then run the checks for those components. Each exits non-zero and prints the
offenders when something is wrong:

```
# Formatting — the languages this branch touches, so a toolchain you don't need
# can't block the push. CI's required `Format` check runs all of them.
langs=$(git diff --name-only --diff-filter=d origin/main..HEAD |
  sed -nE 's/.*\.go$/go/p; s/.*\.rs$/rust/p; s/.*\.py$/python/p; s/.*\.sh$/shell/p;
    s/.*\.(ts|tsx|js|jsx|mjs|cjs|css|scss)$/ts/p; s#(^|.*/)[^/.]+$#shell#p' | sort -u)
# (no extension -> shell: format.sh also formats scripts it finds by shebang)
[ -z "$langs" ] || .github/scripts/format.sh --check $langs

# TypeScript — if meko/meko_ui/ or meko/meko_admin/ changed
(cd meko/meko_ui    && npm run check)      # tsc
(cd meko/meko_admin && npm run check)      # tsc

# Python — if meko/system_agent/ changed
(cd meko/system_agent && mypy . && pycodestyle .)

# Python — if meko/inference_gateway/ changed (run its tests)
(cd meko/inference_gateway && pytest)
```

If they all pass, move on. Otherwise **do not push** — fix and re-check:

```
.github/scripts/format.sh <the languages it flagged>   # it prints the exact command
git diff                                               # review what it rewrote
```

Line limits are **120 for Go, 100 elsewhere**; tabs for Go, 4 spaces for Python,
2 spaces for YAML/shell (`.editorconfig`, `meko/.golangci.yml`, `meko/tox.ini`).

Formatting can land as its own commit — `git commit -am "<area>: gofmt"` —
since PRs merge squashed, so there's no need to amend or rebase it back into the
commit that introduced the drift. Format only what you're landing: if the
formatter rewrites files unrelated to your change, leave those out.

Re-run the checks until they pass, then continue.

Optional but cheap when you've touched Go — `golangci-lint run ./...` in the
module you changed (`meko/.golangci.yml` at the repo root governs `cli_app` and
`api_server`; `meko-provisioner` uses `GOTOOLCHAIN=go1.25.8 make lint`).

## Step 3 — verify no commit is agent-attributed (hard gate)

Repo policy is explicit: commit messages, PR descriptions, and code comments must
not mention any AI assistant, and no
`Co-Authored-By` line may reference one. Commits are authored by the user alone
— both the **author/committer identity** and the **message** must be free of any
agent/assistant attribution (a `Co-Authored-By:` naming Claude or Cursor, a
`Claude-Session:` / session-link trailer, an `_automated · …` signature, a
`🤖 Generated with/by …` line, or an anthropic/cursor `noreply@` address).

The `commit` skill is where this is supposed to be gotten right, per-commit, at
commit time — but push is the last line of defense, and it has to catch commits
that never went through that skill: cherry-picks, rebases, amends, and anything
other tools or harness defaults authored. So re-scan **all** outgoing commits
here before anything leaves the machine. Each scan falls back to `main..HEAD`
when `origin/main` isn't fetched (fresh clone), so the check never silently
passes on missing input:

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
(`hsunder/`, `anijhawan/`, `rajmaddy/`) and the topic is lowercase and
hyphenated: `hsunder/sk-drop-table-branch-discovery`,
`anijhawan/app-no-cognito`. The one non-alias prefix allowed is `feature/` —
`feature/artifact-upload`. When the work tracks a ticket or GitHub issue, lead
the topic with its number — `hsunder/361-gc-trim-unify`,
`feature/270-free-tier-quota`.

Conventional-commit-style type prefixes are **not** branch prefixes: `fix/`,
`feat/`, `bug/`, `chore/`, `meko/docs/`, `test/`, `refactor/`, `release/` are all
rejected, as are bare names with no prefix at all (`change-qs`, `meko1`). Older
branches on the remote predate this rule; don't copy them.

Two things enforce this, covering different halves of the rule. The
**`branch-names-require-prefix` ruleset** on `yugabyte/meko` restricts creation
on `refs/heads/*`, and GitHub's `*` doesn't span `/`, so a bare name (`fix-wal`)
is rejected at push time — but it can only require *a* prefix, not that the
prefix is a username. The **`check-branch-name.sh` hook** (a `PreToolUse` hook
wired in `.claude/settings.json`) blocks a non-conforming `git push` locally and
is what rejects `fix/`, `feat/`, and friends.

Check the ruleset's `enforcement` if a bare name unexpectedly succeeds — it has
been toggled off before:

```
gh api repos/yugabyte/meko/rulesets --jq '.[] | "\(.name)\t\(.enforcement)"'
```

Check the name *before* the first push regardless — once it's public, renaming
means deleting the remote branch and re-pushing, which breaks any open PR and
anyone who fetched it:

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

If that fails (no PR), you're done — this skill does not open PRs.

If a PR **does** exist, re-derive its title and body from the commits now on the
branch and update it so it matches reality:

```
git log --oneline origin/main..HEAD    # re-read the full set of commits
```

- **Title** — must start with the tracking reference so the change is traceable
  (the `PR title` check fails the PR otherwise), mirroring the commit subject
  style: `[<KEY>-<num>] <component>: <title>` with the Jira ticket from any
  project (e.g. `[MEKO-123] meko_ui: make Create Cluster pg_version selectable`,
  `[AMP-55] ...`, `[CLOUDGA-37256] ...`), or `[#<issue>]` for work tracked as a
  GitHub issue in this repo. Several tags go back to back: `[AMP-55][#512] ...`.
  The tag is required: when there is no ticket or issue, file one — never invent
  `[MEKO-xxx]` or guess a number. If the branch grew from one commit to several,
  the title should describe the branch as a whole, not just the first commit.
- **Summary** — update the body so it describes what the branch actually does
  now, using the sections below (drop one only when it truly doesn't apply, and
  say so):
  - **Summary** — 1–3 sentences: what changed and the user-facing effect, not the diff.
  - **Files changed** — one bullet per file, 1–2 lines each (group by component when the PR spans several).
  - **Testing** — what you ran (`go build` / `go vet` / `golangci-lint` / `npm run check` / `pytest`) and what still needs manual verification.
  - **Config / env vars** — any new/required env var or flag, where it's set, and the default/degraded behavior when unset.
  - **Math / derivation** — only when the PR computes something.
  - **Deploy notes** — only when runtime setup changes: migrations, required env, ordering.
  - **Upgrade safety** — only when the diff changes a format (wire/API shape, stored format, migration, persisted task, CRD, chart values): how old and new builds behave side by side and after a rollback, and the downgrade floor (see `open-pr`).

  Drop stale bullets for work that's no longer on the branch; add bullets for
  work that arrived since the PR was opened. Link related PRs/issues with full
  URLs, never bare `#123`.

Apply the update:

```
gh pr edit <number> --title "<refreshed title>" --body "<refreshed summary>"
```

Keep the body's `Closes <url>` lines when you rewrite it: on merge they are what
closes the PR's GitHub issues and Jira tickets (see the `open-pr` skill).

Only touch the title/body when the branch content actually changed what they
should say. A one-commit follow-up that the existing summary already covers
needs no edit — don't churn the description for its own sake.

### If the refresh ADDS a `/ci` directive, it will not fire on its own

Editing the description does not trigger CI — directives are read when the PR
opens and on every push. So an `amp_*` `/ci` line you add while refreshing the
summary requests a suite that never runs, and the PR still reads green. Follow it
with a commit:

```
git commit --allow-empty -m "ci: rerun" && git push
```

Then confirm the suite actually reported (`gh pr checks <number>`): a `skipping`
bucket for it means it did not run, and its coverage is not yours to claim.

### If this push addressed review comments

Use the **`resolve-pr-comments`** skill. Pushing the fix is only half of it: the
threads stay open until someone resolves them, and each automated finding wants a
👍/👎 saying whether the finding was *correct* — a vote that is separate from
whether you fixed anything, and the only feedback loop that improves the reviewer.

Do it **after** this push, never before. Resolving against a local edit claims
something the PR does not yet show.

## No agent attribution

Never add a `🤖 Generated with Claude Code`, session link, `Co-Authored-By`, or
any other agent/assistant signature to the PR title or body. The PR is the
user's. This is repo policy, not just style — see
the `commit` skill's no-attribution rule. The Meko context lines
(`cloud.mekodata.ai://…` and `meko_conversation_id:…`) at the end of a PR body
are allowed and must be kept when refreshing the body — see the `open-pr` skill.

## Anti-patterns

- **Publishing an off-convention branch name on the first push.** Renaming after
  the fact means deleting the remote branch and breaking any open PR — check the
  name in Step 4 while it's still local-only.
- **Using a type prefix as the branch prefix.** `fix/…`, `feat/…`, `chore/…` are
  commit-message conventions, not branch names. Use `<username>/…` or `feature/…`.
- **Running the full monorepo's format/lint checks on every push.** Step 2 is
  scoped to the components the branch touched; checking the rest is wasted time.
- **Force-pushing a shared branch to "fix" a rejected push.** Reconcile with the
  remote first; a blind force-push destroys others' commits.
- **Pushing new commits but leaving a now-wrong PR summary.** The whole point of
  Step 5 — a stale summary misleads every reviewer who trusts it.
- **Rewriting the PR body on every trivial push.** Only refresh when the branch's
  actual content changed what the description should say.
