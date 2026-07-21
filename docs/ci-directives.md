# On-demand CI directives (`/ci`)

Some CI suites are too slow or heavy to run on every PR. Request them per-PR by
adding a `/ci <suite>` line to the **PR description** (body); the
[`on-demand-checks.yml`](../.github/workflows/on-demand-checks.yml) workflow
parses the body and runs the matching jobs.

- One or more suites per line, space/comma separated:
  `/ci integration_slow generate_image`.
- A suite re-runs on every push while its line is present. Adding a `/ci` line
  to the description runs the newly-added suite immediately; editing the
  description **without** changing the `/ci` lines runs nothing.

`integration_slow` also runs **automatically** — no `/ci` line needed — on any
PR whose diff adds or modifies files under `test/integration/compute/slow/`
(evaluated when the PR opens/reopens or a commit is pushed). The slow suite is
what exercises those scenarios, so changing them always runs it.

## Suites

| Directive | Runs | Request it when the diff… |
|---|---|---|
| `integration_slow` | slow integration scenario suite (`test/integration/compute/run.sh slow`) | is correctness-sensitive (see below) |
| `generate_image` | controller container image build (`deploy/controller/Dockerfile`) | changes the `controller/` deploy surface or `deploy/controller/Dockerfile` |

## When a change is correctness-sensitive (`integration_slow`)

Regular CI (`pr-build.yml`) runs only the **fast** integration suite on PRs and
main pushes; `integration_slow` adds the **slow** suite. The slow suite also
runs **daily on main** on a schedule, so a regression is caught within a day even
if no one requested it. However, this is only a safety net, not a substitute: add
`/ci integration_slow` to the PR description whenever the diff touches the LSN /
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
