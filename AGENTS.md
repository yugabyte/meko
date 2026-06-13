# Agent guide — yb-light

This is **yb-light**: a fork of [Neon](https://github.com/neondatabase/neon)
that points every durable byte at **YugabyteDB** instead of S3/Azure/GCS
and instead of the storage controller's local Postgres. If you're new to
the codebase, read `architecture/high-level.md` first, then the
subsystem doc that matches whatever you're touching.

## Meko datapack

This repo has a Meko datapack for agent memory:

- **Name**: `yb-light`
- **ID**: `fd3b43a0-ebd6-4732-958b-fae9a50a0505`

Use it when storing repo-scoped memories.

## Architecture map

Read in this order:

1. `architecture/high-level.md` — component diagram, what lives where, the
   topology of a dev cluster.
2. `architecture/yugabyte.md` — the single YB database (`yb_storage`)
   and its schemas (`public`, `pageserver`, `safekeeper`,
   `storage_controller`), the `blobs` and `wals` tables, the
   `blob_namespace` ENUM, the `rpc_max_message_size` chunking story.
3. `architecture/remote-storage.md` — `YbStore` design, `parse_key` /
   `reconstruct_key`, the round-trip-lossless path mapping.
4. `architecture/pageserver.md` — original upstream design: layer-file
   lifecycle, LSN MVCC, image vs delta, compaction, the page-7 worked
   example. The upstream source is gone (lane H, 2026-06-01); this doc
   stays because the inherited concepts (Key encoding, walredo, getpage
   RPC, sharding, generation handshake) are still load-bearing.
5. `architecture/yb-pageserver.md` — the row-per-page YugabyteDB
   storage design that REPLACES the layer-file path: image-anchored
   reads, background image materialiser, what's gone vs upstream,
   implementation phases.
6. `architecture/safekeeper.md` — `ControlFileStorage::Yb`,
   `YbWalStorage`, the `WalReader` fallback chain, the single-safekeeper
   constraint in YB-WAL mode.
7. `architecture/storage-controller.md` — `database_url` → external
   Postgres (YB) flow, the auto-provisioning of the `yb_storage` DB
   (and its `storage_controller` schema) on first start.

Code anchors:

- `libs/remote_storage/src/yb_store.rs` — the YbStore impl, chunked
  uploads, streaming downloads, parse/reconstruct key.
- `libs/remote_storage/src/config.rs` — `YbStoreConfig` (host, port,
  user, `password_env`, `sslmode`, etc.).
- `safekeeper/src/control_file.rs` — `ControlFileStorage` enum.
- `safekeeper/src/yb_wal_storage.rs` — `YbWalStorage` + `WalStore`.
- `safekeeper/src/wal_storage.rs::PhysicalStorage` — `no_local_writes`
  gating.
- `storage_controller/src/persistence.rs` — `establish_connection_rustls`,
  AcceptAll TLS verifier.
- `control_plane/src/storage_controller.rs` — yb_light_local wrapper:
  external-DB path, `ensure_external_database`, YB schema
  auto-provisioning.
- `control_plane/src/local_env.rs` — TOML config types
  (`YbLightStorageControllerConf::database_url`,
  `SafekeeperConf::control_file_backend` / `wal_yb_config`).
- `control_plane/src/safekeeper.rs` / `pageserver.rs` / `endpoint.rs` —
  per-service CLI assembly + per-component config.
- `pageserver/src/page_store.rs` — `PageStore` row store for the
  fork's row-per-page YB layout (`pageserver.pages` +
  `pageserver.timelines`).
- `pageserver/src/image_rule.rs` — 10-delta image rule that bounds
  the read-chain length.

## Project conventions

- **Don't commit or push unless explicitly asked.** The user controls
  when commits land; do the work, show diffs, wait for "commit" /
  "push".
- **Match Neon upstream conventions** for code style and module layout.
  Don't reformat or rename unrelated things while making a change.
- **Editing existing files is preferred over creating new ones.** Don't
  spin up a parallel module for a small change.
- **No comments unless the *why* is non-obvious.** Identifiers should
  carry meaning. Don't reference the task/PR/issue in code comments —
  rot.
- **Architecture changes go in `architecture/*.md`.** Code change without
  doc change usually means the doc is stale.
- **Work is tracked as GitHub issues**, not files. See
  [`docs/issue-tracking.md`](docs/issue-tracking.md) for the area
  labels, milestones, the create/close contract, and the commit-subject
  format.
- **Every YB query must be idempotent.** `YsqlClient` retries
  connection drops internally and can replay an in-flight write whose
  commit status is unknown. New write paths must use `INSERT ... ON
  CONFLICT` (or `yb_enable_upsert_mode`), absolute `UPDATE`s (no `SET
  col = col + 1`), generation-fenced `UPDATE`s, or `WHERE`-keyed
  `DELETE`s. See
  [`architecture/yugabyte.md`](architecture/yugabyte.md#client-side-retry-contract--every-yb-query-must-be-idempotent).

## Build notes

- Workspace builds with stable Rust. Some files use the `2024` edition.
- Protobuf-codegen needs **protoc ≥ 3.15** (proto3 `optional` keyword).
  On `dev-server-hsunder` the system `protoc` is 3.5.0; release builds
  use `PROTOC=/opt/yb-build/thirdparty/.../installed/uninstrumented/bin/protoc`
  (3.21+). Debug builds tend to work via cached proto output.
- `cargo build -p control_plane --bin yb_light_local` is the fastest "did I
  break the wrapper" check.

## Testing

- Unit/workspace tests: `cargo test`.
- End-to-end: `test/e2e/run.sh [fast|slow|all|<name>]` — a Rust harness
  (`e2e_tests` crate) that brings up a real `yb_light_local` + YB, runs
  scenario tests + golden `.sql` workloads, and scans the pageserver log
  for corruption markers after every test. `-n <count>` repeats for
  flakiness hunting; `--release` runs the release build. See
  [`test/e2e/README.md`](test/e2e/README.md) for how to run and how to
  add a test (just drop a file under `test/e2e/src/scenarios/{fast,slow}/`
  or a `.sql`/`.out` pair under `test/e2e/{fast,slow}/`).
- Run e2e before claiming any corruption-class fix — unit tests don't
  exercise the cross-seam bugs (enforced by the
  `e2e-before-corruption-claim` skill).

## Live dev cluster cheat-sheet

(For when you're running locally on `dev-server-hsunder`. The
`/home/yugabyte/yb-light-bundle/` instance on the AWS box has a separate
handoff doc.)

- The dev YB lives at `127.0.0.1:5433`.
- Topology spawns from `yb_light_local init --config <file>` then
  `yb_light_local start`. All durable state goes to YB; local FS only holds
  logs, pidfiles, and the compute's ephemeral `pgdata`.
- See [`architecture/high-level.md`](architecture/high-level.md) §
  "Topology in our current dev cluster" for ports / commands.

## Outstanding work / known sharp edges

- `yb_light_local start` spawns SC + SK + PS via `JoinSet` with no ordering
  guarantee — SK can race SC and try to open `yb_storage` before SC
  creates it. Workaround: pre-provision YB once by hand; structural fix
  is to make `StorageController::provision_external_database()` a
  synchronous pre-step before the `JoinSet` block.
- The bundle build doesn't yet ship `libssl.so.1.1` / `libcrypto.so.1.1`
  / `libicu*.so.60` alongside `pg_install/v17/lib/`, and `yb_light_local`
  strips `LD_LIBRARY_PATH` from child processes. Both are fixable; see
  the bundle handoff doc.
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

Do store:

- User-stated preferences ("don't commit unless I ask", "I want
  release builds for the bundle").
- Project facts that aren't in the code (deadlines, why a particular
  architectural decision was made).
- Recurring sharp edges that future sessions will rediscover the hard
  way.
