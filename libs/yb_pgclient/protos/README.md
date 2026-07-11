# Vendored YugabyteDB protobuf definitions

These are the YB `.proto` files `yb_pgclient` compiles (via `build.rs`) to
talk to the tserver's `PgClientService`. They are vendored — checked in — so
the crate builds hermetically, with no YB source tree, toolchain, or network.

**Do not hand-edit or hand-add files here.** Re-sync with
`scripts/vendor-yb-protos.py <yb-src> <version>`, which copies the transitive
import closure of the root protos, re-applies the local edit below, and
records the pin in `version.txt`. The currently vendored version + upstream
sha are in [`version.txt`](version.txt).

Root files (the closure starts here; keep in sync with `build.rs`):
- `yb/tserver/pg_client.proto` — the `yb.tserver.PgClientService` service
- `yb/rpc/rpc_header.proto` — YB RPC wire framing

`google/*` well-known types are not vendored — protoc (protoc-bin-vendored,
used by `build.rs`) supplies them from its bundled include path.

Local edit vs. upstream: `[ default = DEFAULT_TABLE_TYPE ]` is rewritten to
`[ default = YQL_TABLE_TYPE ]` (the canonical name for the same value, 2) in
`master_ddl.proto`, `catalog_entity_info.proto`, and `tablet/metadata.proto`
— prost-build does not emit `allow_alias` variants, so the generated default
accessor would otherwise reference a nonexistent `TableType::DefaultTableType`.
The re-vendor script re-applies this automatically.
