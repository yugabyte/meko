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

`amp_integration_slow` also runs **automatically** — no `/ci` line needed — on any
PR whose diff adds or modifies files under `amp/test/integration/compute/slow/`. The
slow suite is what exercises those scenarios, so changing them always runs it.

## Suites

| Directive | Runs | Request it when the diff… |
|---|---|---|
| `amp_integration_slow` | slow integration scenario suite (`test/integration/compute/run.sh slow`) | is correctness-sensitive (see below) |
| `amp_generate_image` | controller container image build (`deploy/controller/Dockerfile`) | changes the `controller/` deploy surface or `deploy/controller/Dockerfile` |
| `amp_deploy_gke` | build + push the four images to Quay and deploy to the itest-dev GKE cluster + smoke test ([`amp-control-plane-gke.yml`](../../.github/workflows/amp-control-plane-gke.yml)) | changes the `deploy/` k8s surface (Helm charts, `up-gke.sh`, Dockerfiles) or the control-plane deploy path. Also runs nightly on `main`. |

## When a change is correctness-sensitive (`amp_integration_slow`)

Regular CI (`amp-pr-build.yml`) runs only the **fast** integration suite on PRs and
main pushes; `amp_integration_slow` adds the **slow** suite. The slow suite also
runs **daily on main** on a schedule, so a regression is caught within a day even
if no one requested it. However, this is only a safety net, not a substitute: add
`/ci amp_integration_slow` to the PR description whenever the diff touches the LSN /
consistency, read-write, WAL, durability, or image-materialiser paths, so it's
caught *before* merge — concretely (non-exhaustive):

- **LSNs & consistency** — `consistent_lsn`, `prev_record_lsn`, record-boundary
  math, `timeline_state`, flush/commit LSN tracking, generation fencing.
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
