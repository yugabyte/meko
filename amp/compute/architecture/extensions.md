# Externalized PostgreSQL extensions (ImageVolumes)

This page describes how yb-amp ships PostgreSQL extensions **outside** the
endpoint image — each extension is built into its own tiny OCI image and mounted
into the endpoint pod as a Kubernetes **ImageVolume**, with PostgreSQL pointed at
it via `extension_control_path` / `dynamic_library_path` (both PG18 features).
`pgvector`, `pg_cron`, and `hexabridge` are delivered this way; the pipeline is
generic so adding another is a manifest row plus a catalog entry.

## Why not bake them into the endpoint image

The yb-amp endpoint image ([`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile))
ships **only** PG18 — including the in-tree contrib modules installed by
[`postgres.mk`](../../postgres.mk) (`pg_trgm`, `btree_gin`, …) — plus the
`yb_amp` extension. Everything else is externalized:

- **Licensing forces it.** GPL extensions (e.g. **PostGIS**) can't be baked into
  our image; they must be built and shipped independently, then downloaded and
  attached on demand by the customer. Externalized ImageVolumes are the delivery
  mechanism that makes this possible.
- **Partner / third-party extensions.** We want to support extensions we don't
  own or build ourselves (e.g. **hexabridge**, HexaCluster's Oracle-compat layer,
  now shipped this way), each on its own release track — an independent image per
  extension is the natural unit. When the source is **closed and in a private
  repo** (hexabridge), the build fetches it with a GitHub token passed as a
  BuildKit secret (see "Authenticated source fetch", below).
- **Small, independent images.** One extension = one `FROM scratch` image
  containing just its `share/` (control + SQL) and `lib/` (the `.so`). It is
  versioned and pushed on its own.
- **Mount, don't rebuild.** Adding an extension to an endpoint is a volume mount
  + two GUCs, not an endpoint-image rebuild.
- **ABI safety by construction.** Every extension is compiled against the *exact*
  forked PostgreSQL v18 the endpoint runs (see "The ABI-anchor base", below).

## The two GUCs

PostgreSQL can locate extensions outside its own install tree:

- `extension_control_path` — where the server looks for `*.control` (and the
  SQL files next to them). We prepend the built-in default token `$system`, then
  append one entry per mounted extension.
- `dynamic_library_path` — where `LOAD` / `CREATE EXTENSION` finds the `.so`. We
  prepend `$libdir`, then append one entry per mounted extension.

An extension mounted at `/extensions/<name>` contributes
`/extensions/<name>/share/postgresql` to the first and
`/extensions/<name>/lib/postgresql` to the second. Note `extension_control_path`
entries are *sharedir bases*: PG18 appends `/extension` to each non-`$system`
entry itself (`get_extension_control_directories`), so the entry is the
`share/postgresql` dir, not `share/postgresql/extension` (the control files
still physically live in the latter). `CREATE EXTENSION <sqlname>` then resolves
normally — the control-path directory name (`<name>`) need not equal the SQL
name (`pgvector` ships `vector.control`, so `CREATE EXTENSION vector`).

`shared_preload_libraries` is separate: an extension that must be preloaded
(e.g. `pg_cron`) has its library name appended to the base
`shared_preload_libraries` (`yb_amp,pg_stat_statements`, both image-baked). It
must contain **only** libraries that are actually mounted, or the postmaster fails
to start — so the controller/chart only add a preload entry for an extension it is
also mounting.

`pg_cron`'s scheduler binds to one database via `cron.database_name` (default
`postgres`, which every endpoint serves, so opting in needs no extra config); a
non-default database is set through the endpoint's `pg_settings`.

## Build pipeline (`deploy/extensions/`)

```
                 repo (compute/vendor/postgres-v18, compute/image/patches/)
                                     |
             pg18-build.Dockerfile   |  build the forked PG18 once,
             (amp-pg18-build)        |  keep headers + pg_config + toolchain
                                     v
     +-----------------------------------------------------+
     |  extension.Dockerfile  (FROM amp-pg18-build)        |  per extension:
     |   fetch tarball -> checksum -> PREP -> patch ->     |  make + make install
     |   make install DESTDIR=/stage -> copy share/+lib/   |
     |                     |                               |
     |                     v  FROM scratch                 |
     |     /share/postgresql/extension  +  /lib/postgresql |  <- the ImageVolume tree
     +-----------------------------------------------------+
                                     |
                          build.sh (reads extensions.yaml)
                                     v
                   quay.io/yugabyte/amp-ext-<name>:<version>
```

`<version>` is the **extension's own upstream version**, not the amp/compute
version — e.g. `amp-ext-pgvector:0.8.2`. It comes straight from the extension's
row in `extensions.yaml` (and must match the controller catalog entry), so each
extension is tagged and released on its own cadence.

- **[`pg18-build.Dockerfile`](../../deploy/extensions/pg18-build.Dockerfile)** —
  the **ABI-anchor base**. It builds `compute/vendor/postgres-v18` with the same
  inputs as the endpoint image's `pg-build` stage and *keeps* the build toolchain
  + PG headers + `pg_config` in the image. Extensions build `FROM` it, so they
  link against byte-for-byte the same PostgreSQL the endpoint runs. Because the
  fork's `storage/smgr.h` `#define`s `YB_AMP_SMGR`, the unlogged-build hooks in
  patches like `pgvector-pg18.patch` light up automatically — no extra compiler flag.
  It also carries the system `-dev` headers some extensions link against
  (`libcurl4-openssl-dev`, `libxml2-dev`, `libssl-dev`, `zlib1g-dev` — hexabridge's
  `utl_*`/`dbms_*` deps); extensions build as an unprivileged user and can't
  install packages, so the headers must be present here. Build-time only — this
  image is never shipped to a pod.
- **[`extension.Dockerfile`](../../deploy/extensions/extension.Dockerfile)** —
  the **generic** recipe for pure-C / PGXS extensions. Build args:
  `NAME`, `VERSION`, `URL`, `SHA256`, `PATCH` (a file under
  [`compute/image/patches/`](../image/patches/)), `PREP` (a shell snippet run in
  the source dir before the patch — e.g. fetch an extra per-version SQL file),
  and `TRUSTED`. It fetches + checksum-verifies the tarball, patches, builds,
  `make install DESTDIR=/stage`, and repackages just `share/postgresql/extension`
  + `lib/postgresql` into a `FROM scratch` image. The fetch takes an optional
  `gh_token` **BuildKit secret** (`--mount=type=secret,id=gh_token,required=false`):
  when present it is sent as an `Authorization: Bearer` header so a **private**
  source (hexabridge) can be pulled; public extensions build with no secret. The
  token is a secret mount, never a build-arg, so it never lands in a layer, and
  the checksum still gates the result (a missing/bad token fails closed).
  **Complex extensions** (Rust/pgrx, heavy system deps) get their own Dockerfile
  referenced from the manifest's `dockerfile:` field instead of this generic one —
  the common path stays small.
- **[`extensions.yaml`](../../deploy/extensions/extensions.yaml)** — the build
  **manifest**: one row per extension (version, URL, checksums, patch, preload,
  prep, optional dedicated dockerfile, optional `regress:` block for the upstream
  suite). The single source of truth for versions and checksums; keep it in
  lockstep with the controller catalog (below).
- **[`build.sh`](../../deploy/extensions/build.sh)** — builds the base once, then
  each extension, and optionally `--push`es them. `--push` publishes the base too
  (for reuse); `--no-base-push` keeps the base local — nothing pulls it back, so
  publishing it is optional — and pushes only the extension images (what the GKE
  deploy uses). The base is the only expensive build (~30 min cold); `--cache`
  gives it a buildx registry layer cache (`<registry>/<base_image>:buildcache`,
  the same pattern as the endpoint image) so unchanged inputs restore in minutes.
  Only the `:buildcache` tag is pushed — never the base image — so it needs a
  buildx docker-container builder and a registry login; it is a CI opt-in (the
  GKE `amp_deploy_gke_extensions` step passes it), not for a plain local build.
  Extension image tags are immutable (version-pinned), so an extension whose tag
  already exists (checked in the registry with `--push`, else the local store) is
  **skipped**; pass `--force` to rebuild. `AMP_EXT_GH_TOKEN` in the environment is
  forwarded to each build as the `gh_token` secret above, so a private-source
  extension (hexabridge) can fetch — CI sets it from the `HEXABRIDGE_GH_PULL`
  secret; it is empty on forks, so the hexabridge steps skip there.

```sh
# build everything locally
deploy/extensions/build.sh
# build + push just pgvector to the default registry
deploy/extensions/build.sh --push pgvector
# build + push only the extension image(s), not the ABI-anchor base
deploy/extensions/build.sh --push --no-base-push pgvector
# CI: same as above, but layer-cache the base build (buildx + registry login)
deploy/extensions/build.sh --push --no-base-push --cache pgvector
```

## Endpoint wiring (Helm chart)

The [`yb-amp-endpoint`](../../deploy/helm/yb-amp-endpoint) chart takes an
`endpoint.extensions` list — each entry `{ name, image, preload }`:

- **[`deployment.yaml`](../../deploy/helm/yb-amp-endpoint/templates/deployment.yaml)**
  renders one read-only ImageVolume per entry, mounted at `/extensions/<name>`.
  (ImageVolume is a Kubernetes 1.33+ beta feature — the `ImageVolume` feature
  gate.)
- **[`configmap.yaml`](../../deploy/helm/yb-amp-endpoint/templates/configmap.yaml)**
  derives `extension_control_path` / `dynamic_library_path` (prepended with
  `$system` / `$libdir`) and appends any `preload` entries to
  `SHARED_PRELOAD_LIBRARIES`. The path GUCs are merged into
  `COMPUTE_EXTRA_SETTINGS_JSON`, which the supervisor renders into the compute's
  `postgresql.conf`.

With `endpoint.extensions` empty, the chart output is unchanged from before —
the feature is additive.

## Controller wiring

- **Catalog** —
  [`controller/internal/extensions`](../../controller/internal/extensions/catalog.go)
  is the fixed, code-defined set of known extensions (`Name`, `Version`,
  `Preload`, `Default`). `Resolve(names)` maps names to pinned image references
  (`<registry>/amp-ext-<name>:<version>`, registry from
  `--extension-registry` / `AMP_EXTENSION_REGISTRY`), skipping unknown names so a
  stale entry can't wedge a deploy.   `DefaultNames()` is the always-mounted set
  (currently **empty** — All extensions are registered but off by
  default, so nothing is auto-mounted; opting in is a **branch** property,
  requested via `createEndpoint` and persisted onto the branch, so it is
  inherited by forks and picked up by every endpoint on that branch). `pg_cron`
  is `Preload: true`, so `Resolve` carries that flag through and the chart adds
  it to `shared_preload_libraries`. `Has(name)` is the API validation gate. Keep
  versions in lockstep with `extensions.yaml`.
- **Endpoint spec** — the create_endpoint step
  ([`step_create_endpoint.go`](../../controller/internal/task/step_create_endpoint.go))
  resolves the endpoint's extension set (see fork inheritance below), sets
  `deploy.EndpointSpec.Extensions`, and the Kubernetes deployer
  ([`kubernetes.go`](../../controller/internal/deploy/kubernetes.go)) renders it
  into `endpoint.extensions`. The Docker deployer has no ImageVolume equivalent
  and ignores it (a dev endpoint ships only image-baked extensions).

## Default vs. on-demand, and restarts

- **Default extensions** (`Default: true`) would be mounted on every endpoint out
  of the box — the "batteries included" set. **There are none today**: pgvector
  is registered but `Default: false`, so it is opt-in.
- **On-demand extensions** are opted into at endpoint create via the API (see
  "Opt-in API" below), which persists the choice onto the branch. Because the
  volume set is fixed at pod start, **adding or removing an extension requires
  recreating the endpoint pod** (a rolling restart). A `preload` extension needs
  the postmaster restart regardless. Non-preload extensions are otherwise
  ready to `CREATE EXTENSION` as soon as the pod is up with the volume mounted.
  The ImageVolume carries only the extension **tree** (control + SQL + `.so`), not
  its system libraries, so the **endpoint runtime image**
  ([`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile)) must carry
  any shared library an externalized `.so` links — e.g. `libxml2` for hexabridge
  (`libcurl4`/`openssl`/`zlib1g` were already present). An extension that links a
  **Postgres core** feature gated behind a build flag needs the *server* built
  with it, not just the runtime lib: hexabridge uses PG's XML machinery
  (`pg_xml_init_library`), so the forked Postgres is configured `--with-libxml`
  (`PG_CONFIGURE_OPTS` in [`Makefile`](../../Makefile)) — without it the `.so`
  fails to load with `undefined symbol: pg_xml_init_library`. An extension is also
  only as trusted as its `.control` says: hexabridge ships no `trusted`, so
  `CREATE EXTENSION hexabridge` is **superuser-only**.

## Opt-in API

`createEndpoint` accepts an optional `extensions: [name, ...]` array
([`openapi_spec.yaml`](../../controller/internal/api/openapi_spec.yaml)):

- The handler
  ([`handlers.go`](../../controller/internal/api/handlers.go)) validates every
  name against the catalog (`Has`) — an unknown name is a `400`, not a silent
  drop — and rejects the request with `400` if the controller has no catalog
  (the Docker driver / feature off), so a caller never gets a silently-ignored
  opt-in.
- The create_endpoint step **persists** the requested names onto the branch's
  `extensions` set (`AddBranchExtensions`, an idempotent absolute union) before
  resolving mounts, so the opt-in is durable: a recreate resolves the same set
  and a fork inherits it (below). The endpoint then mounts
  **catalog defaults ∪ the branch's set**.
- The opted-in set (not the catalog defaults) is **readable** on the `Branch`
  response — `getBranch`/`listBranches` return `extensions`. The opt-in stays
  write-only through `createEndpoint`; there is no branch-level mutate API.

## Branch fork inheritance

Extension **data** (the catalog objects, index contents) is part of a branch's
storage and is inherited by a fork automatically. The extension **files** are
not — they are mounted from images — so a fork must mount (at least) the same
extensions its parent had, or inherited objects become inoperable and a
`preload` extension's missing library crashes the postmaster.

yb-amp makes the extension set **branch-level durable state** and inherits it on
fork. It lives on the shared storage-branch record, `amp_registry.branches` —
the one row every branch-creation path (the controller *and* `yb_amp_local`)
writes and reads — so a fork **and** a new read-replica endpoint both pick it up:

- `amp_registry.branches` has an `extensions jsonb` column
  ([migration 0003](../../controller/migrations/0003_registry.up.sql)) holding
  the branch's *opted-in* extensions (beyond the always-mounted catalog
  defaults), keyed by the branch's `branch_id` uuid.
- On fork, `RecordBranch`
  ([`registry/store.go`](../../controller/internal/registry/store.go)) seeds the
  child's row with a snapshot of the ancestor's `extensions` (a scalar subquery
  on the ancestor id; empty for a bootstrap branch). The create_endpoint opt-in
  (`AddBranchExtensions`,
  [`core/provision.go`](../../controller/internal/core/provision.go)) then
  unions any new names into the same row.
- At endpoint create, the step resolves the mount set as
  **catalog defaults ∪ the branch's opted-in set**, so a child always mounts a
  superset of what its parent had installed. Unknown names are dropped by the
  catalog.

The resolved set is also recorded in the endpoint's recreate `config` blob for
audit / recreate.

## Adding a new extension

1. Add a row to
   [`deploy/extensions/extensions.yaml`](../../deploy/extensions/extensions.yaml)
   (version, URL, sha256, optional patch/prep/preload/dedicated dockerfile) and
   `deploy/extensions/build.sh --push <name>`. If the source is private, export
   `AMP_EXT_GH_TOKEN` first (build.sh forwards it as the `gh_token` secret). If
   the `.so` links a new system library, add its `-dev` package to
   [`pg18-build.Dockerfile`](../../deploy/extensions/pg18-build.Dockerfile) (build)
   and the runtime package to
   [`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile) (load). If it
   links a Postgres **core** feature behind a configure flag (e.g. XML →
   `--with-libxml`), add that flag to `PG_CONFIGURE_OPTS` in the
   [`Makefile`](../../Makefile) and its `-dev` package to every PG build context
   (the endpoint `pg-build` stage, the ABI base, and the on-host CI build-dep
   lists in `.github/workflows/amp-*.yml`).
2. Add the matching entry to the controller catalog
   ([`catalog.go`](../../controller/internal/extensions/catalog.go)) with the
   same version, and `Default: true` only if it should be mounted on every
   endpoint (today all entries are `Default: false`). Set `Preload: true` if the
   extension registers a background worker or needs shared memory (its `_PG_init`
   requires `shared_preload_libraries`), keeping it in step with the manifest's
   `preload:`.
3. For a non-default extension, opt an endpoint in by passing
   `extensions: ["<name>"]` to `createEndpoint`; the step persists it onto the
   branch and the fork path inherits it.
4. (Optional but expected) add a `regress:` block to the manifest row so
   `extensions.sh --regress` runs the extension's upstream suite: `driver: pgxs`
   for a Makefile with a `REGRESS` target, or `driver: script` + `script: <path>`
   for a custom runner. Bless any by-design amp diffs under
   `test/integration/compute/ext_regress_expected/<name>/`.

## Testing

- **Controller unit tests** cover the catalog (`Has`/`Resolve`, preload flag
  threaded through), the create_endpoint step (opt-in mount + branch
  persistence + fork inheritance), and the API (`extensions[]` → 202/400, deploy
  spec, persistence) — see `controller/internal/extensions`,
  `controller/internal/task`, `controller/internal/api`.
- **Image build** — `deploy/extensions/test-build.sh [--docker] <name>` validates
  the manifest and, with `--docker`, builds the image and asserts its
  `control` / `--<version>.sql` / `.so` tree.
- **Local deploy (no Kubernetes)** —
  [`test/integration/compute/extensions.sh`](../../test/integration/compute/extensions.sh)
  `--ext <name>` builds the real image via the docker pipeline, extracts the tree,
  and mounts it into a throwaway `yb_amp_local` compute via the two path GUCs
  (appending a preload extension to `shared_preload_libraries`), then asserts the
  extension end-to-end. For `pg_cron` it goes past `CREATE EXTENSION` and
  schedules a sub-minute job, waiting for `cron.job_run_details` to report a
  succeeded run — so the background-worker scheduler is exercised, not just
  registration. For `hexabridge` it goes past `CREATE EXTENSION` (superuser-only)
  and calls deterministic Oracle-compat functions (`nanvl`, `utl_raw.length`) so
  the loaded `.so` is exercised; that run needs `AMP_EXT_GH_TOKEN` for the private
  fetch and is skipped on forks. It is the local-deploy twin of
  `deploy/smoke-test.sh`'s `SMOKE_EXTENSIONS` check.
- **Upstream regression suites (local)** —
  [`extensions.sh --regress`](../../test/integration/compute/extensions.sh) runs
  each extension's **own** upstream regression suite against the mounted
  ImageVolume — the extension analogue of `pg_regress.sh` for core Postgres (#608)
  — driven by the manifest row's `regress:` block: `driver: pgxs` runs
  `pg_regress` from the extension's Makefile `REGRESS` (pgvector, pg_cron) against
  the live compute; `driver: script` runs the extension's own runner (hexabridge's
  `scripts/regression.sh`). The suite lives in the source tree (not the
  `FROM scratch` image), so the driver re-fetches the source as
  `extension.Dockerfile` does. By-design amp diffs are blessed as alternative
  expected outputs under
  [`test/integration/compute/ext_regress_expected/<name>/`](../../test/integration/compute/ext_regress_expected/README.md),
  never by editing upstream goldens. The `amp_extensions_local` CI suite runs all
  three with `--regress`, and it fires automatically when a diff touches
  `deploy/extensions/`.
- **Smoke test** — `deploy/smoke-test.sh` with `SMOKE_EXTENSIONS=1` exercises the
  real K8s ImageVolume path for pgvector, pg_cron, and hexabridge end-to-end.

## Code anchors

- Build: [`deploy/extensions/`](../../deploy/extensions/) (`pg18-build.Dockerfile`,
  `extension.Dockerfile`, `extensions.yaml`, `build.sh`).
- Chart: [`deploy/helm/yb-amp-endpoint/`](../../deploy/helm/yb-amp-endpoint/)
  (`values.yaml` `endpoint.extensions`, `templates/deployment.yaml`,
  `templates/configmap.yaml`).
- Controller: [`controller/internal/extensions/catalog.go`](../../controller/internal/extensions/catalog.go),
  [`controller/internal/task/step_create_endpoint.go`](../../controller/internal/task/step_create_endpoint.go),
  [`controller/internal/deploy/kubernetes.go`](../../controller/internal/deploy/kubernetes.go),
  [`controller/internal/core/provision.go`](../../controller/internal/core/provision.go)
  (`BranchExtensions` / `AddBranchExtensions`),
  [`controller/internal/registry/store.go`](../../controller/internal/registry/store.go)
  (`RecordBranch` fork inheritance),
  [migration 0003](../../controller/migrations/0003_registry.up.sql)
  (`amp_registry.branches.extensions`).
- Patches: [`compute/image/patches/`](../image/patches/) (reused verbatim).
