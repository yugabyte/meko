# Agent guide — yb-neon

This is **yb-neon**: a fork of [Neon](https://github.com/neondatabase/neon)
that points every durable byte at **YugabyteDB** instead of S3/Azure/GCS
and instead of the storage controller's local Postgres. If you're new to
the codebase, read `architecture/high-level.md` first, then the
subsystem doc that matches whatever you're touching.

## Meko datapack

This repo has a Meko datapack for agent memory:

- **Name**: `Neon`
- **ID**: `2536022f-3713-481a-8cf2-222c0b1b027a`

Use it when storing repo-scoped memories.

## Architecture map

Read in this order:

1. `architecture/high-level.md` — component diagram, what lives where, the
   topology of a dev cluster.
2. `architecture/yugabyte.md` — the two YB databases (`yb_storage`,
   `storage_controller`), schemas of `blobs` and `wals`, the
   `blob_namespace` ENUM, the `rpc_max_message_size` chunking story.
3. `architecture/remote-storage.md` — `YbStore` design, `parse_key` /
   `reconstruct_key`, the round-trip-lossless path mapping.
4. `architecture/pageserver.md` — layer-file lifecycle, LSN MVCC, image vs
   delta, compaction, the page-7 worked example.
5. `architecture/safekeeper.md` — `ControlFileStorage::Yb`,
   `YbWalStorage`, the `WalReader` fallback chain, the single-safekeeper
   constraint in YB-WAL mode.
6. `architecture/storage-controller.md` — `database_url` → external
   Postgres (YB) flow, the auto-provisioning of `storage_controller` +
   `yb_storage` DBs on first start.

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
- `control_plane/src/storage_controller.rs` — neon_local wrapper:
  external-DB path, `ensure_external_database`, YB schema
  auto-provisioning.
- `control_plane/src/local_env.rs` — TOML config types
  (`NeonStorageControllerConf::database_url`,
  `SafekeeperConf::control_file_backend` / `wal_yb_config`).
- `control_plane/src/safekeeper.rs` / `pageserver.rs` / `endpoint.rs` —
  per-service CLI assembly + per-component config.

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

## Build notes

- Workspace builds with stable Rust. Some files use the `2024` edition.
- Protobuf-codegen needs **protoc ≥ 3.15** (proto3 `optional` keyword).
  On `dev-server-hsunder` the system `protoc` is 3.5.0; release builds
  use `PROTOC=/opt/yb-build/thirdparty/.../installed/uninstrumented/bin/protoc`
  (3.21+). Debug builds tend to work via cached proto output.
- `cargo build -p control_plane --bin neon_local` is the fastest "did I
  break the wrapper" check.

## Live dev cluster cheat-sheet

(For when you're running locally on `dev-server-hsunder`. The
`/home/yugabyte/yb-neon-bundle/` instance on the AWS box has a separate
handoff doc.)

- The dev YB lives at `127.0.0.1:5433` (`yugabyted` under
  `/tmp/yb-bench/`).
- Topology spawns from `neon_local init --config <file>` then
  `neon_local start`. All durable state goes to YB; local FS only holds
  logs, pidfiles, and the compute's ephemeral `pgdata`.
- See [`architecture/high-level.md`](architecture/high-level.md) §
  "Topology in our current dev cluster" for ports / commands.

## Outstanding work / known sharp edges

- `neon_local start` spawns SC + SK + PS via `JoinSet` with no ordering
  guarantee — SK can race SC and try to open `yb_storage` before SC
  creates it. Workaround: pre-provision YB once by hand; structural fix
  is to make `StorageController::provision_external_database()` a
  synchronous pre-step before the `JoinSet` block.
- The bundle build doesn't yet ship `libssl.so.1.1` / `libcrypto.so.1.1`
  / `libicu*.so.60` alongside `pg_install/v17/lib/`, and `neon_local`
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
