# Externalized PostgreSQL extensions (ImageVolumes)

This page describes how yb-amp ships PostgreSQL extensions **outside** the
endpoint image — each extension is built into its own tiny OCI image and mounted
into the endpoint pod as a Kubernetes **ImageVolume**, with PostgreSQL pointed at
it via `extension_control_path` / `dynamic_library_path` (both PG18 features).
`pgvector`, `pg_cron`, `hexabridge`, `hexabridge_mssql`, and `age` (Apache AGE)
are delivered this way; the pipeline is generic so adding another is a manifest row plus a catalog
entry. An image amp does **not** build reaches an endpoint by a different route:
an operator **registers** it through the API, and nothing in the product names
it — see "[Registering an extension image](#registering-an-extension-image)".

## Why not bake them into the endpoint image

The yb-amp endpoint image ([`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile))
ships **only** PG18, including the contrib modules installed by
[`postgres.mk`](../../postgres.mk) (`pg_trgm`, `btree_gin`, …), plus the
extensions built from [`compute/pgxn`](../pgxn), which are `yb_amp` and the
vendored `pgsentinel`. Everything else is externalized:

- **Licensing forces it.** GPL extensions (e.g. **PostGIS**) can't be baked into
  our image; they must be built and shipped independently, then downloaded and
  attached on demand by the customer. Externalized ImageVolumes are the delivery
  mechanism that makes this possible — and for an extension we do not build at
  all, registration is how the customer names the image.
- **Partner / third-party extensions.** We want to support extensions we don't
  own or build ourselves (e.g. **hexabridge**, HexaCluster's Oracle-compat layer,
  and **hexabridge_mssql**, its SQL Server sibling), each on its own release
  track — an independent image per extension is the natural unit. When the source
  is **closed and in a private repo** (both hexabridges), the build fetches it with a GitHub token passed as a
  BuildKit secret (see "Authenticated source fetch", below).
- **Small, independent images.** One extension = one `FROM scratch` image
  containing just its `share/` (control + SQL) and `lib/` (the `.so`). It is
  versioned and pushed on its own.
- **Mount, don't rebuild.** Adding an extension to an endpoint is a volume mount
  + two GUCs, not an endpoint-image rebuild.
- **ABI safety by construction.** Every extension is compiled against the *exact*
  forked PostgreSQL v18 the endpoint runs (see "The ABI-anchor base", below).

## What gets baked instead

The dividing line is **default-on**, not first-party-vs-third-party.
`shared_preload_libraries` is fixed at postmaster start, and a name in it whose
library is missing does not degrade: the postmaster refuses to boot. A library
every endpoint preloads by default therefore has to be in the image, because the
ImageVolume mount is per-branch opt-in and the endpoints that did not opt in
would be the ones that fail to start.

`pgsentinel` (active session history, 1.5.1) is baked for that reason
alone, even though it is third-party. It comes in as a submodule at
`compute/pgxn/vendor/pgsentinel` (`vendor/` holds third-party modules; the rest
of `compute/pgxn` is ours) from
[yugabyte/amp-pgsentinel](https://github.com/yugabyte/amp-pgsentinel), a fork of
upstream carrying the fixes amp needs, and an upgrade is a gitlink bump.
`yb-amp-pg-ext` installs it into `pg_install` beside `yb_amp`, and
[`spec_apply.rs`](../compute_tools/src/spec_apply.rs)'s `PRELOADED_EXTENSIONS`
has compute_ctl create the extension after start. The sampler reads only
`pg_stat_activity`, but the `queryid` it records is core's, which under the
default `compute_query_id = auto` is computed only because `pg_stat_statements`
is also preloaded. Without that, pgsentinel hashes the query text itself
and the join below does not match.

Upstream's GUC defaults are kept. `pgsentinel_ash.max_entries` (1000,
`PGC_POSTMASTER`) caps rows, not seconds: each 1 Hz tick writes one row per
active session, so the history reaches back about 1000/N seconds for N active
sessions, 16 minutes with one and under two with ten. Query text is not stored:
the fork's `pgsentinel_ash.yb_track_query_text` is off by default, so `query` and
`top_level_query` are NULL and `queryid` joins to `pg_stat_statements`
([#1448](https://github.com/yugabyte/meko/issues/1448)).

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
`shared_preload_libraries` (`yb_amp,pg_stat_statements,pgsentinel`, all
image-baked). It must contain **only** libraries that are actually present, or the
postmaster fails to start, so the controller/chart only add a preload entry for an
extension it is also mounting. For the same reason the base list is the endpoint
supervisor's `DEFAULT_SHARED_PRELOAD_LIBRARIES`, not a chart value: the controller
renders its one chart against endpoint images of any version, and only the image
knows which libraries it ships (#1572). The chart passes the preload entries in
`EXTRA_SHARED_PRELOAD_LIBRARIES`, which the supervisor appends to the base, and an
override in `BASE_SHARED_PRELOAD_LIBRARIES`. It also sets the whole list in
`SHARED_PRELOAD_LIBRARIES` — `yb_amp`, the one library every image ships, plus the
entries — because that is all a supervisor older than #1572 reads; a newer one
ignores it when either of the other two is set. So an image that old keeps its
preload extensions but runs without `pg_stat_statements` and `pgsentinel` until
it is upgraded.

`pg_cron`'s scheduler binds to one database via `cron.database_name`, and
`CREATE EXTENSION pg_cron` is refused in any other. It defaults to `postgres`,
which every endpoint serves, so opting in needs no extra config. **On a branch
that mounts documentdb it is `documentdb` instead, fixed by the catalog**
(documentdb's Settings, under "Engines"): the scheduler runs there, and pg_cron
cannot be created in `postgres`. A branch's own `pg_settings` still apply on
top, so overriding it there would break documentdb's `CASCADE`. Elsewhere a
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
  `NAME`, `VERSION`, `URL`, `PATCH` (a file under
  [`compute/image/patches/`](../image/patches/)), `PREP` (a shell snippet run in
  the source dir before the patch — e.g. fetch an extra per-version SQL file,
  through `sh "$SAFE_CURL" fetch <url> <dest>`), and `TRUSTED`. It fetches the
  tarball and verifies it against its pin in
  [`downloads_checksum.txt`](../../downloads_checksum.txt) (its header states
  the rules), patches, builds,
  `make install DESTDIR=/stage`, and repackages just `share/postgresql/extension`
  + `lib/postgresql` into a `FROM scratch` image. The fetch takes an optional
  `gh_token` **BuildKit secret** (`--mount=type=secret,id=gh_token,required=false`):
  when present it is sent as an `Authorization: Bearer` header so a **private**
  source (the hexabridges) can be pulled; public extensions build with no secret. The
  token is a secret mount, never a build-arg, so it never lands in a layer, and
  the checksum still gates the result (a missing/bad token fails closed).
  **Complex extensions** (Rust/pgrx, heavy system deps) get their own Dockerfile
  referenced from the manifest's `dockerfile:` field instead of this generic one —
  the common path stays small. A tree whose Makefile is not at the tarball root
  (hexabridge_mssql's is under `extension/`) is lifted by the row's `prep`, not
  by a build-dir knob here, because this Dockerfile is in every generic row's
  recipe hash. Lift it with a move, not a copy: a leftover copy is picked up by
  an `OBJS = $(wildcard */*.c)` and every symbol links twice.
- **[`extensions.yaml`](../../deploy/extensions/extensions.yaml)** — the build
  **manifest**: one row per extension (version, URL, patch, preload, prep,
  optional dedicated dockerfile, and a `regress:` block naming the upstream
  suite — required, see "Adding a new extension"). The single source of
  truth for versions; the checksum of each URL is in `downloads_checksum.txt`,
  and `test-build.sh` fails a row whose URL is not pinned there. Keep it in
  lockstep with the
  controller catalog (below).
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
  Extension image tags are immutable, so an extension whose tag already exists
  (checked in the registry with `--push`, else the local store) is **skipped**;
  pass `--force` to rebuild. The tag is the row's `version`, or
  `<version>-r<N>` from `image_revision: N` (N >= 2).

  Skipping on an existing tag is also how a recipe change got lost: documentdb's
  `0.116-0` was published from an early patch, the final one never reached the
  registry, and every deployed documentdb endpoint pulled the stale image. So
  each image is now **labelled** `org.yugabyte.amp.ext-recipe` with a hash of
  what it was built from: the row's build fields, its Dockerfile and its patch.
  The ABI-anchor base it builds `FROM` is deliberately not part of that hash:
  every vendored-PG bump would otherwise invalidate every labelled tag. So a base
  change that can affect the extensions built on it has to bump their
  `image_revision` by hand.
  An existing tag whose label differs **fails the build** and names the fix,
  which is to bump `image_revision` in the manifest and `ImageRevision` in the
  catalog, so the new recipe gets a new tag. `TestCatalogImageTagsMatchManifest`
  keeps those two in step. This holds for every tag built with the label,
  including a new extension's first. The tags published before the label (the
  revision-1 pgvector, pg_cron, hexabridge and age) are skipped with a warning,
  since their recipe cannot be checked: a recipe change to one of those is not
  caught unless it bumps that row's revision, which also brings it under the
  guard. A **local** build (no `--push`) rebuilds a stale or unlabelled image
  instead of refusing it, since a local tag is only a cache. `AMP_EXT_GH_TOKEN` in the environment is
  forwarded to each build as the `gh_token` secret above, so a private-source
  extension (either hexabridge) can fetch — CI sets it from the `HEXABRIDGE_GH_PULL`
  secret, which must be able to read both repos; it is empty on forks, so the
  hexabridge steps skip there.

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

## Registering an extension image

The built-in catalog covers what this repo builds. It deliberately covers
nothing else: pinning a third-party image reference in the product would mean
every new extension is a controller release, would put someone else's registry
in our source, and would make us the distributor of software we did not build.

So the catalog has two halves, and they meet in
[`extensions.Service`](../../controller/internal/extensions/service.go):

| | built-in | registered |
|---|---|---|
| lives in | `catalog.go`, compiled in | `amp_registry.extensions` (migration 0008) |
| image | derived: `<registry>/amp-ext-<name>:<version>` | the reference as registered |
| built by | `deploy/extensions` | someone else |
| added by | a controller release | `PUT /extensions/{name}` |

A branch opts in by name either way (`createCluster extensions:[…]`,
`addBranchExtensions`), so nothing downstream of the catalog knows the
difference — the deploy spec, the chart, and the supervisor see one shape.

### The API

`PUT /extensions/{name}` registers or fully replaces; `GET /extensions` lists
both halves with a `builtin` flag; `DELETE /extensions/{name}` removes one.
Registration is **admin-only** (`?user_id=` must name a user with `is_admin`)
and **synchronous** — it deploys nothing. It makes an image *available*; a
branch opting in is what mounts it, and that already redeploys the branch's
endpoints. Keeping the two apart means registering can never disturb a running
endpoint.

Replacing a registration changes what endpoints mount at their **next** deploy;
live endpoints keep the image they started with. Deregistering is refused
(`409`) while any branch still opts in — otherwise those endpoints would lose
the mount on their next deploy, taking any objects built on the extension with
them.

### The metadata, and why each field exists

An image is not self-describing, so the registration carries what the endpoint
needs to make it usable. Every path is **relative to the mount root**
(`/extensions/<name>`), and the zero value is the layout `deploy/extensions`
produces — a built-in declares none of this.

- **`layout.share_path` / `layout.lib_path`** — the tree differs between
  publishers. The CloudNativePG images use `share` + `lib` where we produce
  `share/postgresql` + `lib/postgresql`. `share_path` is the sharedir *base*:
  PG18 appends `/extension` itself.
- **`layout.library_path`** — dirs of system shared libraries the extension's
  `.so` links and the endpoint image does not carry, which the image therefore
  bundles. The chart renders these into `EXTENSION_LIBRARY_PATH` and the
  **supervisor appends that to its own `LD_LIBRARY_PATH`** before spawning
  anything. The chart cannot set `LD_LIBRARY_PATH` itself: the endpoint image
  bakes it as an `ENV`, and a container `env:` entry of the same name *replaces*
  rather than extends it.
- **`layout.env_paths`** — runtime data dirs those libraries read, by env var
  name. Rendered as ordinary container env with the mount root prefixed. Without
  PostGIS's `PROJ_DATA` the extension registers and *then* `ST_Transform` fails
  on a missing `proj.db`, which is why the tests assert a transform and not just
  `CREATE EXTENSION`.
- **`abi_extra_exempt`** — see below.
- **`preload` / `library`** — a preload extension's library name joins
  `shared_preload_libraries`, so enabling it restarts the postmaster.

### The ABI waiver

This fork sets `FMGR_ABI_EXTRA` to `"yb_amp Postgres"`
(`pg_config_manual.h`), and `dfmgr.c` rejects any module whose magic block says
`"PostgreSQL"` with *"incompatible library: ABI mismatch"* before it runs a line
of code. The fence is load-bearing: `BufferUsage` and `ExplainState` have both
grown fields and `smgropen()` took a third argument, so a stock-built module
that touches any of them is genuinely unsafe.

Essentially every image the ecosystem publishes is built against stock PG18, so
without a waiver the feature would be useless. `abi_extra_exempt` lists the
modules for which **only** the `abi_extra` string is waived — the major version,
`FUNC_MAX_ARGS`, `INDEX_MAX_KEYS`, `NAMEDATALEN` and `FLOAT8PASSBYVAL` are all
still compared exactly (`yb_amp_abi_extra_exempt_libraries` in
`compute/vendor/postgres-v18`). It defaults empty, and it is the sharpest reason
registration is admin-only: a wrong waiver crashes the postmaster.

The GUC is `PGC_SIGHUP` — a `postgresql.conf` setting, which is what the chart
renders, and deliberately not settable per session. A parallel worker restores
the leader's library list *before* its GUCs, so a session-level value would be
at its default exactly when the worker reloads an exempt module, and every
parallel query over that extension would fail.

### Vetting an image before registering it

Registration is trusted input, so the checks are the registrant's, done once by
hand:

```sh
docker create --name x <image> /extract-only && docker export x | tar -C /tmp/ext -xf -
# 1. every PG symbol it references must exist in our postgres, and none may be drifted
nm -D --defined-only pg_install-debug/v18/bin/postgres | awk '{print $NF}' | sort -u > /tmp/pg.syms
nm -D --undefined-only /tmp/ext/lib/<module>.so | awk '{print $NF}' | sort -u > /tmp/mod.syms
comm -12 /tmp/mod.syms /tmp/pg.syms
# 2. its libc/libstdc++ floor must be <= the endpoint base's (debian:bookworm-slim)
objdump -T /tmp/ext/lib/<module>.so | grep -oE 'GLIBC_[0-9.]+|GLIBCXX_[0-9.]+' | sort -uV | tail
# 3. the FULL loader closure must resolve inside a container off the endpoint image
docker run --rm -v /tmp/ext:/extensions/<name>:ro \
  -e LD_LIBRARY_PATH=/usr/local/v18/lib:/extensions/<name>/system \
  <endpoint image> sh -c 'ldd /extensions/<name>/lib/*.so | grep "not found"'
```

Step 3 is the full closure, not the module's own `DT_NEEDED` list, and the two
differ. PostGIS's `.so` files name only geos, proj, gdal, json-c, protobuf-c and
libxml2 — but `libproj` reaches its EPSG database through `libsqlite3` and
`libgdal`'s HDF5 wants `libnuma`, neither of which the image bundles. Those
surface only transitively, which is why `libsqlite3-0` and `libnuma1` are in
[`deploy/endpoint/Dockerfile`](../../deploy/endpoint/Dockerfile). A library the
endpoint image lacks is the one failure registration cannot fix by itself.

**Step 1 is necessary and not sufficient, and this is the sharp edge.** A symbol
diff catches a changed *function* signature (`smgropen`) but not a changed
*struct layout*: field offsets are compiled into the module and generate no
relocations. This fork's added `ExplainState` fields sit ahead of `format`,
`indent` and `pstmt`, so a module that merely installs an explain hook and reads
`es->format` passes a clean symbol diff and then reads at stale offsets. Waiving
a module means knowing it does not *use* `BufferUsage` or `ExplainState` — which
its symbol table cannot tell you, and which needs its source or documentation.

A module that references a drifted symbol, or uses a drifted struct, does not
get a waiver — it gets built against the ABI base like everything else.

[`runbooks/postgis.md`](../../runbooks/postgis.md) walks the whole thing for
PostGIS, which is the worked example throughout.

## Endpoint wiring (Helm chart)

The [`yb-amp-endpoint`](../../deploy/helm/yb-amp-endpoint) chart takes an
`endpoint.extensions` list. Each entry is `{ name, image, preload, library }`,
plus an optional `settings` map and the optional layout fields a registered
image carries (`sharePath`, `libPath`, `libraryPath`, `envPaths`,
`abiExtraExempt`), all defaulting to the amp layout:

- **[`deployment.yaml`](../../deploy/helm/yb-amp-endpoint/templates/deployment.yaml)**
  renders one read-only ImageVolume per entry, mounted at `/extensions/<name>`,
  and one container `env` entry per `envPaths` key with the mount root prefixed.
  (ImageVolume is a Kubernetes 1.33+ beta feature — the `ImageVolume` feature
  gate.)
- **[`configmap.yaml`](../../deploy/helm/yb-amp-endpoint/templates/configmap.yaml)**
  derives `extension_control_path` / `dynamic_library_path` (prepended with
  `$system` / `$libdir`, each entry's `sharePath` / `libPath` joined onto its
  mount root), merges any `abiExtraExempt` names into
  `yb_amp_abi_extra_exempt_libraries`, and lists any `preload` entries in
  `EXTRA_SHARED_PRELOAD_LIBRARIES`. The GUCs, and each entry's `settings` as-is, are
  merged into `COMPUTE_EXTRA_SETTINGS_JSON`, which the supervisor renders into
  the compute's `postgresql.conf`. `settings` come from the catalog's
  `Settings`, which mirrors the manifest's `settings:`
  (`TestCatalogSettingsMatchManifest`). They are for settings an extension
  cannot work without, typically startup-only ones. documentdb's are below. Any `libraryPath` dirs go into a separate
  `EXTENSION_LIBRARY_PATH` — not a GUC, since the loader reads it, not Postgres.

With `endpoint.extensions` empty, the chart output is unchanged from before —
the feature is additive.

## Controller wiring

- **Catalog** —
  [`controller/internal/extensions`](../../controller/internal/extensions/catalog.go)
  is the fixed, code-defined set of extensions this repo builds (`Name`,
  `Version`, `Preload`, `Default`) — and only those.
  [`service.go`](../../controller/internal/extensions/service.go) pairs it with
  the registered ones from the database and is what callers resolve against.
  `Resolve(names)` maps names to pinned image references
  (`<registry>/amp-ext-<name>:<version>` for a built-in, registry from
  `--extension-registry` / `AMP_EXTENSION_REGISTRY`; the registered reference
  verbatim otherwise), skipping unknown names so a stale entry — including one
  since deregistered — can't wedge a deploy. It also **closes over each entry's
  `Requires`**: a built-in whose control file names another extension (documentdb
  requires pgvector and pg_cron) brings that one's image with it, so the mount
  set can be larger than the names asked for. Requested names come first,
  requirements after, deduped. `DefaultNames()` is the always-mounted set
  (currently **empty** — All extensions are registered but off by
  default, so nothing is auto-mounted; opting in is **per branch** — the initial
  set is seeded on the default branch by `createCluster`, a fork inherits its
  ancestor's, and `addBranchExtensions` adds more to a single branch — so every
  endpoint mounts its branch's set).
  `pg_cron` is `Preload: true`, so `Resolve` carries that flag through and the
  chart adds it to `shared_preload_libraries`. `Has(name)` is the API validation
  gate. Keep versions in lockstep with `extensions.yaml`.
- **Endpoint spec** — `resolveBranchExtensions`
  ([`endpointspec.go`](../../controller/internal/task/endpointspec.go)) resolves
  the endpoint's extension set (its branch's opted-in set, below). A step that
  redeploys a whole branch keeps the resolved set across its loop, so it reads
  once. Every step that (re)deploys an endpoint (`create_endpoint`,
  `add_branch_extensions`, `upgrade_endpoint`) passes the
  resolved set to `deploy.EndpointSpecFrom`
  ([`deploy.go`](../../controller/internal/deploy/deploy.go)) rather than
  hand-rolling a spec, and the Kubernetes deployer
  ([`kubernetes.go`](../../controller/internal/deploy/kubernetes.go)) renders
  `EndpointSpec.Extensions` into `endpoint.extensions`. The Docker deployer has no
  ImageVolume equivalent and ignores it (a dev endpoint ships only image-baked
  extensions).

## Default vs. on-demand, and restarts

- **Default extensions** (`Default: true`) would be mounted on every endpoint out
  of the box — the "batteries included" set. **There are none today**: pgvector
  is registered but `Default: false`, so it is opt-in.
- **On-demand extensions** are opted into **per branch** via the API (see
  "Opt-in API" below), which persists the choice on the branch. Because the
  volume set is fixed at pod start, **adding or removing an extension requires
  recreating the endpoint pod** (a rolling restart) — so `addBranchExtensions`
  redeploys the endpoints on the branch whose mounts are behind it. A `preload` extension needs
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

Extensions are opted into **per branch**. `createCluster` takes an optional
`extensions: [name, ...]` array that seeds the set on the new cluster's default
branch, and `addBranchExtensions`
(`POST .../branches/{branchId}/extensions`) adds more to a single branch later
([`openapi_spec.yaml`](../../controller/internal/api/openapi_spec.yaml)):

- Both handlers
  ([`handlers.go`](../../controller/internal/api/handlers.go)) validate every
  name against the catalog (`Has`) — an unknown name is a `400`, not a silent
  drop — and reject the request with `400` if the controller has no catalog
  (the Docker driver / feature off), so a caller never gets a silently-ignored
  opt-in.
- `createCluster` writes the initial set onto the new cluster's default branch;
  `addBranchExtensions` runs an async task that unions the requested names into
  **that branch's** set (`AddBranchExtensions`, an idempotent absolute union)
  and then **redeploys the branch's endpoints whose recorded mount set differs from
  it** so the changed ImageVolume mounts take effect. Both are durable: an endpoint
  create or recreate resolves **catalog defaults ∪ its branch's set**, closed
  over `Requires`.

  The step records what each endpoint mounts after that endpoint's deploy returns,
  which is what makes the comparison safe: a run that fails partway leaves the
  endpoints it never reached unrecorded, so a replay redeploys exactly those — and
  a task whose set is already applied everywhere deploys nothing. The comparison is
  on names, so a catalog revision that moves an extension's *image* without
  renaming it does not reach endpoints this way; `upgrade_endpoint` re-resolves
  against the current catalog and is the path for that.
- The opted-in set (not the catalog defaults) is **readable** on both the
  `Cluster` response (`getCluster`/`listClusters` return the default branch's set
  as the cluster's) and the `Branch` response (`getBranch`/`listBranches` return
  that branch's own set). It is the set as opted into, not the mount set: a
  branch that opted into `["documentdb"]` reports that one name, while its
  endpoints mount documentdb, pgvector and pg_cron. Because pg_cron is preloaded,
  opting into documentdb on a live branch is a restart for that reason too.

## Engines

An **engine** is a named bundle of these extensions, offered as a product flavour
so a caller can ask for "AI Database" without knowing it means pgvector + age +
pg_cron. `createCluster` takes an optional `engine`; the controller resolves its
bundle, **unions it with any `extensions` in the same request** (the two compose,
neither overrides the other), seeds the result onto the default branch, and
records the engine on the cluster row (`amp_registry.clusters.engine`).

| engine | seeds |
|---|---|
| `postgres` (default) | nothing |
| `aidb` | pgvector, age, pg_cron |
| `oracle` | hexabridge |
| `sqlserver` | hexabridge_mssql |
| `documentdb` | documentdb, pgvector, pg_cron |

**documentdb creates itself.** The gateway sidecar serves nothing until the
extension exists: its startup retries on the missing schema and never binds
its port. So an endpoint that mounts documentdb creates it without anyone
running SQL:

- compute_ctl's `post_apply_config` creates a **`documentdb` database**, then
  `CREATE EXTENSION IF NOT EXISTS documentdb CASCADE` in it, whenever
  `pg_documentdb` is preloaded. This is the same hook that creates
  `pg_stat_statements` when its library is preloaded. It runs only on a
  primary, in a task of its own that retries until it succeeds: the gateway
  holds the pod un-Ready until the extension exists, so a transient failure
  must not be final. A fork inherits both from its parent. `CASCADE` creates
  documentdb_core, pg_cron, tsm_system_rows and vector.
- It is a database of its own so documentdb's catalog and collections stay out
  of the user's `postgres`.
- The catalog's documentdb entry sets `cron.database_name` and
  `documentdb.bg_worker_database_name` to `documentdb`. Both are startup-only.
  pg_cron refuses `CREATE EXTENSION` anywhere but `cron.database_name`, and
  documentdb's background index builds run in its worker database. So on a
  documentdb branch, pg_cron lives in `documentdb` too. That is also why
  `addBranchExtensions` refuses (`400`, permanent) to add documentdb to a branch
  that already has pg_cron: it would move the scheduler, and the jobs already in
  `postgres`'s `cron.job` would silently stop firing. A documentdb cluster
  gets pg_cron in its own database from the start.
- The gateway connects to `documentdb`. Its startup probe follows the endpoint
  container's, since it can only bind once the compute is up and the
  extension created.

The recorded engine is **provenance, not a constraint**: it names what the
cluster was created as and is never re-derived. It cannot go stale either —
`addBranchExtensions` only unions and there is no removal API, so an engine's
bundle stays a subset of what its branches carry for the cluster's whole life.
A non-default engine on a controller with no catalog is the same `400` an
explicit extension would get, since the bundle resolves against this catalog.

The engine table lives in three places, and two generated checks keep them in
step: [`extensions.yaml`](../../deploy/extensions/extensions.yaml) is the source
of truth, `TestEngineTableMatchesManifest` asserts
[`engines.go`](../../controller/internal/extensions/engines.go) matches it, and
and the UI's bundled fallback catalog is generated from it (see "Adding a new
extension" below).

### documentdb and postgis

`documentdb.control` lists `postgis` in its `requires`, and amp does not ship it
— it is GPL, which is already a reason it cannot be baked in.
[`documentdb-no-postgis.patch`](../image/patches/documentdb-no-postgis.patch)
takes it out in two places, because the dependency is real in one of them and not
the other:

- **Link time it is not real.** `SHLIB_LINK` is `$(libpq)` alone and
  `POSTGIS_SCHEMA_NAME` reaches the build only through
  `EXTENSION_SQL_ONLY_DEFINES`, so the geospatial sources compile and the built
  `.so` carries no postgis or geos symbol. Dropping `postgis` from `requires` is
  enough to let `CREATE EXTENSION` past its dependency check.
- **In the install SQL it is real.** Getting past that check is not sufficient:
  `documentdb--0.24-0.sql` declares GIST support functions and operator classes
  whose signatures use postgis's `geometry`/`box2df` types and whose op classes
  name postgis's own `geometry_gist_*` support functions, so `CREATE EXTENSION`
  fails at `function geometry_gist_union_2d(bytea, internal) does not exist`.
  Upstream has no build flag for a postgis-less build (its own packaging compiles
  postgis from source rather than skip it), but the install SQL is assembled by
  `cpp`, so the patch gates the three operator-class `#include`s behind
  `__AMP_NO_POSTGIS__`
  and adds that define to `SQL_DEFINES`. The gate is exactly the operator class
  and the two files that `ALTER OPERATOR FAMILY` onto it. It deliberately does
  **not** cover
  `udfs/geospatial/bson_gist_extensibility_functions--0.16-0.sql`: despite the
  name, every function there is one of documentdb's own C functions declared
  over `bson`/`internal`/`float8` with no postgis type in any signature, and the
  geospatial *query* path calls them. Gating that file too is what made 28 tests
  fail on `function documentdb_api_catalog.bson_validate_geography(bson, text)
  does not exist`.

The cost is the geospatial index types (2d/2dsphere), which need those GIST
operator classes. The non-index `$geo` query operators are built from
`bson_dollar_geospatial` and `bson_geospatial_operators`, which carry no postgis
reference and are kept.

Its upstream pg_regress suite runs through the `pgxs` driver like the others, but
needed the patch's help: upstream leaves `REGRESS` a placeholder and selects tests
through a generated schedule, and its `EXTENSIONLOAD` loads postgis
unconditionally, which fails pg_regress setup before any test runs. The patch
names the tests in `REGRESS` and puts the postgis-free loads in `REGRESS_OPTS`.
It runs upstream's full `basic_schedule` — 104 tests. `REGRESS` is *derived* from
that schedule at make time rather than listed in the patch, so a version bump that
adds or renames a test carries over instead of silently narrowing what runs: the
patch copies `basic_schedule` to `log/`, runs upstream's `mutate_schedule.sh` to
resolve its `!PG18_OR_HIGHER!`-style placeholders, and flattens the `test:` lines
into the list. Parallel groups become a serial list; pg_regress preserves the
given order, and the schedule's real ordering constraints are sequential anyway
(`documentdb_test_helpers` defines the helpers the rest call). By-design diffs get
blessed under `test/integration/compute/ext_regress_expected/documentdb/`.

Its three libraries also link **each other** by name, and upstream bakes the
build-time `pkglibdir` into their rpath — but amp never installs them there: the
image is mounted read-only under `/extensions/<name>/lib`, so at load time the
sibling is on no search path and the postmaster dies with
`pg_documentdb_core.so: cannot open shared object file`. The patch adds `$ORIGIN`
to the rpath, which resolves siblings relative to the loaded library itself and
so works wherever the tree is mounted.

It also links **ICU**. Upstream declares no ICU flags because it builds against a
system shared libicu the dynamic linker resolves; amp links a *static* ICU into
Postgres (`Makefile` `ICU_LIBS`), so there is no shared libicu with matching
symbols at runtime and the extension loads with `undefined symbol: ucnv_open_67`.
The patch links the same static ICU the postmaster does, so the symbols resolve
inside the extension and the runtime image's own libicu version is irrelevant.

Two of its three native dependencies are source-built into the ABI-anchor base
rather than apt-installed: libbson (bookworm ships 1.23, documentdb pins 1.28.0)
and Intel's BID decimal library at 2.0u3-1 — the exact source documentdb pins,
taken as Ubuntu's sha-pinned source package with Debian's patch series applied,
since bookworm's `libintelrdfpmath-dev` is 2.0u2. The third, pcre2, comes from
apt — `libpcre2-dev` in the base to build against, and `libpcre2-8-0` in the
endpoint's runtime image to load against, since `pg_documentdb_core.so` links it
dynamically while libbson and BID link statically. `tsm_system_rows` is a baked
contrib module, installed by `postgres.mk`.

## Storage and fork behaviour

Extension **data** (the catalog objects, index contents) is part of a branch's
storage and is inherited by a fork automatically. The extension **files** are
not — they are mounted from images — so every endpoint must mount (at least) the
same extensions, or inherited objects become inoperable and a `preload`
extension's missing library crashes the postmaster.

The set is stored **per branch**, so that superset property holds by
construction: a fork inherits its ancestor's set at fork time, and
`addBranchExtensions` only ever adds to a branch, so a fork's endpoints always
mount a superset of what its objects need. It reuses the existing
`extensions jsonb` column on `amp_registry.branches`
([migration 0002](../../controller/migrations/0002_resource_model.up.sql)) — no
schema change:

- `EnsureCluster`
  ([`core/provision.go`](../../controller/internal/core/provision.go)) writes the
  create-time set onto the default (`is_default`) branch row.
- A fork copies its ancestor's `extensions` column into its own row —
  `RecordBranch`
  ([`registry/store.go`](../../controller/internal/registry/store.go)) and
  `EnsureBranch` (`core/provision.go`).
- `AddBranchExtensions` unions later names into a single branch row;
  `BranchExtensions` reads that branch's row.
- At endpoint create, the step resolves the mount set as
  **catalog defaults ∪ its branch's opted-in set**, closed over each entry's
  `Requires`. Unknown names are dropped by the catalog.

The resolved set is also recorded in the endpoint's recreate `config` blob for
audit / recreate.

## Adding a new extension

> **Changing an extension that is already published** (its patch, `prep`,
> Dockerfile or build flags, under the same `version`): bump `image_revision`
> in its manifest row, and `ImageRevision` in the catalog, in the same PR.
> For a tag built with the recipe label, `build.sh --push` refuses to go on
> without it, because the existing tag was built from the old recipe. For the
> four tags published before the label (pgvector, pg_cron, hexabridge, age)
> nothing catches a missed bump, so it is on the author.

1. **Create the Quay repository first.** A new extension means a new image,
   `quay.io/<registry>/amp-ext-<name>`, and the CI robot can push to repositories
   that already exist but **cannot create one**. Without it the build succeeds and
   the push fails with `unauthorized: access to the requested resource is not
   authorized` — which reads like a broken credential rather than a missing repo,
   and the other extensions give no hint because `build.sh` skips any whose
   version tag is already present, so the new one is the only push attempted. Ask
   whoever administers the Quay org to create it and grant the CI robot write
   access. The same applies to any other new image the extension needs (the
   documentdb gateway has its own, `amp-documentdb-gateway`).
2. Add a row to
   [`deploy/extensions/extensions.yaml`](../../deploy/extensions/extensions.yaml)
   (version, URL, optional patch/prep/preload/dedicated dockerfile), pin its URL
   with `scripts/safe_curl.sh add <url>` (which appends the line to
   `downloads_checksum.txt`; check it is the file you meant), and
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
3. Add the matching entry to the controller catalog
   ([`catalog.go`](../../controller/internal/extensions/catalog.go)) with the
   same version, and `Default: true` only if it should be mounted on every
   endpoint (today all entries are `Default: false`). Set `Preload: true` if the
   extension registers a background worker or needs shared memory (its `_PG_init`
   requires `shared_preload_libraries`), keeping it in step with the manifest's
   `preload:`.
4. Give the manifest row a one-line `description:` (step 2), copy it onto the
   catalog entry in step 3, and regenerate the UI's bundled catalog:
   `cd amp_ui && npm run gen:extensions`, committing the resulting
   `src/config/extensions.json`. The picker reads `GET /extensions` at runtime —
   that is how a registered image reaches it at all — and falls back to the
   bundled list for first paint and when the request fails, so the two must
   agree: a built-in the controller serves without a description renders a blank
   card. `src/config/extensions.test.ts` fails if the checked-in file is stale,
   and `TestBuiltinsAllCarryADescription` fails if a catalog entry lacks one. To add or change an **engine**, edit the manifest's `engines:` block and
   [`engines.go`](../../controller/internal/extensions/engines.go) together, then
   regenerate — `TestEngineTableMatchesManifest` fails if they disagree, and the
   openapi `engine` enum has its own check (`TestEngineEnumMatchesCatalog`).
5. For a non-default extension, opt a branch in by passing
   `extensions: ["<name>"]` to `createCluster` (seeds the default branch), or
   `addBranchExtensions` on a branch later; every endpoint on that branch then
   mounts it.
6. **Add a `regress:` block — this is required, not optional.** It is the only
   thing that runs the extension against a real amp compute: `driver: pgxs` for a
   Makefile with a `REGRESS` target, or `driver: script` + `script: <path>` for a
   custom runner. If the extension's control file requires another extension,
   list it under `requires:` (manifest names) so the harness co-mounts it —
   without that, `CREATE EXTENSION` fails with "extension X is not available".
   If upstream's suite relies on a `--temp-config` (which the driver strips,
   since amp runs against a live compute), carry the settings it needs in
   `pg_settings:` — a list of `key=value` that becomes `--pg-setting` on the
   endpoint, i.e. lines in the compute's postgresql.conf. That is the faithful
   equivalent of upstream's `--temp-config`, and it is deliberately not a
   per-session `PGOPTIONS`: a GUC read once when a library loads (documentdb
   decides about extended-RUM explain output at rum load time) is already fixed
   by the time a session sets it.
   A multi-module tree adds `dir:` — the subdirectory holding the
   regress Makefile, since the harness reads `REGRESS` from the tarball root by
   default. Bless any by-design amp diffs under
   `test/integration/compute/ext_regress_expected/<name>/`.

   Without it nothing ever executes `CREATE EXTENSION <name>`, and a build that
   compiles is not evidence the extension *loads*: documentdb compiled cleanly and
   still failed at postmaster start twice — first preloading a library name that
   did not exist, then on `undefined symbol: ucnv_open_67` because it links ICU
   and amp's is static. Both were caught only by the regress job actually starting
   Postgres.

   **The CI step is not automatic.** The per-extension steps in
   [`amp-on-demand-checks.yml`](../../.github/workflows/amp-on-demand-checks.yml)
   are written out one by one, so a manifest block alone runs nothing — add a step
   for the new extension, with its own `INTEGRATION_PORT_BASE` (reusing another
   extension's collides).

That is the path for an extension **we build**. For one we do not, there is no
manifest row, no catalog entry and no controller release: vet the image
("Registering an extension image"), then `PUT /extensions/{name}` with its
metadata. [`runbooks/postgis.md`](../../runbooks/postgis.md) is the worked
example.

## Testing

- **Controller unit tests** cover the catalog (`Has`/`Resolve`, preload flag
  threaded through), the create_endpoint step (branch-set mount), the
  add_branch_extensions step (union + redeploy of the endpoints that are behind), and the
  API (`createCluster extensions[]` / `addBranchExtensions` → 202/400, deploy spec,
  persistence) — see `controller/internal/extensions`,
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
  fetch and is skipped on forks. For `hexabridge_mssql` it checks the extension
  landed in schema `sys` and calls T-SQL `charindex`, `len` and `stuff`. For
  `age` it does `LOAD 'age'`, creates a graph,
  and runs a `cypher()` CREATE/MATCH round-trip. `--ext` also takes a
  comma-separated list to **co-mount** several extensions on one endpoint (e.g.
  `--ext pgvector,age` — both non-preload — mounts both trees and runs each one's
  smoke in turn). `--registered <file>` mounts a **registered** extension instead
  of a built one, from a `registerExtension` body under
  [`ext_fixtures/`](../../test/integration/compute/ext_fixtures/): a local compute
  has no controller, so the driver reads the same JSON and applies it by hand —
  pulling the image, exporting the tree (the bundled `system/` dir is neither
  `share` nor `lib`), putting that dir on `LD_LIBRARY_PATH`, exporting the
  `env_paths`, and passing `yb_amp_abi_extra_exempt_libraries`. `--registered
  ext_fixtures/postgis.json` then asserts `ST_Area`, a GiST nearest-neighbour
  scan and an `ST_Transform` — the transform is what proves `proj.db` was found,
  not just that the extension registered. It needs a host with **glibc ≥ 2.35**
  (that image's floor). It is the local-deploy twin of `deploy/smoke-test.sh`'s
  `SMOKE_EXTENSIONS` check.
- **Upstream regression suites (local)** —
  [`extensions.sh --regress`](../../test/integration/compute/extensions.sh) runs
  each extension's **own** upstream regression suite against the mounted
  ImageVolume — the extension analogue of `pg_regress.sh` for core Postgres (#608)
  —   driven by the manifest row's `regress:` block: `driver: pgxs` runs
  `pg_regress` from the extension's Makefile `REGRESS` (pgvector, pg_cron, age,
  hexabridge_mssql)
  against the live compute; `driver: script` runs the extension's own runner
  (hexabridge's `scripts/regression.sh`). The suite lives in the source tree (not
  the `FROM scratch` image), so the driver re-fetches the source as
  `extension.Dockerfile` does. By-design amp diffs are blessed as alternative
  expected outputs under
  [`test/integration/compute/ext_regress_expected/<name>/`](../../test/integration/compute/ext_regress_expected/README.md),
  never by editing upstream goldens. The `amp_extensions_local` CI suite runs each
  extension with `--regress` (one per step, distinct port block), and it fires
  automatically when a diff touches `deploy/extensions/`.
- **Smoke test** — `deploy/smoke-test.sh` with `SMOKE_EXTENSIONS=1` exercises the
  real K8s ImageVolume path end-to-end: pgvector and `age` co-mounted on one
  endpoint (asserting both `/extensions/<name>` mounts, the pgvector data query,
  and an AGE `cypher()` round-trip), then pg_cron and hexabridge on their own,
  then hexabridge_mssql through `engine: "sqlserver"`,
  then the **registration** path — a non-admin `PUT` refused with 403, an admin
  registering `ext_fixtures/postgis.json`, a cluster opting in, the pod carrying
  the registered layout (`share/extension`, the bundled `system/libgeos_c.so.1`,
  `share/proj/proj.db`), `CREATE EXTENSION` + `ST_Transform`, and deregistration
  refused with 409 while the branch still uses it. Last comes the **documentdb
  gateway**:
  - an `engine:"documentdb"` cluster's endpoint runs the sidecar, Ready, and
    reports `documentdb_address`;
  - with `SMOKE_DOCUMENTDB_CONNECTIVITY` set (CI sets `nlb_external`,
    allow-listed to the runner), the endpoint is created in that mode, the
    address must be on the endpoint's external host, and the `-lb` Service's
    `documentdb` port or the shard's SNI route must exist;
  - pymongo from upstream's pinned `functional-tests` image logs in as
    `cloud_admin` over SCRAM-SHA-256: through a port-forward to the Service, or
    straight at `documentdb_address` in an external mode;
  - over the wire protocol it inserts, indexes, finds, updates, aggregates and
    deletes, and SQL reads the same documents back;
  - a wrong password is refused.
- **DocumentDB functional-test gate** — `/ci amp_deploy_gke_documentdb` runs
  upstream documentdb's own PR gate against the deployed gateway
  ([`deploy/documentdb-functional/`](../../deploy/documentdb-functional/README.md)).
  That is the ~10,500 allowlisted functional tests, taken from the release amp
  ships, minus amp's reasoned exclusions. It passes only if every gated test
  passes.

## Code anchors

- Build: [`deploy/extensions/`](../../deploy/extensions/) (`pg18-build.Dockerfile`,
  `extension.Dockerfile`, `extensions.yaml`, `build.sh`).
- Chart: [`deploy/helm/yb-amp-endpoint/`](../../deploy/helm/yb-amp-endpoint/)
  (`values.yaml` `endpoint.extensions`, `templates/deployment.yaml`,
  `templates/configmap.yaml`).
- Controller: [`controller/internal/extensions/catalog.go`](../../controller/internal/extensions/catalog.go),
  [`controller/internal/task/endpointspec.go`](../../controller/internal/task/endpointspec.go)
  (`resolveBranchExtensions` — where the mount set is resolved),
  [`controller/internal/task/step_create_endpoint.go`](../../controller/internal/task/step_create_endpoint.go),
  [`controller/internal/task/step_add_branch_extensions.go`](../../controller/internal/task/step_add_branch_extensions.go),
  [`controller/internal/task/step_upgrade_endpoint.go`](../../controller/internal/task/step_upgrade_endpoint.go),
  [`controller/internal/deploy/kubernetes.go`](../../controller/internal/deploy/kubernetes.go),
  [`controller/internal/core/provision.go`](../../controller/internal/core/provision.go)
  (`BranchExtensions` / `AddBranchExtensions`; `EnsureBranch` copies the parent's set),
  [`controller/internal/registry/store.go`](../../controller/internal/registry/store.go)
  (`RecordBranch` — a fork copies its ancestor's extensions),
  [migration 0002](../../controller/migrations/0002_resource_model.up.sql)
  (`amp_registry.branches.extensions`, per branch).
- UI: [`amp_ui/src/components/ui/extensions-field.tsx`](../../amp_ui/src/components/ui/extensions-field.tsx)
  (the picker), offered by the create-cluster wizard and
  [`ExtensionsDialog.tsx`](../../amp_ui/src/pages/multitenancy/components/ExtensionsDialog.tsx)
  on a branch. It takes its options from `GET /extensions` (`useExtensionOptions`),
  which is what makes a registered image selectable; the list generated from the
  manifest by [`gen-extensions.py`](../../amp_ui/scripts/gen-extensions.py) into
  `amp_ui/src/config/extensions.json` remains as first paint and fallback, and
  holds built-ins only.
  [`ExtensionsPage.tsx`](../../amp_ui/src/pages/ExtensionsPage.tsx) is the admin
  surface: it lists the registered images and imports one (`useAdminUser` supplies
  the acting identity — deliberately not `useCurrentUser`, which is the pool owner
  elsewhere).
- Engines: [`controller/internal/extensions/engines.go`](../../controller/internal/extensions/engines.go)
  (the bundles), `engine` on `CreateClusterRequest` / `Cluster` in
  [`openapi_spec.yaml`](../../controller/internal/api/openapi_spec.yaml),
  [migration 0002](../../controller/migrations/0002_resource_model.up.sql)
  (`amp_registry.clusters.engine`).
- Patches: [`compute/image/patches/`](../image/patches/) (reused verbatim).
- Registration: [`controller/internal/extensions/service.go`](../../controller/internal/extensions/service.go)
  (the built-in ∪ registered union, and the metadata validation),
  [`controller/internal/api/extensions.go`](../../controller/internal/api/extensions.go)
  (the handlers and the admin gate),
  [`controller/internal/core/registered_extensions.go`](../../controller/internal/core/registered_extensions.go)
  (`amp_registry.extensions`), [migration 0008](../../controller/migrations/0008_registered_extensions.up.sql).
- ABI waiver: `yb_amp_abi_extra_exempt_libraries` in the forked Postgres
  (`compute/vendor/postgres-v18`: `src/backend/utils/fmgr/dfmgr.c`
  `module_abi_compatible` / `abi_extra_exempt`, `src/backend/utils/misc/guc_tables.c`).
- Supervisor: [`compute/endpoint_supervisor/src/main.rs`](../endpoint_supervisor/src/main.rs)
  (`append_ld_library_path`, reading `EXTENSION_LIBRARY_PATH`).
