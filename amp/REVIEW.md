# PR review guide — amp

**Read [`REVIEW.md`](../REVIEW.md) at the repository root first.** It carries the
size gates, the review budget, the reporting rules, the model-selection rule and the
**upgrade-safety check** — all of which apply to every review in this repo. This
file is only the amp-specific layer: the traps and checks particular to this
codebase, organized by area.

## 1. Area-specific concerns

Weight these by what the diff touches.

**Compute / storage engine** (`compute/` — pageserver, safekeeper,
compute_ctl). Correctness is the dominant concern here:

- **Every YB statement must be idempotent**, DDL included. `YsqlClient` retries
  dropped connections and can replay an in-flight statement whose commit status
  is unknown. New write paths need `INSERT ... ON CONFLICT` (or
  `yb_enable_upsert_mode`), absolute `UPDATE`s (never `SET col = col + 1`),
  `WHERE`-keyed `DELETE`s, and `IF NOT EXISTS` on DDL. A non-idempotent statement is a latent corruption bug even if it passes
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
- **Storage formats and the links between processes are read by other builds.** A
  pageserver and its safekeeper can run different versions, so the SK→PS WAL wire
  format must stay version-portable
  ([`compute/architecture/high-level.md`](compute/architecture/high-level.md#pssk-co-location-is-not-an-invariant)).
  The same holds for compute↔pageserver traffic while endpoints roll one at a time. What
  lands in `yb_storage` (`blobs`, `wals`, the `blob_namespace` enum) outlives the build
  that wrote it and must stay readable, including after a rollback. A new encoding or
  enum value that an older reader can't handle needs negotiation or a stated downgrade
  floor.

**Control plane** (`controller/`). The concerns are cluster-state consistency,
not row-level storage:

- Changes to the node/branch/endpoint registries or the `yb_storage` schema
  provisioning on controller startup should be checked against
  [`controller/architecture/sc-integration.md`](controller/architecture/sc-integration.md);
  changes to the `resource_token` store against **both**
  [`compute/architecture/compute-fencing.md`](compute/architecture/compute-fencing.md)
  (the endpoint write lease) and
  [`controller/architecture/ac-locking.md`](controller/architecture/ac-locking.md)
  (the control plane's task and endpoint locks) — they lease the same rows through
  the same `token_acquire` / `token_release`.
- YB writes here follow the same idempotency contract as above.
- **The controller resumes tasks an older controller started.** A step `kind` that is
  renamed or removed, or step `args` whose shape no longer decodes, fails every in-flight
  task of that kind after the upgrade
  ([`controller/architecture/ac-tasks.md`](controller/architecture/ac-tasks.md)). Keep
  old kinds registered and old args decodable for at least one release.
- **Every file under `controller/migrations/` raises the downgrade floor.** The
  controller runs `m.Up()` before it serves (`cmd/controller/main.go`), and an older
  binary refuses to start against a database at a version it doesn't embed
  ([`runbooks/startup-failures.md`](runbooks/startup-failures.md#b2-migration-no-migration-found-for-version-n)).
  Additive SQL doesn't change that, so ask for the floor in the PR. Separately, the SQL
  still expands, then contracts, so a controller that is already running keeps working
  through the rollout.
- **The API has clients the controller doesn't upgrade.** The `reconcile-agent` builds
  against a vendored copy of `openapi_spec.yaml`, and MCP clients call the tools
  directly. Request and response changes are additive; a removed or renamed field or
  route is a break.

**Deploy / Kubernetes** (`deploy/`). Mostly operational correctness:

- The pull secret expires ~12h, so `up.sh` / `redeploy.sh` must keep re-minting
  it each run — a change that drops that leaves idle namespaces unable to pull.
- Image registry/repo references stay pointed at the canonical ECR repos.
- Update the matching [`runbooks/*.md`](runbooks/) when a deploy failure mode or
  its fix changes.
- **Chart changes land as `helm upgrade` on existing releases.** A new controller
  re-renders endpoint releases that an older one installed. A newly required value, a
  renamed key, or an install-only hook breaks releases that predate it, so check the
  upgrade path, not only a fresh `up.sh`.

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
- A new or bumped download (a Dockerfile `curl`/`wget`/`ADD`, a fetch script,
  a workflow step) goes through `scripts/safe_curl.sh` and pins its URL in
  [`downloads_checksum.txt`](downloads_checksum.txt), whose header has the
  rules. An inline `sha256sum -c` or a floating URL ("latest", an unversioned
  dist path) is a finding.

## 4. Hygiene — amp-specific

The repo-wide rules (no agent attribution, what the body must explain, full URLs) are
in the root [`REVIEW.md`](../REVIEW.md). On top of those:

- Title follows the subject convention (`[<KEY>-<num>] <area>: <sub-component>: <title>`
  with a Jira key from any project such as `[AMP-55]`, or `[#<issue>]` for a GitHub issue).
- Links the tracked issue with `Closes <full issue URL>` when it completes one — a
  bare `#<n>` is unsafe, since AMP's issues were renumbered by the monorepo migration.
- New AMP work is tracked as a Jira ticket under an epic, not a new GitHub issue —
  see [`docs/issue-tracking.md`](docs/issue-tracking.md).
