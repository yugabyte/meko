# PR review guide — amp

**Read [`REVIEW.md`](../REVIEW.md) at the repository root first.** It carries the
size gates, the review budget, the reporting rules and the model-selection rule —
all of which apply to every review in this repo. This file is only the
amp-specific layer: the traps and checks particular to this codebase, organized by area.

## 1. Area-specific concerns

Weight these by what the diff touches.

**Compute / storage engine** (`compute/` — pageserver, safekeeper, compute_ctl,
endpoint_storage). Correctness is the dominant concern here:

- **Every YB statement must be idempotent**, DDL included. `YsqlClient` retries
  dropped connections and can replay an in-flight statement whose commit status
  is unknown. New write paths need `INSERT ... ON CONFLICT` (or
  `yb_enable_upsert_mode`), absolute `UPDATE`s (never `SET col = col + 1`),
  generation-fenced `UPDATE`s, `WHERE`-keyed `DELETE`s, and `IF NOT EXISTS` on
  DDL. A non-idempotent statement is a latent corruption bug even if it passes
  tests. See
  [`compute/architecture/yugabyte.md`](compute/architecture/yugabyte.md#client-side-retry-contract--every-yb-query-must-be-idempotent).
  This holds for **test SQL too** — fixtures go through the same client and the
  same replay. A statement that can't be made replay-safe at the call site means
  `YsqlClient` needs a non-retrying path; flag that rather than approving a
  local workaround.
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

## 2. On-demand CI checks

Some suites are opt-in, requested with `/ci <suite>` lines in the PR body — see
[`docs/ci-directives.md`](docs/ci-directives.md) for the list and when each
applies. If a relevant on-demand check is missing for what the diff touches,
flag it, ask the author to add the `/ci` line, and wait for it to go green before
approving.

## 3. Companion updates travel in the same PR

Confirm the diff didn't leave these stale (see the `commit` skill):

- `*/architecture/*.md` docs when behavior/design changed.
- Tests covering the new/changed behavior.
- Inspection/diagnostic tooling under `tools/` and `scripts/` when the thing
  they inspect changed.
- Operational `runbooks/*.md` when a failure mode or its fix changed.
- On Rust changes, `cargo clippy` is clean (`-D warnings`).

## 4. Hygiene — amp-specific

The repo-wide rules (no agent attribution, what the body must explain, full URLs) are
in the root [`REVIEW.md`](../REVIEW.md). On top of those:

- Title follows the subject convention (`[#<issue>] <area>: <sub-component>: <title>`).
- Links the tracked issue with `Closes <full issue URL>` when it completes one — a
  bare `#<n>` is unsafe, since AMP's issues were renumbered by the monorepo migration.
- Area label (`amp_*`) + `AMP:` milestone on the linked issue — see
  [`docs/issue-tracking.md`](docs/issue-tracking.md).
