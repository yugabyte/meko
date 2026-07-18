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

Start with [`architecture/high-level.md`](architecture/high-level.md) for the
system overview and data-flow diagram, then the per-component docs under
[`architecture/`](architecture/).
