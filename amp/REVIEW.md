# PR review guide

Use this when reviewing a pull request in this repo (human or agent). A good
review covers whatever the change actually touches — correctness, tests, docs,
operational impact, hygiene — so weight your attention by what's in the diff.
This guide is the amp-specific layer on top of reading the code: the traps and
checks particular to this codebase, organized by area.

> **Report findings that matter.** Surface issues you're reasonably confident
> about and that carry real severity — don't report low-confidence guesses or
> low-severity nits. Tag each finding with its severity so the author can
> triage.
>
> **Keep the output reasonably sized.** Cover the findings, but write each one
> tersely — enough for the author to locate the issue and act on it (what's
> wrong, where, why it matters), and no more. Skip restating the diff, padding,
> and multi-paragraph explanations of one-line problems. Terse and actionable
> beats verbose.

> **Agents: pick the model by review complexity.** Run substantive reviews —
> anything touching correctness, concurrency, storage/WAL, or cross-service
> seams — on an Opus-class model (e.g. Opus 4.8); review judgment is the
> priority there, not speed or cost. Simple reviews (docs, comments, config,
> small mechanical diffs) may downgrade to Sonnet. Never use Haiku, and don't
> use Fable unless explicitly requested.

## 1. Before you start — gates

**Oversized PRs — ask before proceeding.** Check the size first. If any
threshold is exceeded, **stop and ask the user for permission to proceed** — the
point is to avoid accidentally burning tokens on a review that big:

- more than **20 commits**, or
- more than **100 files** changed, or
- more than **5000 lines** changed (additions + deletions).

If they decline, suggest the PR author run the review with their local AI agent
instead.

**Cap the review's own budget.** Stop and report progress rather than pushing
past either limit:

- **Wall-clock: 10 minutes.**
- **Tokens: 1M (~$25 on Opus).**

On hitting a cap, say how far you got and what's left. These are hard limits, not
targets — don't exceed them.

## 2. Area-specific concerns

Weight these by what the diff touches.

**Compute / storage engine** (`compute/` — pageserver, safekeeper, compute_ctl,
endpoint_storage). Correctness is the dominant concern here:

- **Every YB write must be idempotent.** `YsqlClient` retries dropped
  connections and can replay an in-flight write whose commit status is unknown.
  New write paths need `INSERT ... ON CONFLICT` (or `yb_enable_upsert_mode`),
  absolute `UPDATE`s (never `SET col = col + 1`), generation-fenced `UPDATE`s, or
  `WHERE`-keyed `DELETE`s. A non-idempotent write is a latent corruption bug even
  if it passes tests. See
  [`compute/architecture/yugabyte.md`](compute/architecture/yugabyte.md#client-side-retry-contract--every-yb-query-must-be-idempotent).
- **Corruption / WAL / data-loss / flush fixes** need more than a green
  `cargo build` and unit tests to believe. Confirm the author ran the integration
  suite (`test/integration/compute/run.sh fast`, and `slow` for sustained-write
  or restart changes) — unit tests don't exercise the cross-seam bugs. If the PR
  claims such a fix without integration evidence, ask for it before approving.
  (See the `integration-before-corruption-claim` skill.)

**Control plane** (`controller/`). The concerns are cluster-state consistency,
not row-level storage:

- Changes to generation authority, the node/branch/endpoint registries, or the
  `yb_storage` schema provisioning on controller startup should be checked
  against [`controller/architecture/sc-integration.md`](controller/architecture/sc-integration.md).
- YB writes here follow the same idempotency contract as above.

**Deploy / Kubernetes** (`deploy/`). Mostly operational correctness:

- The pull secret expires ~12h, so `up.sh` / `redeploy.sh` must keep re-minting
  it each run — a change that drops that leaves idle namespaces unable to pull.
- Image registry/repo references stay pointed at the canonical ECR repos.
- Update the matching [`runbooks/*.md`](runbooks/) when a deploy failure mode or
  its fix changes.

## 3. On-demand CI checks

Some suites are opt-in, requested with `/ci <suite>` lines in the PR body — see
[`docs/ci-directives.md`](docs/ci-directives.md) for the list and when each
applies. If a relevant on-demand check is missing for what the diff touches,
flag it, ask the author to add the `/ci` line, and wait for it to go green before
approving.

## 4. Companion updates travel in the same PR

Confirm the diff didn't leave these stale (see the `commit` skill):

- `*/architecture/*.md` docs when behavior/design changed.
- Tests covering the new/changed behavior.
- Inspection/diagnostic tooling under `tools/` and `scripts/` when the thing
  they inspect changed.
- Operational `runbooks/*.md` when a failure mode or its fix changed.
- On Rust changes, `cargo clippy` is clean (`-D warnings`).

## 5. Hygiene

- Title follows the subject convention (`[#<issue>] <area>: <sub-component>: <title>`).
- Body explains **what and why**, not just what; links the tracked issue with
  `Closes #<n>` when it completes one.
- **No agent/assistant attribution** in commits or the PR body — no
  "🤖 Generated with Claude Code" / "Generated by Claude" line, no
  `https://claude.ai/code/…` session link or session id, and no
  `Co-Authored-By:` trailer. The PR and its commits are authored by the human.
- Area label + milestone on the linked issue.
