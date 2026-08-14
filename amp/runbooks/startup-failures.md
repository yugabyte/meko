# Runbook: yb-amp startup failures

A component won't start, won't register, or the compute can't serve queries. This runbook covers the
yb-amp bring-up failure modes by **symptom → cause → fix**. It is topology-agnostic: the compute,
pageserver, safekeeper, and controller may be co-located or on separate boxes. Substitute
the conventions below.

## Conventions (fill these in for your deployment)
```bash
export PS_HOST=...        export PS_HTTP_PORT=...      # pageserver
export SC_BASE=http://<sc-host>:<sc-port>             # controller
export SK_HOST=...        export SK_PG_PORT=...        # safekeeper PG listen
export COMPUTE_HOST=...   export COMPUTE_PORT=...      # yb-amp compute
export YB_HOST=...  export YB_PORT=...  export YB_DB=...   # YugabyteDB backing store
export YB_STORAGE_PASSWORD=...                            # YB password, used by PS/SK/compute_ctl
export PSQL=<path-to>/psql
```
Component config locations vary by deployment but typically:
`pageserver.toml` + `identity.toml` in the PS data dir; the safekeeper/SC start
scripts; the yb_amp_local repo dir (`.yb_amp/config`, `.yb_amp/endpoints/<ep>/` — which
holds that endpoint's `pageserver/` and `safekeeper/`).

---

## Recommended start order
Start bottom-up so each component's dependency is already listening. **Verify at each checkpoint**
before moving on — a silent half-failure here surfaces as a confusing error three steps later.

1. **controller** → checkpoint: `curl $SC_BASE/control/v1/node` returns (even if `[]`).
2. **safekeeper** → checkpoint: its HTTP `/v1/status` (or process up + port listening).
3. **pageserver** — *single-branch model (#16)*: the process refuses to start unless
   `pageserver.toml` carries `branch_id`. On a fresh repo this is
   normal: `yb_amp_local start` prints `Skipping pageserver …: no timeline identity yet` and
   branch creation bakes the identity in and starts the pageserver itself. Manual
   deployments must write the identity before spawning. The generation is not in the
   toml — the process claims it from `amp_storage.branch_state` at startup, and will
   refuse to start if that row is missing (the controller provisions it at branch
   create). How to create the branch comes from the same row, not the toml.
   Checkpoint once started: `curl http://$PS_HOST:$PS_HTTP_PORT/v1/status` returns OK.
4. **Register SK with the SC** (see "Manual registration"). The pageserver needs no
   registration step — it claims its generation itself on every start.
5. **Create the branch** (via `yb_amp_local cluster create` for a new lineage, or
   `branch create --ancestor-branch-id` to fork one; that seeds
   `amp_storage.branch_state`, and the pageserver bootstraps from it — a NULL ancestor means
   initdb + import, a set one means branch off it — then the branch is created on the safekeeper).
6. **Create + start the compute endpoint** → checkpoint: `psql … 'SELECT 1'`.

---

## Symptom index
| Symptom / error text | Cause | Section |
|---|---|---|
| `yb_amp_local init` panics / serde error on the config TOML | init config schema mismatch | **A** |
| SC: `Migration error: no schema has been selected to create in` | backing DB missing the schema | **B** |
| SK: `unexpected argument '--<flag>'` | flag removed/renamed in this build | **C** |
| PS/SK/compute_ctl: `no pg_hba.conf entry … no encryption` | client connected to YB without TLS | **D** |
| SC node list empty after PS starts; PS log `register: None` | PS didn't auto-register | **E** |
| PS exits at startup: `pageserver.toml must set branch_id/generation` | single-branch identity missing — launcher hasn't assigned one (run `cluster create` / `branch create`, or write the identity into pageserver.toml) | — |
| PS exits at startup: `timeline … has no branch_state row` | the controller never provisioned the branch (wiped YB DB, or the pageserver started without `branch create`) | — |
| SK exits at startup: `YB-WAL mode requires --branch-id (single-branch-per-process)` | single-branch identity missing from `safekeeper.toml` — the SK serves one branch and scopes its load to it. Provisioned SKs get `branch_id` written by `allocate_safekeeper_conf`; a hand-written `safekeeper.toml` must set it alongside `yb_storage_conn` | — |
| Timeline create: `couldn't find 1 safekeepers in different AZs (found: 0)` | SK not registered with SC | **F** |
| psql: `libreadline.so.7: cannot open shared object file` | bundle psql wants older readline | **G** |
| Compute postmaster dies with `EADDRINUSE` on its own port | yb_amp_local port collision | **H** |
| Fresh `cloud_admin` can't auth over socket / password rejected | role needs socket-trust password set | **I** |
| PS can't find the safekeeper (broker-less builds) | missing `safekeeper_pg_addr` | **J** |
| Any process exits at startup: `unknown configuration item(s) …` / `unknown field …` | misspelled, stale, or misplaced config key — rejected, not ignored | **K** |
| SC: `Postgres directory 'lib' not found in <dir>/pg_install` | init baked a `pg_distrib_dir` that doesn't exist on this box | **L** |
| SC connects to YB and fails: `password missing` / `building database URL from --yb-config` | init omits `password_env`/`sslmode` from the YB conn blocks | **M** |
| `pageserver.toml` top-level knob rejected at startup as `<section>.<key>` | a top-level key was placed **after** a `[section]` header | **N** |
| PS/SK panics at startup: `OS can't spawn worker thread: Resource temporarily unavailable` | per-user process/thread limit (`ulimit -u` / cgroup `pids.max`) exhausted | **O** |
| SK: `failed to load timeline` / `end-of-WAL record-boundary scan failed`; compute reconnects in a loop and the PS can't stream | the safekeeper won't trust a scan it couldn't finish, so it fails the load rather than delete or misreport durable WAL. Deterministic causes (unknown codec tag, anchor bail) keep the branch down until fixed | **P** |

---

## A. `yb_amp_local init` config schema / serde errors
`yb_amp_local init --config <file>` is strict about its TOML. Common cascade of failures, each fixed
by adding the missing/retyped key:
- `yb_storage_conn` under `[[pageservers]]` must be a **string-encoded inline table**, not a native
  TOML map — i.e. `yb_storage_conn='{ host_ports="…:5433", user="…", dbname="…" }'` (quoted string),
  not `yb_storage_conn={…}`. It carries no `table_name` (each component hardcodes its tables).
- Missing top-level keys it requires: `default_branch_id`, `generate_local_ssl_certs`,
  `control_plane_api`. A config-load failure in `controller/local/config` on init usually means `control_plane_api` is absent.
- The `[controller.yb]` block (the shared `host_ports`/`user`/`dbname` connection
  block) must be present: every component's YB connection is derived from it.

Fix by adding each missing key and re-running init. After a successful init, the generated
`.yb_amp/config` may **drop the `[[pageservers]]` stanza** when the PS is off-box / self-registering —
that's expected, not a failure.

## B. Migration: "no schema has been selected to create in"
A freshly created backing DB has no `amp_registry` schema, and the controller's `search_path` points
at it. `db.Migrate` creates it (on a single pinned connection, so YB's per-backend
catalog cache cannot hide it from the next statement), but if you are driving
`psql` by hand, create it first:
```bash
PGPASSWORD="$YB_STORAGE_PASSWORD" $PSQL \
  "host=$YB_HOST port=$YB_PORT user=yugabyte dbname=$YB_DB sslmode=require" \
  -c "CREATE SCHEMA IF NOT EXISTS controller;"
```
(The SC's `--database-url` typically pins `options=-csearch_path%3Dcontroller`.) Same idea
if an `amp_storage` schema is expected and missing.

## C. Safekeeper: "unexpected argument '--<flag>'"
Build drift removes/renames flags. The safekeeper's whole config surface now lives in
`<datadir>/safekeeper.toml` (written by the launcher; the analogue of `pageserver.toml`), so the
binary accepts only `-D <datadir>` and `--init`. A start script still passing
`--id`/`--listen-pg`/etc. will fail — move those settings into `safekeeper.toml`
(see `compute/architecture/safekeeper.md` "Run / configure"). Earlier drift also removed `--broker-endpoint`
and `--control-file-backend`. Run `<build>/safekeeper --help` and diff against the start script.

## D. "no pg_hba.conf entry … no encryption" (TLS to YB)
A client opened a **non-TLS** connection to YugabyteDB, which requires TLS. Seen when the
a `YbStore` config derived `sslmode:None`. Fix the relevant config to force TLS and
supply the password env — add `"sslmode":"require"` and
`"password_env":"YB_STORAGE_PASSWORD"` to the connection block. Any component talking to YB
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
yb_amp_local can set the endpoint's `internal_http_port` **equal to** its `pg_port`; `compute_ctl`
then binds `*:pg_port` and the postmaster fails to bind the same port (looks like a phantom
`EADDRINUSE`). Fix: ensure the endpoint's `internal_http_port` ≠ `pg_port` (edit the endpoint spec /
`.yb_amp` config), then restart the endpoint.

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
Config is **fail-fast across every process** (pageserver, safekeeper, controller,
and the `yb_amp_local` on-disk config): a config item that is wrong is
refused at startup rather than logged and ignored. The classes that abort start-up:

- **Unknown / misspelled / stale key** — e.g. `unknown configuration item(s) in pageserver.toml:
  yb_page_writer.scale_factor`, or serde `unknown field \`foo\``. The pageserver names the offending
  paths; the safekeeper / `--yb-config` (`YbConnConfig`) report the serde field.
- **Wrong type / out-of-range value / duplicate key** — already failed at deserialize; unchanged.

A silently-ignored key used to be the trap (the value you intended had no effect). Now it's loud.
**Fix:** remove or correct the named key, then restart. Note a key is also "unknown" if it's in the
**wrong table** — e.g. a `[tenant_config]` knob placed at top level, or a connection field outside the
`yb_storage_conn` block. The compute spec (`compute_ctl`) is deliberately **exempt** — it's supplied
by the control plane on a separate lifecycle, so it still tolerates unknown fields for version skew.

## L. SC: `Postgres directory 'lib' not found in <dir>/pg_install`
`yb_amp_local init` bakes `pg_distrib_dir` into the generated repo config. The default is
`$POSTGRES_DISTRIB_DIR` if set, otherwise `<cwd-at-init-time>/pg_install` (`local_env.rs`). If you
init from a directory that has no `pg_install` (e.g. `/home/ec2-user`), SC startup fails immediately
because the Postgres install tree isn't there.

Fix — point it at the build's pg_install, either before init or by patching the generated config:
```bash
# Cleanest: set this before `yb_amp_local init` so the config is written correctly
export POSTGRES_DISTRIB_DIR=$BUILD/pg_install

# Or patch the already-generated config:
CFG=$YB_AMP_REPO_DIR/config
sed -i "s|^pg_distrib_dir = .*|pg_distrib_dir = \"$BUILD/pg_install\"|" "$CFG"
```
The companion `yb_amp_distrib_dir = "$BUILD"` should already be correct.

## M. SC: `password missing` / `building database URL from --yb-config`
`yb_amp_local init` writes the YB connection blocks (`[controller.yb]` and the per-template
`yb_storage_conn = '{…}'` inline tables) with `password_env: None` and `sslmode: None`
(`controller/local/launch`, the `init` command) — only `host_ports/user/dbname`. SC then can't authenticate, and YB rejects
non-TLS connections anyway (see **D**).

Fix: edit the generated `$YB_AMP_REPO_DIR/config` and add both keys to every YB block:
```bash
CFG=$YB_AMP_REPO_DIR/config
# [controller.yb] block (TOML section)
sed -i '/^\[controller\.yb\]/,/^\[/ {
  /^dbname = /a\
password_env = "YB_STORAGE_PASSWORD"\
sslmode = "require"
}' "$CFG"
# Inline yb_storage_conn = '{ … }' on PS/SK templates
sed -i 's|\(yb_storage_conn = .{ host_ports = "[^"]*", user = "[^"]*", dbname = "[^"]*"\) }\(.*\)|\1, password_env = "YB_STORAGE_PASSWORD", sslmode = "require" }\2|' "$CFG"
```
Then restart everything (or just don't `yb_amp_local start` until the patch is in).

## N. `pageserver.toml` top-level knob placed after a `[section]` header
TOML sections are open-ended: once a `[section]` header appears, every subsequent key belongs to that
section until the next header. So a top-level knob (e.g. `scale_factor`, `page_cache_size`) appended
**after** `[yb_page_writer]` becomes `yb_page_writer.scale_factor` — an unknown key in the wrong table.
The PS then **refuses to start** with
`unknown configuration item(s) in pageserver.toml: yb_page_writer.scale_factor` (see **K**).

Fix: place top-level keys **before** any `[…]` section, e.g.
```toml
# top-level keys here
scale_factor    = 6.0
page_cache_size = 8192

[yb_page_writer]   # section keys after here
```
Then `yb_amp_local pageserver restart --id 1` and confirm the PS starts clean (no `unknown
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
the direct-RPC reactors) couldn't spawn a worker. Seen when many endpoint
PS+SK pairs run on one host: with `hot_path = "yb_thin_client"` each daemon also
opens pggate RPC connections to the tserver RPC port, adding worker/reactor
threads per daemon (daemons sharing a process share one client). The pool
packs `tsclient_sessions_per_conn` server sessions (default 4) onto each TCP
connection and opens
`ceil((read_sessions + write_sessions) / sessions_per_conn)` connections, so
the connection/thread cost scales with the session counts / packing density
— NOT one-per-session. To cut connections without losing much concurrency,
raise `tsclient_sessions_per_conn`; to cut them harder, lower
`tsclient_read_sessions` / `tsclient_write_sessions`. Each daemon logs the
effective counts in its `thin-client hot path ready …` line.

Check and raise:
```sh
ulimit -u                                   # per-user limit in this shell
ps -eLf | grep -c $USER                     # live thread count for the user
ps -eo pid,nlwp,comm --sort=-nlwp | head    # top per-process consumers
```
Raise via `/etc/security/limits.d/` (`<user> soft nproc 8192`) or the systemd
user slice (`TasksMax=`). Rule of thumb per direct-RPC-mode endpoint pair:
~90 threads (PS ~55, SK ~36) plus the compute's postgres processes.

## P. SK: `end-of-WAL record-boundary scan failed` — branch never loads

The safekeeper refuses to load a timeline whose end-of-WAL scan it could not
complete, because both ways of guessing are unsafe: truncating to the boundary
reached so far deletes durable rows whose commits were already acked (#756), and
advertising the raw max offset publishes a `flush_lsn` that may sit mid-record,
where every `wait_lsn` hangs (#678). **This is deliberate — do not "fix" it by
deleting `wals` rows to make the branch start. That is the data loss the check
exists to prevent.**

Failing closed costs availability when the failure is deterministic. `load_one_timeline`
logs and leaves the slot empty, and only the walproposer handshake can create/load
it — `START_REPLICATION` and `IDENTIFY_SYSTEM` go through `GlobalTimelines::get`,
which never creates. So a scan error that recurs on every attempt leaves the branch
unavailable: the compute reconnects in a loop and the pageserver cannot stream WAL
that is sitting durable in `wals`.

Read the error chain in `failed to load timeline <branch>` to tell the three apart:

| Chain mentions | Cause | Recovery |
|---|---|---|
| `end-of-WAL scan: reading wals` + a YB error | transient query failure | none needed — the next handshake retries and succeeds |
| `decoding stored WAL chunk` / `unknown payload codec tag 0x<NN>` | the row was written by a build whose codec this one doesn't know (version skew), or the payload is damaged | run a safekeeper build that understands the tag — usually roll forward to the version that wrote it. The offending row is named in the chain (`segno=N offset=M`); inspect it with `dump-wals --branch <id>`, which reports each chunk's stored codec |
| `no fresh-record WAL page found within 64 MiB below` | `find_anchor` hit `MAX_ANCHOR_SCAN_BYTES` — an unusually large record, or `max_offset` points outside the persisted range | check `min(segno)`/`max(segno)` in `wals` against `branch_state.consistent_lsn`; a `max_offset` far above the stored rows means the row set and the branch state disagree, which is the thing to investigate |
| `reading WAL page header at <LSN>: WAL hole at <LSN>: no covering chunk` | the anchor walk descended past the lowest surviving `segno`, into WAL that retention has already trimmed. Its floor is `timeline_start_lsn`, which sits below the retained window, so a long run of continuation pages (one large record) can push the probe into segments that are legitimately gone | compare the LSN in the message with `min(segno) * wal_seg_size` from `wals`. Below it, no operator action helps — the walk needs flooring at the lowest surviving chunk rather than `timeline_start_lsn`, which is a code fix. Escalate; deleting rows only removes more WAL |

If a branch must be brought up before the underlying cause is fixed, the only safe
lever is to make the scan succeed — not to delete rows. Escalate rather than
improvise: the acked WAL above the failure point is still there, and any manual
truncation forfeits it permanently.
