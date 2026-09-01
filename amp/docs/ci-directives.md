# On-demand CI directives (`/ci`)

Some CI suites are too slow or heavy to run on every PR. Request them per-PR by
adding a `/ci <suite>` line to the **PR description** (body). Every suite obeys the
same rule, implemented once in
[`.github/actions/ci-directives`](../../.github/actions/ci-directives/action.yml) and
called by [`amp-on-demand-checks.yml`](../../.github/workflows/amp-on-demand-checks.yml) and
[`amp-control-plane-gke.yml`](../../.github/workflows/amp-control-plane-gke.yml).

- One or more suites per line, space/comma separated:
  `/ci amp_integration_slow amp_generate_image`.
- A suite runs on each new commit while its line is present. The directive is read
  when the PR **opens** and on every **push** — editing the description fires
  nothing, so put the `/ci` lines in before you open the PR. Adding one later takes
  a commit (the `git-push` skill covers it).
- Every qualifying event runs the suites the body asks for — there is no
  per-commit dedupe, so re-opening a PR re-runs them. The `Decide …` job annotates
  exactly what it decided and why. To run a suite again against the same commit
  without a new one, use *Re-run jobs* in the Actions tab.

Two suites also run **automatically** — no `/ci` line needed — on any PR whose diff
adds or modifies files under a path they own, because each is what exercises those
files: `amp_integration_slow` on `amp/test/integration/compute/slow/` (the scenarios
it runs) and `amp_extensions_local` on `amp/deploy/extensions/` (the extension build
pipeline it drives).

## Suites

| Directive | Runs | Request it when the diff… |
|---|---|---|
| `amp_integration_slow` | slow integration scenario suite (`test/integration/compute/run.sh slow`) | is correctness-sensitive (see below) |
| `amp_generate_image` | controller container image build (`deploy/controller/Dockerfile`) | changes the `controller/` deploy surface or `deploy/controller/Dockerfile` |
| `amp_extensions_local` | local-deploy extension regression test (`test/integration/compute/extensions.sh --regress`): builds the real extension image and runs each externalized extension's **own upstream regression suite** (pgvector/pg_cron/... via `pg_regress`, hexabridge via its `scripts/regression.sh`) against a throwaway `yb_amp_local` cluster ([`amp-on-demand-checks.yml`](../../.github/workflows/amp-on-demand-checks.yml)). Also **automatic** when the diff touches `deploy/extensions/`, and nightly on `main` | want the cheap local proof of an extension-pipeline change without a GKE deploy. The lighter twin of `amp_deploy_gke_extensions`; builds the ABI-anchor PG18 base (~30 min cold, then restored from `build.sh --cache`'s registry layer cache — the nightly run keeps that cache warm). |
| `amp_deploy_gke` | build + push the four images to Quay and deploy to the itest-dev GKE cluster + smoke test ([`amp-control-plane-gke.yml`](../../.github/workflows/amp-control-plane-gke.yml)) | changes the `deploy/` k8s surface (Helm charts, `up-gke.sh`, Dockerfiles) or the control-plane deploy path. Also runs nightly on `main`. |
| `amp_deploy_gke_extensions` | a superset of `amp_deploy_gke`: also builds + pushes the externalized-extension images (`deploy/extensions/build.sh --push`: the ABI-anchor PG18 base + every extension in the manifest) to Quay, and runs the smoke test with `SMOKE_EXTENSIONS=1` (each extension's ImageVolume opt-in + fork inheritance) | changes `deploy/extensions/` (the extension build pipeline / manifest) or the endpoint's extension-mount path. Adds a cold PG18 base build (~30 min) on top of `amp_deploy_gke`. |
| `amp_deploy_gke_ycp` | a superset of `amp_deploy_gke`: also builds + pushes the **YCP control-plane** image (`meko/api_server`, `quay.io/yugabyte/ycp-api-server`) AND the **reconcile-agent** image (`ycp/Dockerfile.reconcile-agent`, `quay.io/yugabyte/ycp-reconciler-agent`) from PR source, stands up an ephemeral control plane (`api_server --control-plane-mode` + Postgres) in the CI namespace via [`ycp-up.sh`](../deploy/ycp-up.sh), registers an `amp` environment bound to a generated api key id, deploys the reconcile-agent (`agent.*` Helm values + the `amp-agent-api-key` Secret), and runs [`ycp-reconcile-smoke.sh`](../deploy/ycp-reconcile-smoke.sh): it declares a pool→cluster→branch→endpoint through YCP desired state and asserts the authoritative agent creates the tree, reports it `READY` in `GET /api/v1/environments`, and garbage-collects it on withdrawal. Combine with `amp_deploy_gke_extensions` to also assert a branch-extensions edit converges — the branch reports it and the redeployed endpoint comes back Ready with it mounted — which is skipped otherwise, since the extension images are only pushed by that directive. The API-driven `smoke-test.sh` is skipped in this mode (the agent would GC its out-of-band resources). | changes the amp↔YCP integration surface (`ycp/reconcile/`, `ycp/cmd/reconcile-agent/`, the `agent.*` Helm wiring, the controller's `/api/v1/ycp/binding` endpoint) or `meko/api_server`'s fleet-registry / desired-state / status-report path. Directive-only (not nightly). |

## When a change is correctness-sensitive (`amp_integration_slow`)

Regular CI (`amp-pr-build.yml`) runs only the **fast** integration suite on PRs and
main pushes; `amp_integration_slow` adds the **slow** suite. The slow suite also
runs **daily on main** on a schedule, so a regression is caught within a day even
if no one requested it. However, this is only a safety net, not a substitute: add
`/ci amp_integration_slow` to the PR description whenever the diff touches the LSN /
consistency, read-write, WAL, durability, or image-materialiser paths, so it's
caught *before* merge — concretely (non-exhaustive):

- **LSNs & consistency** — `consistent_lsn`, `prev_record_lsn`, record-boundary
  math, `branch_state`, flush/commit LSN tracking, write-lease fencing.
- **Read/write path** — `page_store` / `page_writer`, getpage, `UnifiedPages`,
  the smgr extension's page reads or WAL writes.
- **WAL** — safekeeper WAL ingest/read, `yb_wal_storage`, WAL apply / replay /
  catch-up, walproposer/walreceiver, segment/chunk handling.
- **Image materialiser** — the image rule / delta-chain length bound, chain
  reconstruction, walredo folding.
- **Idempotency** — any new or changed YB write (must stay idempotent per
  [`AGENTS.md`](../AGENTS.md) → "Every YB query must be idempotent").

When in doubt, add it: a false positive costs one CI run; a false negative can
land a corruption-class bug the fast suite never exercised. Pure docs / comment
/ test-only / UI / deploy-manifest changes are **not** correctness-sensitive.
