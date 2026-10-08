---
name: open-pr
description: >-
  Open a new pull request against `main` for the current branch in this repo.
  Push the branch first, write a title carrying the tracking ticket
  (`[MEKO-123]`, `[AMP-55]` or any Jira key: `[<KEY>-<num>] <component>: <title>`) and a body with the sections AGENTS.md
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

A PR title must start with the tracking reference so the change is traceable. The
`PR title` CI check fails the PR otherwise (only bot-opened PRs are exempt), and the
squash merge makes the title the commit subject on `main`. Mirror the commit
subject convention (see the `commit` skill):

```
[<KEY>-<num>] <component>: <title>              # with a Jira ticket
[#<issue>] <component>: <title>                 # with a GitHub issue in this repo
[<KEY>-<num>][#<issue>] <component>: <title>    # tracked in several places
```

- `[<KEY>-<num>]` — the tracking Jira ticket, in whichever project tracks the
  work: `MEKO-`, `AMP-`, `CLOUDGA-`, `PLAT-`, … (the project is all caps). Work
  tracked as a GitHub issue in this repo uses `[#<issue>]` (see the
  `create-amp-issue` skill). Several tags go back to back with no space between
  them, and one space follows the last — no colon.
  **The tag is required.** When the work has no ticket or issue, file one first;
  never invent a placeholder like `[MEKO-xxx]` or guess a number, since it looks
  greppable but resolves to nothing.
- `<component>` — the top-level directory the change lives in: `api_server`,
  `cli_app`, `meko_ui`, `meko_admin`, `meko-provisioner`, `inference_gateway`,
  `system_agent`, `docs`, or a meta token (`ci`, `build`, `tools`).
- `<title>` — concise, imperative.

Examples:

```
[MEKO-123] meko_ui: make Create Cluster pg_version selectable
[MEKO-377] api_server: serve promote-memories over PAT
[AMP-55] test/functional: add stress/sqlancer, SQLancer logic-bug oracles against amp endpoints (opt-in)
[CLOUDGA-37256] api_server: fix the amp install hint's credentials, telemetry and foundation steps
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
- **Upgrade safety** — only when the diff changes a format: a wire or API shape, a
  stored format, a migration, a persisted task, a CRD or chart values
  ([`AGENTS.md`](../../../AGENTS.md#every-change-must-be-upgrade-safe)). Say how the old
  and new builds behave side by side and after a rollback. Give the rollout order when
  there is one, and the downgrade floor: a new migration file always sets one, since the
  previous build can't start past it.

Keep it crisp — size the Summary to the change and hold Files changed to a line
each. Say **why**, always: a reviewer who has to reverse-engineer the motivation is
the expensive case. What wastes their time is the other stuff — a description that
narrates the diff, or a paragraph where a line would do
([`AGENTS.md > Prose discipline`](../../../AGENTS.md#prose-discipline--write-for-the-reader-not-for-volume)).
Link related PRs/issues with **full URLs**, never bare `#123`.

### Closes lines — say which tickets the merge finishes

When the PR completes a ticket, put a `Closes` line for it in the body, one per
line, at the start of the line:

```
Closes https://yugabyte.atlassian.net/browse/AMP-55
Closes https://github.com/yugabyte/meko/issues/123
```

- **Jira** (`AMP`, `MEKO`): on merge, the `jira-on-merge` workflow comments the
  PR link on the ticket and moves it to Done. It reads only lines that are
  exactly `Closes`, `Fixes` or `Resolves` (capitalized) followed by ticket URLs
  or keys, so put nothing else on the line. A key in the title or in prose is
  only a reference: `[AMP-55] ...` or "Fixes the flake AMP-73 reported" closes
  nothing.
- **Other Jira projects** (`CLOUDGA`, `PLAT`, …): still write the `Closes` line so
  the reader sees what the PR finishes, but the workflow leaves those tickets
  alone. Move them yourself once the PR merges.
- **GitHub issues**: GitHub closes them itself on merge.

Write it only for a ticket the PR finishes. Leave it off for partial progress, and
for tickets the PR merely mentions, such as bugs a new test found: those must stay
open.

### On-demand CI directives — request the right suites

Some AMP suites are too heavy to run on every PR and are opt-in, requested with
`/ci <suite>` lines in the PR body: `amp_integration_slow`, `amp_generate_image`,
`amp_deploy_gke`. They are `amp_`-prefixed because they belong to AMP — there is no
meko equivalent, and writing one on a meko PR really does deploy amp to itest-dev.
The full contract and what makes a change correctness-sensitive live in
[`amp/docs/ci-directives.md`](../../../amp/docs/ci-directives.md).

**Put them in the body you create the PR with.** The directive is read when the PR
opens and on every push — a later description edit fires nothing, so a suite added
afterwards needs a commit before it runs (the `git-push` skill covers that).

Getting this wrong is quiet, not loud: the PR reads green while the suite you asked
for never ran. If you claim a suite as evidence, confirm it actually reported —
`gh pr checks <number>` showing `skipping` for it means it did not.

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

**Exception — Meko context lines.** The body may end with the Meko datapack and
conversation the work drew on, and they stay through later body refreshes:

```
cloud.mekodata.ai://<datapack-id>;<datapack-name>
meko_conversation_id:<id>
```

These are not attribution (see the root `REVIEW.md` hygiene rules).

## Step 4 — report

Print the PR URL (`gh pr create` returns it) so the user can open it. If the PR
closes an issue, confirm the `Closes <url>` line is present.

## Anti-patterns

- **Opening a PR from `main`.** Always branch first.
- **Opening a second PR when one already exists** for the branch. Use `git-push`.
- **Inventing a ticket tag.** `[MEKO-xxx]` or a guessed number resolves to
  nothing. When there really is no ticket, file one and tag that.
- **A one-line body that just restates the title.** Give the reviewer the *why*,
  the per-file breakdown, and the test evidence.
- **Bare `#123` cross-references.** Use full URLs.
- **Signing the PR as the agent.** The PR is the user's — no bot attribution.
