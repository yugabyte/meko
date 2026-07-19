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

## Suites

| Directive | Runs | Request it when the diff… |
|---|---|---|
| `integration_slow` | slow integration scenario suite (`test/integration/compute/run.sh slow`) | is correctness-sensitive (see below) |
| `generate_image` | controller container image build (`deploy/controller/Dockerfile`) | changes the `controller/` deploy surface or `deploy/controller/Dockerfile` |

## When a change is correctness-sensitive (`integration_slow`)

Regular CI (`pr-build.yml`) runs only the **fast** integration suite on PRs;
`integration_slow` adds the **slow** suite. Add `/ci integration_slow` whenever
the diff touches the LSN / consistency, read-write, WAL, durability, or
image-materialiser paths — concretely (non-exhaustive):

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
