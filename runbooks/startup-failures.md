# Runbook: yb-light startup failures

A component won't start, won't register, or the compute can't serve queries. This runbook covers the
yb-light bring-up failure modes by **symptom → cause → fix**. It is topology-agnostic: the compute,
pageserver, safekeeper, and storage_controller may be co-located or on separate boxes. Substitute
the conventions below.

## Conventions (fill these in for your deployment)
```bash
export PS_HOST=...        export PS_HTTP_PORT=...      # yb-pageserver
export SC_BASE=http://<sc-host>:<sc-port>             # storage_controller
export SK_HOST=...        export SK_PG_PORT=...        # safekeeper PG listen
export COMPUTE_HOST=...   export COMPUTE_PORT=...      # yb-light compute
export YB_HOST=...  export YB_PORT=...  export YB_DB=...   # YugabyteDB backing store
export YB_STORAGE_PASSWORD=...                            # YB password, used by PS/SK/endpoint-storage
export PSQL=<path-to>/psql
```
Component config locations vary by deployment but typically:
`pageserver.toml` + `identity.toml` in the PS data dir; the safekeeper/SC/endpoint-storage start
scripts; the yb_light_local repo dir (`.yb_light/config`, `.yb_light/endpoints/<ep>/` — which
holds that endpoint's `pageserver/`, `safekeeper/`, and `endpoint_storage/`).

---

## Recommended start order
Start bottom-up so each component's dependency is already listening. **Verify at each checkpoint**
before moving on — a silent half-failure here surfaces as a confusing error three steps later.

1. **storage_controller** → checkpoint: `curl $SC_BASE/control/v1/node` returns (even if `[]`).
2. **safekeeper** → checkpoint: its HTTP `/v1/status` (or process up + port listening).
3. **endpoint_storage** (if used) → checkpoint: process up, no TLS errors in its log.
4. **pageserver** — *single-branch model (#16)*: the process refuses to start unless
   `pageserver.toml` carries `branch_id`/`generation`. On a fresh repo this is
   normal: `yb_light_local start` prints `Skipping pageserver …: no timeline identity yet` and
   `branch create` bakes the identity in, bumps the generation via the SC attach-hook, and starts
   the pageserver itself. Manual deployments must write the identity (and `[timeline_init]` for a
   first boot) before spawning. Checkpoint once started:
   `curl http://$PS_HOST:$PS_HTTP_PORT/v1/status` returns OK.
5. **Register SK with the SC** (see "Manual registration"). PS registration is launcher-driven
   (`node_register` + attach-hook happen inside every `yb_light_local pageserver start`).
6. **Create the branch** (via `yb_light_local branch create`; the pageserver bootstraps the branch from its
   `[timeline_init]` config — the SC only observes via `timeline_detail` and then creates the
   branch on the safekeeper).
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
| PS exits at startup: `pageserver.toml must set branch_id/generation` | single-branch identity missing — launcher hasn't assigned one (run `branch create`, or write the identity + `[timeline_init]` into pageserver.toml) | — |
| PS exits at startup: `timeline … has no timeline_state row and no [timeline_init]` | row missing and no creation mode configured (wiped YB DB, or toml written without `[timeline_init]`) | — |
| SK exits at startup: `YB-WAL mode requires --branch-id (single-branch-per-process)` | single-branch identity missing from `safekeeper.toml` — the SK serves one branch and scopes its load to it. Provisioned SKs get `branch_id` written by `allocate_safekeeper_conf`; a hand-written `safekeeper.toml` must set it alongside `yb_storage_conn` | — |
| Timeline create: `couldn't find 1 safekeepers in different AZs (found: 0)` | SK not registered with SC | **F** |
| psql: `libreadline.so.7: cannot open shared object file` | bundle psql wants older readline | **G** |
| Compute postmaster dies with `EADDRINUSE` on its own port | yb_light_local port collision | **H** |
| Fresh `cloud_admin` can't auth over socket / password rejected | role needs socket-trust password set | **I** |
| PS can't find the safekeeper (broker-less builds) | missing `safekeeper_pg_addr` | **J** |
| Any process exits at startup: `unknown configuration item(s) …` / `unknown field …` | misspelled, stale, or misplaced config key — rejected, not ignored | **K** |
| SC: `Postgres directory 'lib' not found in <dir>/pg_install` | init baked a `pg_distrib_dir` that doesn't exist on this box | **L** |
| SC connects to YB and fails: `password missing` / `building database URL from --yb-config` | init omits `password_env`/`sslmode` from the YB conn blocks | **M** |
| `pageserver.toml` top-level knob rejected at startup as `<section>.<key>` | a top-level key was placed **after** a `[section]` header | **N** |
| PS/SK panics at startup: `OS can't spawn worker thread: Resource temporarily unavailable` | per-user process/thread limit (`ulimit -u` / cgroup `pids.max`) exhausted | **O** |

---

## A. `yb_light_local init` config schema / serde errors
`yb_light_local init --config <file>` is strict about its TOML. Common cascade of failures, each fixed
by adding the missing/retyped key:
- `yb_storage_conn` under `[[pageservers]]` must be a **string-encoded inline table**, not a native
  TOML map — i.e. `yb_storage_conn='{ host_ports="…:5433", user="…", dbname="…" }'` (quoted string),
  not `yb_storage_conn={…}`. It carries no `table_name` (each component hardcodes its tables).
- Missing top-level keys it requires: `default_branch_id`, `generate_local_ssl_certs`,
  `control_plane_api`. A panic at `local_env.rs:…` on init usually means `control_plane_api` is absent.
- `[endpoint_storage]` stanza must be present.
- endpoint_storage `YbStore` derivation needs the `[storage_controller.yb]` block (the shared
  `host_ports`/`user`/`dbname` connection block) to be present.

Fix by adding each missing key and re-running init. After a successful init, the generated
`.yb_light/config` may **drop the `[[pageservers]]` stanza** when the PS is off-box / self-registering —
that's expected, not a failure.

## B. SC migration: "no schema has been selected to create in"
A freshly created backing DB has no `storage_controller` schema, and the SC's `search_path` points
at it. Create it before starting the SC:
```bash
PGPASSWORD="$YB_STORAGE_PASSWORD" $PSQL \
  "host=$YB_HOST port=$YB_PORT user=yugabyte dbname=$YB_DB sslmode=require" \
  -c "CREATE SCHEMA IF NOT EXISTS storage_controller;"
```
(The SC's `--database-url` typically pins `options=-csearch_path%3Dstorage_controller`.) Same idea
if a `pageserver` schema is expected and missing.

## C. Safekeeper: "unexpected argument '--<flag>'"
Build drift removes/renames flags. The safekeeper's whole config surface now lives in
`<datadir>/safekeeper.toml` (written by the launcher; the analogue of `pageserver.toml`), so the
binary accepts only `-D <datadir>`, `--init`, and `--dump-control-file`. A start script still passing
`--id`/`--listen-pg`/etc. will fail — move those settings into `safekeeper.toml`
(see `compute/architecture/safekeeper.md` "Run / configure"). Earlier drift also removed `--broker-endpoint`
and `--control-file-backend`. Run `<build>/safekeeper --help` and diff against the start script.

## D. "no pg_hba.conf entry … no encryption" (TLS to YB)
A client opened a **non-TLS** connection to YugabyteDB, which requires TLS. Seen when the
endpoint_storage `YbStore` config derived `sslmode:None`. Fix the relevant config to force TLS and
supply the password env, e.g. in `.yb_light/endpoints/<ep>/endpoint_storage/endpoint_storage.json` add
`"sslmode":"require"` and `"password_env":"YB_STORAGE_PASSWORD"`. Any component talking to YB
(`pageserver.toml yb_storage_conn`, safekeeper `yb_storage_conn`, SC `--database-url`) must specify
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
If a branch was created before the SK was registered, **delete and recreate** it so it
lands on the now-known SK: `curl -X DELETE $SC_BASE/v1/branch/<branch_id>` then recreate.

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

## K. A process refuses to start on a bad config item
Config is **fail-fast across every process** (pageserver, safekeeper, endpoint_storage,
storage_controller, and the `yb_light_local` on-disk config): a config item that is wrong is
refused at startup rather than logged and ignored. The classes that abort start-up:

- **Unknown / misspelled / stale key** — e.g. `unknown configuration item(s) in pageserver.toml:
  timeline_init.scale_factor`, or serde `unknown field \`foo\``. The pageserver names the offending
  paths; the safekeeper / endpoint_storage / `--yb-config` (`YbConnConfig`) report the serde field.
- **Wrong type / out-of-range value / duplicate key** — already failed at deserialize; unchanged.

A silently-ignored key used to be the trap (the value you intended had no effect). Now it's loud.
**Fix:** remove or correct the named key, then restart. Note a key is also "unknown" if it's in the
**wrong table** — e.g. a `[tenant_config]` knob placed at top level, or a connection field outside the
`yb_storage_conn` block. The compute spec (`compute_ctl`) is deliberately **exempt** — it's supplied
by the control plane on a separate lifecycle, so it still tolerates unknown fields for version skew.

## L. SC: `Postgres directory 'lib' not found in <dir>/pg_install`
`yb_light_local init` bakes `pg_distrib_dir` into the generated repo config. The default is
`$POSTGRES_DISTRIB_DIR` if set, otherwise `<cwd-at-init-time>/pg_install` (`local_env.rs`). If you
init from a directory that has no `pg_install` (e.g. `/home/ec2-user`), SC startup fails immediately
because the Postgres install tree isn't there.

Fix — point it at the build's pg_install, either before init or by patching the generated config:
```bash
# Cleanest: set this before `yb_light_local init` so the config is written correctly
export POSTGRES_DISTRIB_DIR=$BUILD/pg_install

# Or patch the already-generated config:
CFG=$YB_LIGHT_REPO_DIR/config
sed -i "s|^pg_distrib_dir = .*|pg_distrib_dir = \"$BUILD/pg_install\"|" "$CFG"
```
The companion `yb_light_distrib_dir = "$BUILD"` should already be correct.

## M. SC: `password missing` / `building database URL from --yb-config`
`yb_light_local init` writes the YB connection blocks (`[storage_controller.yb]` and the per-template
`yb_storage_conn = '{…}'` inline tables) with `password_env: None` and `sslmode: None`
(`yb_light_local.rs`) — only `host_ports/user/dbname`. SC then can't authenticate, and YB rejects
non-TLS connections anyway (see **D**).

Fix: edit the generated `$YB_LIGHT_REPO_DIR/config` and add both keys to every YB block:
```bash
CFG=$YB_LIGHT_REPO_DIR/config
# [storage_controller.yb] block (TOML section)
sed -i '/^\[storage_controller\.yb\]/,/^\[/ {
  /^dbname = /a\
password_env = "YB_STORAGE_PASSWORD"\
sslmode = "require"
}' "$CFG"
# Inline yb_storage_conn = '{ … }' on PS/SK templates
sed -i 's|\(yb_storage_conn = .{ host_ports = "[^"]*", user = "[^"]*", dbname = "[^"]*"\) }\(.*\)|\1, password_env = "YB_STORAGE_PASSWORD", sslmode = "require" }\2|' "$CFG"
```
Then restart everything (or just don't `yb_light_local start` until the patch is in).

## N. `pageserver.toml` top-level knob placed after a `[section]` header
TOML sections are open-ended: once a `[section]` header appears, every subsequent key belongs to that
section until the next header. So a top-level knob (e.g. `scale_factor`, `page_cache_size`) appended
**after** `[timeline_init]` becomes `timeline_init.scale_factor` — an unknown key in the wrong table.
The PS then **refuses to start** with
`unknown configuration item(s) in pageserver.toml: timeline_init.scale_factor` (see **K**).

Fix: place top-level keys **before** any `[…]` section, e.g.
```toml
# top-level keys here
scale_factor    = 6.0
page_cache_size = 8192

[timeline_init]   # section keys after here
mode = "bootstrap"
pg_version = 17
```
Then `yb_light_local pageserver restart --id 1` and confirm the PS starts clean (no `unknown
configuration item` rejection, no `ignoring unknown` warning). See throughput Step 3a for the
`scale_factor` tuning this most often bites.

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
  -tAc "select 1; show yb_amp.pageserver_connstring;"
```
If `yb_amp.pageserver_connstring` doesn't point at the PS you expect, the compute is talking to the
wrong pageserver — re-check branch placement (Step E/F) before anything else.

## O. PS/SK panic at startup: `OS can't spawn worker thread: Resource temporarily unavailable`

`pthread_create` returned EAGAIN — the box hit the per-user process/thread
limit (`ulimit -u`, and/or the user slice's cgroup `pids.max`), so tokio (or
the tsclient RPC reactors) couldn't spawn a worker. Seen when many endpoint
PS+SK pairs run on one host: with `hot_path = "tsclient"` each daemon also
opens pggate RPC connections to the tserver RPC port, adding worker/reactor
threads per daemon (daemons sharing a process share one client).

Check and raise:
```sh
ulimit -u                                   # per-user limit in this shell
ps -eLf | grep -c $USER                     # live thread count for the user
ps -eo pid,nlwp,comm --sort=-nlwp | head    # top per-process consumers
```
Raise via `/etc/security/limits.d/` (`<user> soft nproc 8192`) or the systemd
user slice (`TasksMax=`). Rule of thumb per tsclient-mode endpoint pair:
~90 threads (PS ~55, SK ~36) plus the compute's postgres processes.
