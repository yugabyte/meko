# Runbook: yb-neon startup failures

A component won't start, won't register, or the compute can't serve queries. This runbook covers the
yb-neon bring-up failure modes by **symptom → cause → fix**. It is topology-agnostic: the compute,
pageserver, safekeeper, and storage_controller may be co-located or on separate boxes. Substitute
the conventions below.

## Conventions (fill these in for your deployment)
```bash
export PS_HOST=...        export PS_HTTP_PORT=...      # yb-pageserver
export SC_BASE=http://<sc-host>:<sc-port>             # storage_controller
export SK_HOST=...        export SK_PG_PORT=...        # safekeeper PG listen
export COMPUTE_HOST=...   export COMPUTE_PORT=...      # neon compute
export YB_HOST=...  export YB_PORT=...  export YB_DB=...   # YugabyteDB backing store
export NEON_YB_PASSWORD=...                            # YB password, used by PS/SK/endpoint-storage
export PSQL=<path-to>/psql
```
Component config locations vary by deployment but typically:
`pageserver.toml` + `identity.toml` in the PS data dir; the safekeeper/SC/endpoint-storage start
scripts; the yb_light_local repo dir (`.yb_light/config`, `.yb_light/endpoint_storage/`, `.yb_light/endpoints/<ep>/`).

---

## Recommended start order
Start bottom-up so each component's dependency is already listening. **Verify at each checkpoint**
before moving on — a silent half-failure here surfaces as a confusing error three steps later.

1. **storage_controller** → checkpoint: `curl $SC_BASE/control/v1/node` returns (even if `[]`).
2. **safekeeper** → checkpoint: its HTTP `/v1/status` (or process up + port listening).
3. **endpoint_storage** (if used) → checkpoint: process up, no TLS errors in its log.
4. **pageserver** → checkpoint: `curl http://$PS_HOST:$PS_HTTP_PORT/v1/status` returns OK.
5. **Register PS + SK with the SC** (see "Manual registration" — they may not auto-register).
6. **Create tenant/timeline** (via yb_light_local or SC API).
7. **Create + start the compute endpoint** → checkpoint: `psql … 'SELECT 1'`.

---

## Symptom index
| Symptom / error text | Cause | Section |
|---|---|---|
| `yb_light_local init` panics / serde error on the config TOML | init config schema mismatch | **A** |
| SC: `Migration error: no schema has been selected to create in` | backing DB missing the schema | **B** |
| SK: `unexpected argument '--<flag>'` | flag removed/renamed in this build | **C** |
| endpoint_storage / PS: `no pg_hba.conf entry … no encryption` | client connected to YB without TLS | **D** |
| SC node list empty after PS starts; PS log `register: None` | PS didn't auto-register | **E** |
| Timeline create: `couldn't find 1 safekeepers in different AZs (found: 0)` | SK not registered with SC | **F** |
| psql: `libreadline.so.7: cannot open shared object file` | bundle psql wants older readline | **G** |
| Compute postmaster dies with `EADDRINUSE` on its own port | yb_light_local port collision | **H** |
| Fresh `cloud_admin` can't auth over socket / password rejected | role needs socket-trust password set | **I** |
| PS can't find the safekeeper (broker-less builds) | missing `safekeeper_pg_addr` | **J** |

---

## A. `yb_light_local init` config schema / serde errors
`yb_light_local init --config <file>` is strict about its TOML. Common cascade of failures, each fixed
by adding the missing/retyped key:
- `remote_storage` under `[[pageservers]]` must be a **string-encoded inline table**, not a native
  TOML map — i.e. `remote_storage='{host="…", port=…, …}'` (quoted string), not `remote_storage={…}`.
- Missing top-level keys it requires: `default_tenant_id`, `generate_local_ssl_certs`,
  `control_plane_api`. A panic at `local_env.rs:…` on init usually means `control_plane_api` is absent.
- `[endpoint_storage]` stanza must be present.
- endpoint_storage `YbStore` derivation needs `[storage_controller] database_url` to be a **full**
  `postgresql://…` URL (not `host:port`).

Fix by adding each missing key and re-running init. After a successful init, the generated
`.yb_light/config` may **drop the `[[pageservers]]` stanza** when the PS is off-box / self-registering —
that's expected, not a failure.

## B. SC migration: "no schema has been selected to create in"
A freshly created backing DB has no `storage_controller` schema, and the SC's `search_path` points
at it. Create it before starting the SC:
```bash
PGPASSWORD="$NEON_YB_PASSWORD" $PSQL \
  "host=$YB_HOST port=$YB_PORT user=yugabyte dbname=$YB_DB sslmode=require" \
  -c "CREATE SCHEMA IF NOT EXISTS storage_controller;"
```
(The SC's `--database-url` typically pins `options=-csearch_path%3Dstorage_controller`.) Same idea
if a `pageserver` schema is expected and missing.

## C. Safekeeper: "unexpected argument '--<flag>'"
Build drift removes/renames flags. Known: **broker-less builds removed `--broker-endpoint` and
`--control-file-backend`** from the safekeeper. If the SK refuses to start on an unknown flag, run
`<build>/safekeeper --help`, diff against the start script, and delete the stale flags. Don't pin an
old start script to a new build without checking `--help`.

## D. "no pg_hba.conf entry … no encryption" (TLS to YB)
A client opened a **non-TLS** connection to YugabyteDB, which requires TLS. Seen when the
endpoint_storage `YbStore` config derived `sslmode:None`. Fix the relevant config to force TLS and
supply the password env, e.g. in `.yb_light/endpoint_storage/endpoint_storage.json` add
`"sslmode":"require"` and `"password_env":"NEON_YB_PASSWORD"`. Any component talking to YB
(`pageserver.toml remote_storage`, safekeeper `--remote-storage`, SC `--database-url`) must specify
`sslmode=require`/`sslmode="require"`.

> Separately: some builds **cannot** do TLS to YB at all due to a rustls→OpenSSL switch
> (`open_tls … (openssl)` / `SSL_connect` failures). That's a build defect, not a config fix —
> see the memory note on `448dd1618/e2c2a2e80 YB TLS connect fail`; vet the build with
> `strings <bin> | grep -i 'SSL_connect\|OpenSSL'`.

## E. PS doesn't auto-register with the SC (empty node list)
In broker-less builds the PS no longer auto-registers; its log shows `register: None` and
`$SC_BASE/control/v1/node` stays `[]`. Register it by hand (adjust addrs/ports to the PS):
```bash
curl -s -X POST $SC_BASE/control/v1/node -H 'Content-Type: application/json' -d '{
  "node_id": 1,
  "listen_pg_addr": "'"$PS_HOST"'",   "listen_pg_port": <ps_pg_port>,
  "listen_http_addr": "'"$PS_HOST"'", "listen_http_port": '"$PS_HTTP_PORT"',
  "listen_grpc_addr": "'"$PS_HOST"'", "listen_grpc_port": <ps_grpc_port>
}'
```
Then re-check `curl $SC_BASE/control/v1/node`.

## F. Timeline create: "couldn't find 1 safekeepers in different AZs (found: 0)"
The safekeeper isn't registered with the SC, so it can't place the timeline. Register it:
```bash
curl -s -X POST $SC_BASE/control/v1/safekeeper/1 -H 'Content-Type: application/json' -d '{
  "id": 1, "region_id": "local", "version": 0,
  "host": "'"$SK_HOST"'", "port": '"$SK_PG_PORT"',
  "http_port": <sk_http_port>, "availability_zone_id": "sk-1"
}'
```
If a tenant/timeline was created before the SK was registered, **delete and recreate** it so it
lands on the now-known SK: `curl -X DELETE $SC_BASE/v1/tenant/<tenant_id>` then recreate.

## G. psql `libreadline.so.7` missing
```bash
mkdir -p /tmp/psqllibs; ln -sf /usr/lib64/libreadline.so.8 /tmp/psqllibs/libreadline.so.7
export LD_LIBRARY_PATH=/tmp/psqllibs:$LD_LIBRARY_PATH
```
Do this on whichever box you run that psql from. Many bundles ship a psql that doesn't need it.

## H. Compute postmaster dies with phantom EADDRINUSE
yb_light_local can set the endpoint's `internal_http_port` **equal to** its `pg_port`; `compute_ctl`
then binds `*:pg_port` and the postmaster fails to bind the same port (looks like a phantom
`EADDRINUSE`). Fix: ensure the endpoint's `internal_http_port` ≠ `pg_port` (edit the endpoint spec /
`.yb_light` config), then restart the endpoint.

## I. Fresh `cloud_admin` can't authenticate
A newly created compute role may have no usable password for socket/`trust` auth. Set one so local
tooling can connect:
```sql
ALTER ROLE cloud_admin PASSWORD 'Yugabyte!1';
```
(Run via the local socket as the bootstrap superuser if password auth is what's failing.)

## J. PS can't find the safekeeper (broker-less builds)
With the storage_broker removed, the PS discovers the safekeeper via a `safekeeper_pg_addr` key in
`pageserver.toml` (e.g. `safekeeper_pg_addr = '$SK_HOST:$SK_PG_PORT'`) instead of dialing a broker.
If the PS can't pull WAL / find the SK, confirm that key is present and points at the SK's
advertised PG address. There is no `broker_endpoint` in these builds.

---

## Fast triage checklist
```bash
# 1. Is each component's port listening?
ss -ltnp | grep -E ':(<ps_pg>|<ps_http>|<sk_pg>|<sk_http>|<sc_port>|<compute_port>)'
# 2. SC sees the nodes?
curl -s $SC_BASE/control/v1/node; echo
# 3. PS healthy?
curl -s http://$PS_HOST:$PS_HTTP_PORT/v1/status; echo
# 4. Compute serves queries + reads from the right PS?
PGPASSWORD="$PGPASSWORD" $PSQL "host=$COMPUTE_HOST port=$COMPUTE_PORT user=cloud_admin dbname=postgres sslmode=disable" \
  -tAc "select 1; show neon.pageserver_connstring;"
```
If `neon.pageserver_connstring` doesn't point at the PS you expect, the compute is talking to the
wrong pageserver — re-check tenant placement (Step E/F) before anything else.
