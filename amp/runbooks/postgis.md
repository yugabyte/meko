# Runbook: add PostGIS to an amp deployment

PostGIS is not built by amp and is not in the built-in extension catalog. It
gets in the way every third-party extension does: an admin **registers** a
prebuilt image with the controller, and branches opt into it by name. This
runbook is the whole procedure, plus what each failure looks like when a step is
missed.

Background on the mechanism: [`compute/architecture/extensions.md`](../compute/architecture/extensions.md),
section "Registering an extension image". The registration body used below is
checked in at
[`test/integration/compute/ext_fixtures/postgis.json`](../test/integration/compute/ext_fixtures/postgis.json)
and is what the smoke test and the local driver both use.

## Conventions

```bash
export NS=amp-dev                                   # the controller's namespace
kubectl -n "$NS" port-forward svc/amp-controller 8080:8080 &
export BASE=http://127.0.0.1:8080/api/v1
export ADMIN=<a user id with is_admin>              # see "Get an admin" below
```

## 0. Prerequisites

- **Kubernetes 1.33+** with the `ImageVolume` feature gate. Below it the kubelet
  brings the pod up with **no mount and no API error** — the extension simply is
  not there.
- **An endpoint image carrying `libsqlite3-0` and `libnuma1`.** These are
  transitive dependencies of the libraries the PostGIS image bundles (libproj
  reads its EPSG database through sqlite3; libgdal's HDF5 wants libnuma), so
  they appear in no `.so`'s `DT_NEEDED` and only in the full `ldd` closure. They
  are in `deploy/endpoint/Dockerfile`; an older endpoint image will fail at
  `CREATE EXTENSION` with `libsqlite3.so.0: cannot open shared object file`.
- **A forked Postgres with `yb_amp_abi_extra_exempt_libraries`.** Without it the
  stock-built PostGIS module is refused outright (step 4's first failure mode).

### Get an admin

Registration is admin-only. There is no authentication yet, so the caller names
itself with `?user_id=`:

```bash
ADMIN=$(curl -sf -X POST -H 'content-type: application/json' \
  -d '{"is_admin":true}' "$BASE/users" | jq -r .id)
```

## 1. Pick the right tag

Use the **`-bookworm`** variant, never `-trixie`:

| | endpoint runtime (`debian:bookworm-slim`) | `-trixie` build needs |
|---|---|---|
| glibc | 2.36 | `GLIBC_2.38` |
| libstdc++ | `GLIBCXX_3.4.30` | `GLIBCXX_3.4.32` |

The bookworm build tops out at `GLIBC_2.35` / `GLIBCXX_3.4.29` and fits. Verify
before registering anything:

```bash
docker create --name gis ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm /x
docker export gis | tar -C /tmp/gis -xf - && docker rm gis
objdump -T /tmp/gis/lib/postgis-3.so | grep -oE 'GLIBC_[0-9.]+|GLIBCXX_[0-9.]+' | sort -uV | tail -3
```

Then confirm the loader closure resolves against the **real endpoint image** —
not the build host, which answers a different question:

```bash
docker run --rm -v /tmp/gis:/extensions/postgis:ro \
  -e LD_LIBRARY_PATH=/usr/local/v18/lib:/extensions/postgis/system \
  <endpoint image> sh -c 'ldd /extensions/postgis/lib/*.so | grep "not found"'
```

Silence is the pass. Anything listed is a package the endpoint image needs.

## 2. Register the image

```bash
curl -sf -X PUT -H 'content-type: application/json' \
  --data-binary @amp/test/integration/compute/ext_fixtures/postgis.json \
  "$BASE/extensions/postgis?user_id=$ADMIN" | jq .
```

The body carries the metadata the endpoint needs, all paths relative to
`/extensions/postgis`:

| field | value | why |
|---|---|---|
| `image` | `ghcr.io/cloudnative-pg/…:3.6.2-18-bookworm` | pinned; `:latest` and untagged are refused |
| `layout.share_path` | `share` | CNPG's tree, not our `share/postgresql`. The sharedir **base** — PG18 appends `/extension` |
| `layout.lib_path` | `lib` | ditto for `dynamic_library_path` |
| `layout.library_path` | `["system"]` | the bundled geos/proj/gdal; reaches the loader via `EXTENSION_LIBRARY_PATH` |
| `layout.env_paths` | `PROJ_DATA`, `GDAL_DATA` | the PROJ/GDAL data dirs; without `PROJ_DATA`, `ST_Transform` fails on a missing `proj.db` |
| `abi_extra_exempt` | the five modules | the image is built against stock PG18 |

Confirm it landed and is not mistaken for something we ship:

```bash
curl -sf "$BASE/extensions" | jq '.extensions[]|select(.name=="postgis")|{builtin,image}'
# { "builtin": false, "image": "ghcr.io/cloudnative-pg/postgis-extension:3.6.2-18-bookworm" }
```

## 3. Opt a branch in

At cluster creation:

```bash
curl -sf -X POST -H 'content-type: application/json' \
  -d '{"name":"gis","extensions":["postgis"]}' \
  "$BASE/storage-pools/$POOL/clusters"
```

or on an existing branch (this **redeploys every endpoint on the branch** —
the ImageVolume set is fixed at pod start):

```bash
curl -sf -X POST -H 'content-type: application/json' \
  -d '{"extensions":["postgis"]}' \
  "$BASE/storage-pools/$POOL/clusters/$CLUSTER/branches/$BRANCH/extensions"
```

Both validate the name against the catalog, so a `400 unknown extension
"postgis"` here means step 2 did not happen on **this** controller.

## 4. Verify on the endpoint

```bash
POD=$(kubectl -n "$NS" get pods --no-headers | awk "/yb-ep-$ENDPOINT/{print \$1; exit}")
kubectl -n "$NS" exec "$POD" -- ls /extensions/postgis          # share lib system licenses
kubectl -n "$NS" exec "$POD" -- test -f /extensions/postgis/share/proj/proj.db && echo "proj.db ok"
kubectl -n "$NS" exec "$POD" -- sh -c 'tr "\0" "\n" < /proc/1/environ | grep -E "EXTENSION_LIBRARY_PATH|PROJ_DATA"'
```

Then the one query that exercises all of it — `ST_Transform` is the assertion
that matters, because a build that resolved libproj but not its database still
answers `ST_AsText`:

```sql
CREATE EXTENSION postgis;
SELECT postgis_full_version();
SELECT round(ST_X(ST_Transform(ST_SetSRID(ST_MakePoint(180,0),4326),3857))::numeric,2);
-- 20037508.34  (half the Web Mercator world; a spec constant, not a fixture)
```

## Failure modes, in the order you hit them

Each is a specific missing piece, and the error names it if you know the map.

| Symptom | Cause | Fix |
|---|---|---|
| `400 unknown extension "postgis"` on opt-in | not registered on this controller | step 2 |
| `403` on `PUT /extensions/postgis` | `user_id` is not an admin | use an admin id |
| `/extensions/postgis` missing in the pod | Kubernetes < 1.33, or the `ImageVolume` gate is off | upgrade / enable the gate |
| `could not load library …: libgeos_c.so.1: cannot open shared object file` | `layout.library_path` missing, so `EXTENSION_LIBRARY_PATH` is unset | add `["system"]` and redeploy |
| `… libsqlite3.so.0: cannot open shared object file` | endpoint image lacks the transitive deps | endpoint image with `libsqlite3-0`, `libnuma1` |
| `incompatible library "…postgis-3.so": ABI mismatch` — *Server has ABI "yb_amp Postgres", library has "PostgreSQL"* | `abi_extra_exempt` missing | list the modules and redeploy |
| `CREATE EXTENSION` fine, `ST_Transform` errors on `proj.db` | `PROJ_DATA` not set | add `layout.env_paths` and redeploy |
| `ImagePullBackOff` on the endpoint pod | private registry | there is no per-extension credential; the cluster's `imagePullSecrets` must cover it |

A change to the registration takes effect on an endpoint's **next** deploy —
editing the row does not restart anything. Re-run the branch opt-in (or recreate
the endpoint) after fixing one of the above.

## Removing it

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE "$BASE/extensions/postgis?user_id=$ADMIN"
```

`409` while any branch still opts in — that is deliberate: dropping the row would
strip the mount on those endpoints' next deploy and take any PostGIS objects with
it. Drop it from the branches first. Deregistering does not restart anything;
endpoints already mounting it keep running until they are next recreated.
