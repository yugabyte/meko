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
| `amp_deploy_gke_extensions` | a superset of `amp_deploy_gke`: also builds + pushes the externalized-extension images (`deploy/extensions/build.sh --push`: the ABI-anchor PG18 base + every extension in the manifest) to Quay, and runs the smoke test with `SMOKE_EXTENSIONS=1` (each extension's ImageVolume opt-in + fork inheritance, and the documentdb gateway driven through an `nlb_external` load balancer allow-listed to the runner) | changes `deploy/extensions/` (the extension build pipeline / manifest) or the endpoint's extension-mount path. Adds a cold PG18 base build (~30 min) on top of `amp_deploy_gke`. |
| `amp_deploy_gke_ycp` | a superset of `amp_deploy_gke`: also builds + pushes the **YCP control-plane** image (`meko/api_server`, `quay.io/yugabyte/ycp-api-server`) AND the **reconcile-agent** image (`ycp/Dockerfile.reconcile-agent`, `quay.io/yugabyte/ycp-reconciler-agent`) from PR source, stands up an ephemeral control plane (`api_server --control-plane-mode` + Postgres) in the CI namespace via [`ycp-up.sh`](../deploy/ycp-up.sh), registers an `amp` environment bound to a generated api key id, deploys the reconcile-agent (`agent.*` Helm values + the `amp-agent-api-key` Secret), and runs [`ycp-reconcile-smoke.sh`](../deploy/ycp-reconcile-smoke.sh): it declares a pool→cluster→branch→endpoint through YCP desired state and asserts the authoritative agent creates the tree, reports it `READY` in `GET /api/v1/environments`, replaces an `nlb_internal` endpoint's `allowed_cidrs` in place (Service source ranges updated, pod and address untouched), and garbage-collects it on withdrawal. Combine with `amp_deploy_gke_extensions` to also assert a branch-extensions edit converges — the branch reports it and the redeployed endpoint comes back Ready with it mounted — which is skipped otherwise, since the extension images are only pushed by that directive. The API-driven `smoke-test.sh` is skipped in this mode (the agent would GC its out-of-band resources).<br><br>This suite also exercises the **BYOC telemetry path** (as does `amp_deploy_gke_foundation`, which passes `YCP_ENABLED=1` too): the chart's preflight probes the real metrics door and waits for an export, and the control plane checks the endpoint at enroll. Neither fails anything — both warn — so `ycp-reconcile-smoke.sh` asserts it instead, and is the only thing standing between a broken telemetry path and a green build. Both suites therefore need the `BYOC_METRICS_PASSWORD` and `DATADOG_API_KEY` repository secrets and a door reachable from itest-dev (`BYOC_METRICS_URL` repository variable, default devcloud). | changes the amp↔YCP integration surface (`ycp/reconcile/`, `ycp/cmd/reconcile-agent/`, the `agent.*` Helm wiring, the controller's `/api/v1/ycp/binding` endpoint) or `meko/api_server`'s fleet-registry / desired-state / status-report path. Directive-only (not nightly). |
| `amp_deploy_gke_documentdb` | the extension images of `amp_deploy_gke_extensions` (it needs documentdb's), then — after the smoke — **upstream documentdb's own functional-test gate** against the deployed gateway ([`deploy/documentdb-functional/`](../deploy/documentdb-functional/README.md)): the ~10,500-test allowlist upstream gates its PRs on, taken from the documentdb release amp ships, minus amp's reasoned [`exclusions.yml`](../deploy/documentdb-functional/exclusions.yml), run as in-cluster shard pods through the endpoint's Service and logged in as the default user. Passes only if every gated test passes; the summary is on the job page and the reports are the `documentdb-functional-results` artifact. Directive/dispatch-only (`documentdb_functional`), not nightly. | changes the documentdb extension or gateway, the documentdb version, the gateway's chart wiring, or anything on the wire-protocol path to the compute. |
| `amp_deploy_gke_foundation` | the ONLY suite that exercises the **operator-managed foundation**: builds the amp images plus the YCP control-plane and reconcile-agent images, plus the **foundation** image, and stands the universe up the way a customer would: the `meko-foundation` CRD and controller charts, then a `Foundation` object the pod converges — CRDs, the `yugaware` chart with the Kubernetes operator enabled, a `YBProvider` and `YBUniverse` CR and the tserver RPC Service. No `foundation` CLI runs anywhere; the same Go is reached through the reconcile loop, so this is the only coverage the pod has. `up-gke.sh` is then pointed at the result with `FOUNDATION=external` instead of applying its single-node `yugabyted` StatefulSet, and creates `yb_storage` itself — the deployer no longer does. That universe is **encrypted** — node-to-node and client-to-node, on cert-manager certificates from the same `amp-ca` root the endpoints use — so this is also the only suite that exercises a TLS-enabled operator universe. TLS is not an input here: both flags are create-time only, so the spec decides and `up-gke.sh` is handed the shape as `YB_SSLMODE`, read off the object's `status.address`. It then runs the agent-driven reconcile smoke against that universe, asserts the tserver RPC Service has endpoints, asserts the node certificates name the endpoint Service and that a `verify-full` session to it is really encrypted, and deletes the `Foundation` under `deletionPolicy: Delete` before checking that no cluster-scoped RBAC leaked. It also provokes the failure paths the loop exists to survive: a `Foundation` edited into something YBA rejects, the fix going through afterwards, and a helm release wedged mid-operation. Its own workflow ([`amp-foundation-gke.yml`](../../.github/workflows/amp-foundation-gke.yml)) on its own cron (03:17 UTC, away from the 07:17 slot the others share) with a single fixed concurrency group, because a foundation holds a ~4.5 CPU yugaware pod for the better part of an hour. **Also nightly on `main`.** | change the `foundation/` module, `up-gke.sh`'s `FOUNDATION` switch, or anything about how the backing YugabyteDB is brought up. Every other deploy path still uses single-node `yugabyted`, deliberately — this is where the operator path soaks. |

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
