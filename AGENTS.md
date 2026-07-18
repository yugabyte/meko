# Agent guide — yb-amp

This is **yb-amp**: a
serverless Postgres-compatible storage system that runs PostgreSQL
computes over **YugabyteDB** as the durable backing store. If 
you're new to the codebase, read `compute/architecture/high-level.md` first,
then the subsystem doc that matches whatever you're touching.

## Meko datapack

This repo has a Meko datapack for agent memory:

- **Name**: `YugabyteDB AMP`
- **ID**: `fd3b43a0-ebd6-4732-958b-fae9a50a0505`

Use it when storing repo-scoped memories.

## Architecture map

Read in this order:

1. `compute/architecture/high-level.md` — component diagram, what lives where, the
   topology of a dev cluster.
2. `compute/architecture/yugabyte.md` — the single YB database (`yb_storage`)
   and its schemas (`public`, `pageserver`, `safekeeper`,
   `registry`), the `blobs` and `wals` tables, the
   `blob_namespace` ENUM, the `rpc_max_message_size` chunking story.
3. `compute/architecture/remote-storage.md` — `YbStore` design, `parse_key` /
   `reconstruct_key`, the round-trip-lossless path mapping.
4. `compute/architecture/yb-pageserver.md` — the row-per-page YugabyteDB
   storage design that REPLACES upstream Neon's layer-file path:
   image-anchored reads, background image materialiser, what's gone vs
   upstream, implementation phases. The upstream source is gone (lane H,
   2026-06-01) and its standalone design doc with it; the inherited
   concepts (Key encoding, walredo, getpage RPC, generation handshake)
   trace back to upstream Neon's `pageserver/`. Sharding was removed
   entirely — yb-amp is single-shard.
5. `compute/architecture/safekeeper.md` — `ControlFileStorage::Yb`,
   `YbWalStorage`, the `WalReader` fallback chain, the single-safekeeper
   constraint in YB-WAL mode.
6. `controller/architecture/sc-integration.md` — cluster control: generation
   authority (attach-hook), the node/branch/endpoint registries, and the
   provisioning of the `yb_storage` DB (all its schemas) on controller startup.
   These live in the Go `controller/`; the compute daemons bind the schemas the
   controller provisions.

For per-service HTTP API and Prometheus metrics, see the per-service
`-apis.md` / `-metrics.md` references: the compute-engine services
(pageserver, safekeeper, compute_ctl, endpoint_storage) are under
`compute/architecture/` (indexed in `compute/architecture/README.md`); the
controller's are under `controller/architecture/`.

Code anchors:

- `compute/libs/remote_storage/src/yb_store.rs` — the YbStore impl, chunked
  uploads, streaming downloads, parse/reconstruct key.
- `compute/libs/yb_tsclient` — pure-Rust direct-to-tserver client
  (`hot_path = "tsclient"`, the default): speaks the pggate RPC protocol
  to a tserver's RPC port, no native library; see
  `compute/architecture/yugabyte.md` §"Direct-to-tserver hot path".
- `compute/libs/remote_storage/src/config.rs` — `YbStoreConfig` (host, port,
  user, `password_env`, `sslmode`, etc.).
- `compute/safekeeper/src/control_file.rs` — `ControlFileStorage` enum.
- `compute/safekeeper/src/yb_wal_storage.rs` — `YbWalStorage`, the YB WAL mirror.
- `compute/safekeeper/src/wal_storage.rs::WalBookkeeping` — in-memory WAL
  LSN/record-boundary bookkeeping.
- `controller/src/persistence.rs` — `establish_connection_rustls`,
  AcceptAll TLS verifier.
- `control_plane/src/controller.rs` — yb_amp_local wrapper:
  external-DB path, `ensure_external_database`, YB schema
  auto-provisioning.
- `control_plane/src/local_env.rs` — TOML config types
  (`YbAmpControllerConf::yb`, `SafekeeperConf::yb_storage_conn`;
  every service takes the same `yb_storage_conn` connection block).
- `control_plane/src/safekeeper.rs` / `pageserver.rs` / `endpoint.rs` —
  per-service CLI assembly + per-component config.
- `compute/pageserver/src/page_store.rs` — `PageStore` row store for the
  fork's row-per-page YB layout (`pageserver.pages` +
  `pageserver.timelines`).
- `compute/pageserver/src/image_rule.rs` — 10-delta image rule that bounds
  the read-chain length.

## Project conventions

- **Don't commit or push unless explicitly asked.** The user controls
  when commits land; do the work, show diffs, wait for "commit" /
  "push".
- **Editing existing files is preferred over creating new ones.** Don't
  spin up a parallel module for a small change.
- **No comments unless the *why* is non-obvious.** Identifiers should
  carry meaning. Don't reference the task/PR/issue in code comments —
  rot.
- **Architecture changes go in the matching `*/architecture/*.md` docs**
  (`compute/architecture/` for the storage/compute engine,
  `controller/architecture/` for the control plane). Code change without doc
  change usually means the doc is stale.
- **Work is tracked as GitHub issues**, not files. See
  [`docs/issue-tracking.md`](docs/issue-tracking.md) for the area
  labels, milestones, the create/close contract, and the commit-subject
  format.
- **Remote branches are `<alias>/<feature_name>`** (e.g.
  `hsunder/ingest-perf-hardening`). `<alias>` is your git/GitHub alias;
  `<feature_name>` is kebab-case. A leading GitHub issue number in the
  feature part (`hsunder/174-ingest-perf-hardening`) is optional but
  encouraged when the work maps to an issue. Enforced for Claude Code by a
  `PreToolUse` hook ([`.claude/hooks/check-branch-name.sh`](.claude/hooks/check-branch-name.sh),
  registered in [`.claude/settings.json`](.claude/settings.json)): it blocks
  any `git push` that would create a non-conforming branch (`main`/`master`
  exempt; deletions ignored). No per-clone setup needed — it runs from the
  committed project settings.
- **Every YB query must be idempotent.** `YsqlClient` retries
  connection drops internally and can replay an in-flight write whose
  commit status is unknown. New write paths must use `INSERT ... ON
  CONFLICT` (or `yb_enable_upsert_mode`), absolute `UPDATE`s (no `SET
  col = col + 1`), generation-fenced `UPDATE`s, or `WHERE`-keyed
  `DELETE`s. See
  [`compute/architecture/yugabyte.md`](compute/architecture/yugabyte.md#client-side-retry-contract--every-yb-query-must-be-idempotent).

## Build notes

- `./build.sh` is the single build entry point: no flag = debug, `--release`
  = release, `--release-tar` = release + shippable tarball, `--headers` =
  install just the PG headers (for `cargo check` / IDE). It checks out the
  `compute/vendor/postgres-v18` submodule and rebuilds PostgreSQL (and the compute
  extensions) whenever the installed PG no longer matches the checked-out
  submodule commit, so prefer it over a bare `make`/`cargo build`. The logic
  lives in [`scripts/build.py`](scripts/build.py); `build.sh` is a thin shim
  (`--pg-only` does just the PostgreSQL step, which `run.sh` reuses).
- Workspace builds with stable Rust. Some files use the `2024` edition.
- Protobuf-codegen needs **protoc ≥ 3.15** (proto3 `optional` keyword).
  On `dev-server-hsunder` the system `protoc` is 3.5.0; release builds
  use `PROTOC=/opt/yb-build/thirdparty/.../installed/uninstrumented/bin/protoc`
  (3.21+). Debug builds tend to work via cached proto output.
- `cargo build -p control_plane --bin yb_amp_local` is the fastest "did I
  break the wrapper" check.

## Testing

- Unit/workspace tests: `cargo test`. Several live-DB tests
  (`pageserver::page_store::tests`, `pageserver::page_writer::tests`,
  `safekeeper::yb_wal_storage::tests`, and the `yb_tsclient` live suite
  `compute/libs/yb_tsclient/tests/live.rs`) need a live YB with the production
  schema migrated — they default to a developer cluster on
  `5433`/`yb_storage`; without one, run them via
  `test/integration/compute/run.sh unit`, which points them at an isolated
  ephemeral cluster instead.
- End-to-end: `test/integration/compute/run.sh [unit|fast|slow|all|<name>]`
  (default: unit then fast) — a Rust harness
  (`integration_tests` crate) that brings up a real `yb_amp_local` + YB, runs
  scenario tests + golden `.sql` workloads, and scans the pageserver log
  for corruption markers after every test. `-n <count>` repeats for
  flakiness hunting; `--release` runs the release build. See
  [`test/integration/compute/README.md`](test/integration/compute/README.md) for how to run and how to
  add a test (just drop a file under `test/integration/compute/{fast,slow}/`
  or a `.sql`/`.out` pair under `test/integration/compute/{fast,slow}/`).
- Run integration before claiming any corruption-class fix — unit tests don't
  exercise the cross-seam bugs (enforced by the
  `integration-before-corruption-claim` skill).

## Live dev cluster cheat-sheet

(For when you're running locally on `dev-server-hsunder`. The
`/home/yugabyte/yb-amp-bundle/` instance on the AWS box has a separate
handoff doc.)

- AMP is a hosted service; for local work you run the whole stack yourself.
  Start a local single-node YB with `./scripts/yb-local.sh start` (same
  CI-pinned image), reachable at `127.0.0.1:5433` / db `yb_storage`.
- Topology spawns from `yb_amp_local init --config <file>` then
  `yb_amp_local start`. All durable state goes to YB; local FS only holds
  logs, pidfiles, and the compute's ephemeral `pgdata`.
- See [`compute/architecture/high-level.md`](compute/architecture/high-level.md) §
  "Topology in our current dev cluster" for ports / commands.

## Kubernetes deploy (ECR images + pull secret)

When asked about deploying amp on Kubernetes — the image repos or the pull secret —
the canonical details are in [`deploy/README.md`](deploy/README.md) ("One-shot
bring-up — `up.sh`" + "ECR access & the pull secret"). In short:

- **Images:** registry `454529406029.dkr.ecr.us-west-2.amazonaws.com`, repos
  **`amp/controller`** and **`amp/endpoint`**. Both `deploy/up.sh` (from nothing)
  and `deploy/controller/redeploy.sh` (controller-only) default to these.
- **ECR auth:** the **`dshubin_bot`** IAM bot account (static keys — they don't
  expire, unlike a personal SSO session). The deploy scripts resolve AWS creds via
  [`deploy/ecr-creds.sh`](deploy/ecr-creds.sh) in priority order: `AWS_ACCESS_KEY_ID`
  + `AWS_SECRET_ACCESS_KEY` in the env → else `AWS_BOT_JSON` (a JSON keys file, e.g.
  `aws iam create-access-key` output) → else the ambient config (`aws sso login`).
  Set `AWS_DEFAULT_REGION` too. For a local docker login:
  `aws ecr get-login-password --region $AWS_DEFAULT_REGION | docker login --username AWS --password-stdin 454529406029.dkr.ecr.us-west-2.amazonaws.com`.
- **Pull secret:** a namespace-scoped `pull-secret` minted from `dshubin_bot`. Its
  ECR token **expires ~12h**, so it must be **refreshed** (a stale one → the cluster
  can't pull): **both `up.sh` and `redeploy.sh` re-mint it each run**; for a namespace
  idle *between* deploys, re-run the idempotent `kubectl create secret docker-registry
  … | kubectl apply -f -` one-liner (or a CronJob) in `deploy/README.md` ("Refreshing
  the pull secret").
- To debug a deploy (endpoint won't come up, a task fails), see
  [`runbooks/controller_runbook.md`](runbooks/controller_runbook.md).

## Outstanding work / known sharp edges

- `yb_amp_local start` spawns SC + SK + PS via `JoinSet` with no ordering
  guarantee — SK can race SC and try to open `yb_storage` before SC
  creates it. Workaround: pre-provision YB once by hand; structural fix
  is to make `Controller::provision_external_database()` a
  synchronous pre-step before the `JoinSet` block.
- `endpoint create --pg-setting NAME=VALUE` is the escape hatch for
  arbitrary GUCs. Dedicated `--shared-buffers` and `--lfc-size` flags
  exist for the common cases.
- YbStore TLS support (`sslmode = "require"`) is wired but
  `ensure_external_database` in the control-plane wrapper still uses
  `NoTls` — so against a TLS-only YB you'd need to add a matching
  rustls branch there too.

## Memory hygiene

Use the Meko datapack above for repo-scoped memory. Don't store:

- Secrets, passwords, connection strings with credentials.
- Ephemeral state (what's in flight this session — use plans/tasks).
- Things derivable from `git log` / current code.
- **Resolved-bug narratives.** Once a fix has shipped, its root cause,
  the change, and the verification all live in the commit message and the
  closed GitHub issue. Don't re-record "issue #N: cause was X, fixed by Y,
  verified by Z" in Meko — that's git/issue-tracker territory, not memory.
  The only durable carve-out is a *recurring sharp edge* (see below): a
  trap the fix itself doesn't prevent and a future session would hit
  again. Store that one sentence, not the case history.

Do store:

- User-stated preferences ("don't commit unless I ask", "I want
  release builds for the bundle").
- Project facts that aren't in the code (deadlines, why a particular
  architectural decision was made).
- Recurring sharp edges that future sessions will rediscover the hard
  way.
