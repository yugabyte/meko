<p align="center">
  <img src="docs/yugabytedb-light-logo.jpg" alt="yugabyteDB Light" width="600">
</p>

# ybLight

**ybLight** is a serverless Postgres-compatible storage system that runs
PostgreSQL computes over **YugabyteDB** as the durable backing store.
Compute nodes are stateless and the storage tier is row-per-page tables
in YB — so the system inherits YugabyteDB's geo-distributed, multi-region,
multi-cloud, infinitely-scalable replication model directly underneath
every byte of every page.

ybLight started as a fork of [neondatabase/neon](https://github.com/neondatabase/neon)
but is no longer one in any meaningful sense. The layer-file storage
engine, the disk-backed page cache, the in-process LFC pretense of
"local cache", the secondary-tenant warm replicas, and most of the
S3 / GCS / Azure remote-storage machinery have all been removed. What
remains is the shape of the original control plane (`neon_local`,
`storage_controller`, `safekeeper`, `pageserver` binary layout, the
patched Postgres compute) wrapped around an entirely different storage
substrate. The "pageserver" is now a thin row-store gateway between
the Postgres compute and YugabyteDB.

## What's left of Neon, what's new

**Kept (the shell):**
- The compute / pageserver / safekeeper / storage_controller separation.
- The `neon_local` developer experience: `init`, `start`, `tenant create`,
  `endpoint create`, `endpoint start`, timelines as forks.
- The patched Postgres tree with the `neon` smgr extension wiring page
  reads and WAL writes through the gateway.
- The gRPC page_service protocol; the WAL streaming protocol to the
  safekeepers.

**Replaced wholesale:**
- **Page storage**: every block lives in a YugabyteDB row keyed by
  `(tenant, timeline, rel, fork, blknum, lsn, is_image)`. No layer
  files. No disk-backed page cache. Reads chain the LSN history out
  of YB rows + an in-RAM `UnifiedPages` buffer; an image materialiser
  task compacts long delta chains into walredo-applied images in the
  background.
- **Safekeeper**: **stateless, no consensus protocol**. Original Neon
  safekeepers ran a Paxos quorum of 3 (or more) nodes with local disk
  WAL + leader election to guarantee a single durable WAL stream. In
  ybLight every WAL record is `INSERT`ed into a YugabyteDB row at the
  moment it's received; durability is YB's Raft replication, not a
  per-safekeeper Paxos. The safekeeper itself holds no persistent
  state beyond its config — restart-from-scratch and it just resumes
  by reading the next LSN out of YB. You can run one safekeeper or
  ten; HA is provided by the YB cluster underneath either way.
- **Remote storage**: `YbStore` writes blobs (basebackup tar, initdb)
  into a YugabyteDB table — same connection model as the page store.
  S3 / GCS / Azure backends are deleted.
- **Concurrency model**: `YsqlClient` is a pooled, auto-reconnecting,
  prepared-statement-caching wrapper around `tokio_postgres`. Hot
  read paths run pool_size=4 (or more) so compute-side prefetches
  parallelise on YB.

**New properties inherited from YB:**
- **Multi-region resilience**: YB's Raft replication runs across
  zones / regions / clouds. ybLight's storage tier is wherever the YB
  cluster says it is.
- **Geo-distributed**: tablet placement is YB's job. The pageserver
  just talks YSQL to whatever endpoint resolves.
- **Multi-cloud**: the storage tier is a YB cluster, which can live
  on AWS, GCP, Azure, on-prem, or any mix. ybLight follows.
- **Infinite scale**: tablet count grows with data; YB shards on the
  PK hash of `(tenant, timeline, fork)`. Adding YB nodes adds
  pageserver-side read capacity automatically.

## What removed for real

- ~30K LOC of Neon's layer-file machinery, secondary tenants, disk-usage
  eviction, heatmap synchronisation, compaction loops, and remote-
  storage backends. See `architecture/yb-pageserver.md` Phase 4d for the
  ongoing cascade.
- The OS-level page_cache is dead in this fork — preallocated buffers
  that the hot path never touches. Stub-sized in current builds; full
  removal is gated on the storage_layer cascade.

## Architecture overview

A ybLight install is:

- **Compute nodes**: patched PostgreSQL, stateless, configured against a
  pageserver endpoint and a safekeeper set. The `neon` smgr extension
  routes every `smgr_read` to a gRPC `GetPage` and every WAL flush to
  the safekeepers.
- **Pageserver (`yb-pageserver`)**: the row-store gateway. Two YSQL
  connection pools per tenant:
  - `compute_page_store` (pool_size=4): serves compute reads, runs
    chain reconstruction over `pageserver.pages`.
  - `internal_page_store` (single conn): walreceiver WAL ingestion +
    image materialiser writes.
- **Safekeeper**: stateless WAL ingress. Receives the WAL stream from
  the compute, `INSERT`s each record into a YB row, returns ack the
  moment the row is durable. No local disk, no quorum, no leader
  election. Run one for low-load deployments, run several behind a
  load balancer for connection-spread on heavy workloads — but every
  instance is independent and interchangeable. HA comes from YB.
- **Storage controller**: tenant/timeline placement & generation
  management. Backs its own state in YB too.
- **YugabyteDB cluster**: holds `pageserver.pages`, `pageserver.slru_pages`,
  `pageserver.meta`, the safekeeper WAL rows, the storage_controller
  schema, and the blob table. This is the durable storage tier.

## AI assistants

AI coding agents working in this repo should read [`AGENTS.md`](AGENTS.md)
first for repo conventions, architecture pointers, and ongoing project
context. [`CLAUDE.md`](CLAUDE.md) just points there.

This repo has a Meko datapack associated with it for agent memory:

- **Name**: `yb-light`
- **ID**: `fd3b43a0-ebd6-4732-958b-fae9a50a0505`

## Quick start

ybLight is **not** a hosted service. To run it you bring your own
YugabyteDB cluster (any deployment — local single-node, YBA-managed
multi-region, YBM) and point a pageserver + safekeeper at it.

The bundled developer workflow uses `neon_local`:

```sh
make -j`nproc` -s             # build patched PG + Rust binaries
cargo neon init               # writes .neon repo dir against config.toml
cargo neon start              # storage_broker, safekeeper, pageserver
cargo neon tenant create --set-default
cargo neon endpoint create main
cargo neon endpoint start main
```

The pageserver expects to find a YB cluster reachable from the host;
configure host/port/user/dbname/sslmode in `config.toml` under the
`remote_storage` and `wal_yb_config` sections.

Once running, connect with any Postgres client:

```sh
psql -p 55432 -h 127.0.0.1 -U cloud_admin postgres
```

Branches still work the same way as Neon — they're cheap timeline
forks at a given LSN, served by the same pageserver out of the same
YB rows:

```sh
cargo neon timeline branch --branch-name migration_check
cargo neon endpoint create migration_check --branch-name migration_check
cargo neon endpoint start migration_check
```

## Building from source

### Linux deps

```sh
# Ubuntu / Debian
apt install build-essential libtool libreadline-dev zlib1g-dev flex bison \
  libseccomp-dev libssl-dev clang pkg-config libpq-dev cmake \
  postgresql-client protobuf-compiler libprotobuf-dev libcurl4-openssl-dev \
  openssl python3-poetry lsof libicu-dev

# Fedora
dnf install flex bison readline-devel zlib-devel openssl-devel \
  libseccomp-devel perl clang cmake postgresql postgresql-contrib \
  protobuf-compiler protobuf-devel libcurl-devel openssl poetry lsof \
  libicu-devel libpq-devel python3-devel libffi-devel
```

`protoc` ≥ 3.15 required.

### Rust

Use the toolchain pinned in [`rust-toolchain.toml`](./rust-toolchain.toml) —
`rustup` picks it up automatically.

### macOS

```sh
xcode-select --install
brew install protobuf openssl flex bison icu4c pkg-config m4
brew install libpq && brew link --force libpq
```

### Build

```sh
make -j`nproc` -s                     # debug
BUILD_TYPE=release make -j`nproc` -s  # release
```

Build artefacts land in `target/{debug,release}/`. The release bundle
deployed to a target host typically contains:

```
yb-light-bundle/
  bin/
    yb-pageserver
    safekeeper
    neon_local
    compute_ctl
    endpoint_storage
    storage_broker
    storage_controller
  pg_install/v17/         # patched PG + libssl + libcrypto + libicu
  config.toml             # per-deploy YB connection settings
  start.sh / stop.sh
```

`bin/yb-pageserver` is the binary name `neon_local` looks for by
default — override with `NEON_PAGESERVER_BIN` if running against an
upstream Neon pageserver for A/B comparison.

## Tests

Rust unit tests:

```sh
cargo test --workspace
```

Integration tests (require a YB cluster reachable from the test runner):

```sh
CARGO_BUILD_FLAGS="--features=testing" make
DEFAULT_PG_VERSION=17 BUILD_TYPE=release ./scripts/pytest
```

Many of the legacy Neon unit tests have been deleted along with the
layer-file machinery they exercised — they tested code paths that no
longer execute in ybLight. The remaining suite focuses on the
row-store gateway, the YB-side write batching, the chain
reconstruction, and the materialiser.

## Flamegraphs

[`flamegraph-rs`](https://github.com/flamegraph-rs/flamegraph) works
out of the box.

> [!IMPORTANT]
> If you're linking with `lld` or `mold`, pass `--no-rosegment` —
> general Rust+alt-linker thing, not specific to this repo.

## Cleanup

```sh
make clean      # remove build artefacts
make distclean  # everything from build + configure
rm -rf .neon    # local dev cluster state (destroys local data)
```

## Documentation

- `architecture/yb-pageserver.md` — the row-store gateway design,
  phase plan for the Neon-shell deletion cascade, materialiser
  semantics, write batching shape.
- `architecture/yb-safekeeper.md` — WAL-in-YB layout.
- `architecture/yb-store.md` — the blob layer.
- [`docs/sourcetree.md`](/docs/sourcetree.md) — original Neon
  sourcetree map (still mostly accurate for the shell).

`cargo doc --no-deps --open` for rustdoc.

## Contributing

- Read [`AGENTS.md`](AGENTS.md) — applies to humans too.
- Read [`CONTRIBUTING.md`](/CONTRIBUTING.md) for style and process.
- Don't restore deleted Neon code without a concrete reason; if you
  need a removed feature, surface the requirement instead of resurrecting
  the implementation.
