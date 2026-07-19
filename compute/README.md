# Compute & Storage Engine

The storage / compute engine for yb-amp — the data plane. This is the patched
PostgreSQL compute, the pageserver and safekeeper, the `yb_amp` Postgres
extension, and their supporting Rust libraries. The control plane lives in
[`../controller/`](../controller/).

## Layout

| Path | What |
|---|---|
| [`pageserver/`](pageserver/) | `yb-pageserver` — serves pages from YugabyteDB; LSN-chain reconstruction + image materialiser |
| [`safekeeper/`](safekeeper/) | `yb-safekeeper` — stateless WAL ingress backed by YugabyteDB |
| [`compute_tools/`](compute_tools/) | `compute_ctl` — compute supervisor (config, start/stop, catalog migrations) |
| [`endpoint_storage/`](endpoint_storage/) | per-endpoint scratch-data service (`yb-endpoint-storage`) |
| [`pgxn/`](pgxn/) | Postgres extensions, including the `yb_amp` smgr extension |
| [`libs/`](libs/) | shared Rust crates (`pageserver_api`, `safekeeper_api`, `remote_storage`, …) |
| [`image/`](image/) | compute image build — patches and vm-image specs |
| [`vendor/`](vendor/) | vendored patched PostgreSQL (submodule) |
| [`architecture/`](architecture/) | design docs |
| [`old_neon_docs/`](old_neon_docs/) | archived upstream Neon docs, kept for reference |

## Docs

Start here:

- [`architecture/README.md`](architecture/README.md) — index page for the
  storage/compute engine architecture section; one-line summary of every
  component doc below.
- [`architecture/high-level.md`](architecture/high-level.md) — components,
  processes, ports, what-talks-to-what at a single page.

Per-component deep dives:

- [`architecture/compute.md`](architecture/compute.md) — the patched
  Postgres compute (unchanged in yb-amp); how the `yb_amp` smgr
  extension wires page reads and WAL writes through the gateway.
- [`architecture/yb-pageserver.md`](architecture/yb-pageserver.md) —
  the row-store gateway design: schema, chain reconstruction,
  `UnifiedPages` buffer, image materialiser semantics, and write batching.
- [`architecture/safekeeper.md`](architecture/safekeeper.md) — the WAL-in-YB
  reduction in AMP is described inline in
  [`architecture/yugabyte.md`](architecture/yugabyte.md) and in
  the safekeeper module docs.
- [`../controller/architecture/sc-integration.md`](../controller/architecture/sc-integration.md)
  — cluster control in the controller: generation authority (attach-hook), the
  node/branch/endpoint registries, and `yb_storage` provisioning.
- [`architecture/remote-storage.md`](architecture/remote-storage.md)
  — the `RemoteStorage` abstraction and the `YbStore` backend that
  replaced S3/GCS/Azure (blob/basebackup tar/initdb layer).
- [`architecture/yugabyte.md`](architecture/yugabyte.md) — what the
  YB cluster looks like from AMP's perspective: databases,
  tables, prefixes, idempotency contract, and the DDL the operator
  must provision.

Per-service HTTP API + Prometheus metrics reference (one `-apis.md` and
one `-metrics.md` per service): the compute-engine services (pageserver,
safekeeper, compute_ctl, endpoint_storage) are indexed in
[`architecture/README.md`](architecture/README.md) — e.g.
[`architecture/yb-pageserver-apis.md`](architecture/yb-pageserver-apis.md),
[`architecture/safekeeper-metrics.md`](architecture/safekeeper-metrics.md);
the controller's are under
[`../controller/architecture/`](../controller/architecture/).

`cargo doc --no-deps --open` for rustdoc.

## Flamegraphs

[`flamegraph-rs`](https://github.com/flamegraph-rs/flamegraph) works
out of the box.

> [!IMPORTANT]
> If you're linking with `lld` or `mold`, pass `--no-rosegment` —
> general Rust+alt-linker thing, not specific to this repo.

## Operator tools

Three compiled `pageserver` bins (`cargo build -p pageserver`) that read
the row-store tables and render them in a human-readable form. They share
one connection-args struct ([`ysql_client::YbConnArgs`](libs/ysql_client/src/lib.rs)),
so the flags are identical across all three and can't drift — and all work
against both a local no-TLS cluster (default) and a TLS-required one
(`--sslmode require`). `build.sh --release-tar` stages them into
`${BUNDLE}/tools/` so they ship to deploy hosts alongside the daemons.

- [`yb-dump-pages`](tools/src/bin/yb-dump-pages.rs) — dump or decode
  `pageserver.pages` rows for one (rel, fork) tuple. Renders the LSN as
  `H/L` hex and forknum as `main`/`fsm`/`vm`/`init`. Required hash columns:
  `--tenant --timeline --spc --db --rel --fork`. Range filters: `--blknum`
  (or `--blknum-from/--blknum-to`), `--lsn-from`/`--lsn-to` (PG `X/Y` hex).
  `--no-payload` drops the byte preview, `--limit 0` is unbounded.
  `--decode` switches to a single-block chain decoder: it walks the chain
  LSN-ascending (the order walredo applies it), prints each image's
  line-pointer `max_off` and each delta's WAL resource-manager + op + target
  `offnum`, and flags a GAP when a delta's `offnum` exceeds the running
  max+1 — the signature of a chain walredo can't fold and of a base image
  too small for the deltas above it (see issue #22).
- [`yb-dump-wals`](tools/src/bin/yb-dump-wals.rs) — dump `safekeeper.wals`
  rows (the safekeeper's WAL-in-YB chunks). Required hash columns:
  `--tenant --timeline`. Range filters: `--segno` (or
  `--segno-from/--segno-to`), `--chunk-offset` (or its `-from/-to`
  variants), `--lsn-from`/`--lsn-to` (mapped to segno+offset via
  `--wal-seg-size`, default 16 MiB). `--no-data` drops the byte preview.
  `--summary` swaps the row dump for a per-segno health view
  (`min/max LSN, total_bytes, gap_bytes`) — the fastest way to spot a WAL
  hole during an incident.
- [`yb-reset-consistent-lsn`](tools/src/bin/yb-reset-consistent-lsn.rs)
  — find (and optionally rewind) `pageserver.timeline_state.consistent_lsn`
  to a real WAL record boundary. Given `--tenant --timeline --target-lsn
  X/Y` it anchors on a fresh-record WAL page, replays the real WAL decoder
  over `safekeeper.wals`, and reports the largest record boundary `<= target`
  plus its matching `prev_record_lsn`. Defaults to a dry run; `--set`
  writes the pair back (`--expect-generation N` to generation-fence the
  write, `--null-prev` to let the walreceiver re-derive prev). Use it to
  rewind past a bad replay point — picking an arbitrary segment/page-aligned
  LSN instead makes PS loop forever on `YB WAL hole` or `invalid xlog page
  header: xlp_rem_len`.

Sample invocations (use the bundle's `tools/` copy on a deploy host, or
`cargo run --bin <tool> --` locally):

```sh
yb-dump-wals  --tenant <hex> --timeline <hex> --summary
yb-dump-wals  --tenant <hex> --timeline <hex> \
              --lsn-from 0/1545000 --lsn-to 0/15A0000 --limit 5
yb-dump-pages --tenant <hex> --timeline <hex> \
              --spc 1663 --db 5 --rel 16384 --fork 0 --blknum 0 --limit 3
# decode one block's chain and check it folds (offnum gaps = landmine)
yb-dump-pages --decode --tenant <hex> --timeline <hex> \
              --spc 1663 --db 16384 --rel 16414 --fork 0 --blknum 1456384
# dry run: print the max valid consistent_lsn <= target, then rewind to it
yb-reset-consistent-lsn --tenant <hex> --timeline <hex> --target-lsn 22/63000000
yb-reset-consistent-lsn --tenant <hex> --timeline <hex> --target-lsn 22/63000000 --set
# remote TLS cluster (e.g. AWS): add --sslmode (and --sslrootcert to verify)
yb-dump-wals  --host-ports <host:port> --sslmode require --password-env PGPASSWORD \
              --tenant <hex> --timeline <hex> --summary
```

All three default to a local `yb_amp_local` cluster (`127.0.0.1:5433`,
`yugabyte`/`yb_storage`, no TLS) and accept the shared
`--host-ports`/`--user`/`--db-name`/`--password-env`/`--sslmode`/`--sslrootcert`
overrides for remote clusters.

## Operational runbooks

[`../runbooks/`](../runbooks/) — incident-time playbooks for common failure
modes on a running AMP deployment. Each runbook is structured as
"symptom → quick diagnosis → fix"; read them in advance once so the
shape is familiar, then jump straight to the relevant one during an
incident.

- [`../runbooks/startup-failures.md`](../runbooks/startup-failures.md) —
  a component (PS / SK / compute / controller) won't start,
  won't register, or the compute can't serve queries.
- [`../runbooks/throughput-troubleshooting.md`](../runbooks/throughput-troubleshooting.md)
  — low throughput on TPC-C load / execute or any OLTP workload;
  isolating compute vs. PS vs. SK vs. YB as the bottleneck.
- [`../runbooks/perf-investigation.md`](../runbooks/perf-investigation.md) —
  broader performance investigation: memory growth, pageserver
  internals, WAL backlog, materialiser pacing.
