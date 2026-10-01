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
   deployments must write the identity before spawning. The process reads
   `amp_storage.branch_state` at startup and will refuse to start if that row is
   missing (the controller provisions it at branch create). How to create the
   branch comes from the same row, not the toml.
   Checkpoint once started: `curl http://$PS_HOST:$PS_HTTP_PORT/v1/status` returns OK.
4. **Register SK with the SC** (see "Manual registration"). The pageserver needs no
   registration step — it reads its branch row on every start.
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
| PS/SK: `column "version" does not exist`, or a thin-lane write rejected on its key columns | `amp_storage` predates the `version` key column and the in-place edit of `0003` never re-ran | **B2** |
| SK: `unexpected argument '--<flag>'` | flag removed/renamed in this build | **C** |
| PS/SK/compute_ctl: `no pg_hba.conf entry … no encryption` | client connected to YB without TLS | **D** |
| SC node list empty after PS starts; PS log `register: None` | PS didn't auto-register | **E** |
| PS exits at startup: `pageserver.toml must set branch_id` | single-branch identity missing — launcher hasn't assigned one (run `cluster create` / `branch create`, or write the identity into pageserver.toml) | — |
| PS exits at startup: `timeline … has no branch_state row` | the controller never provisioned the branch (wiped YB DB, or the pageserver started without `branch create`) | — |
| SK exits at startup: `YB-WAL mode requires --branch-id (single-branch-per-process)` | single-branch identity missing from `safekeeper.toml` — the SK serves one branch and scopes its load to it. Provisioned SKs get `branch_id` written by `allocate_safekeeper_conf`; a hand-written `safekeeper.toml` must set it alongside `yb_storage_conn` | — |
| SK exits at startup: `thin-client hot path init` … `Service yb.tserver.ThinClientService not registered on TabletServer` | the backing YugabyteDB universe is older than 2026.1.2, the first release with the thin-client RPC service amp's shim speaks. Every endpoint on it crash-loops. Upgrade the universe to 2026.1.2 or later | — |
| Timeline create: `couldn't find 1 safekeepers in different AZs (found: 0)` | SK not registered with SC | **F** |
| psql: `libreadline.so.7: cannot open shared object file` | bundle psql wants older readline | **G** |
| Compute postmaster dies with `EADDRINUSE` on its own port | yb_amp_local port collision, or a postmaster orphaned by an earlier compute_ctl | **H** |
| Fresh `cloud_admin` can't auth over socket / password rejected | role needs socket-trust password set | **I** |
| PS can't find the safekeeper (broker-less builds) | missing `safekeeper_pg_addr` | **J** |
| Any process exits at startup: `unknown configuration item(s) …` / `unknown field …` | misspelled, stale, or misplaced config key — rejected, not ignored | **K** |
| SC: `Postgres directory 'lib' not found in <dir>/pg_install` | init baked a `pg_distrib_dir` that doesn't exist on this box | **L** |
| SC connects to YB and fails: `password missing` / `building database URL from --yb-config` | init omits `password_env`/`sslmode` from the YB conn blocks | **M** |
| `pageserver.toml` top-level knob rejected at startup as `<section>.<key>` | a top-level key was placed **after** a `[section]` header | **N** |
| PS/SK panics at startup: `OS can't spawn worker thread: Resource temporarily unavailable` | per-user process/thread limit (`ulimit -u` / cgroup `pids.max`) exhausted | **O** |
| SK: `failed to load timeline` / `end-of-WAL record-boundary scan failed`; compute reconnects in a loop and the PS can't stream | the safekeeper won't trust a scan it couldn't finish, so it fails the load rather than delete or misreport durable WAL. Deterministic causes (unknown codec tag, anchor bail) keep the branch down until fixed | **P** |
| SK: `safekeeper_timelines` is 0, no error logged, walproposer and PS stuck `connecting`; writes block on `SyncRep` | `load_timeline` is blocked on a YB statement that never returns — no error, so nothing retries | **Q** |
| Compute postmaster exits at startup: `FATAL: could not access file "<lib>"` | `shared_preload_libraries` names a library the endpoint image does not ship, usually a chart newer than the image | **S** |

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

## B2. Migration: "no migration found for version N"
The database is at a migration this controller build does not carry. Two causes, told
apart by the number and by whether `amp_registry.controller_settings` exists (migration
`0009`):
```bash
PGPASSWORD="$YB_STORAGE_PASSWORD" $PSQL \
  "host=$YB_HOST port=$YB_PORT user=yugabyte dbname=$YB_DB sslmode=require" \
  -c "SELECT version, dirty FROM controller.schema_migrations;" \
  -c "SELECT to_regclass('amp_registry.controller_settings');"
```
- **A controller rolled back below a migration it does not carry** — the current
  numbering, with `controller_settings` present at 9 or above. The data is fine; the build
  is too old for it. **Roll forward** to a build that carries the migration. To stay on the
  older build instead, run the newer migrations' `.down.sql` in reverse order first (for
  `0009` that drops only the fleet version, which the controller reseeds) and set
  `schema_migrations.version` to match.
- **The old numbering.** The twenty migrations were folded into eight and renumbered
  ([#1490](https://github.com/yugabyte/meko/pull/1490)), and golang-migrate records only a
  version number, so a database migrated before that carries a number from the old scheme:
  above the current highest (an up-to-date old database is at 20), or 9 with no
  `controller_settings`. The controller refuses to start, or — at or below the current
  highest — starts taking the database as already migrated with a schema that may not
  match. Either way the only fix is to **drop and recreate the controller's database**.
  That database also holds `amp_storage`, so every branch on it goes too.

Do not stamp the version to 8 instead. `0002` and `0003` were edited in place after they
shipped, so a long-lived database can carry an older form of them that no version number
reveals. The latest such edit added a `version` key column to `pages`, `slru_pages`, `meta`,
`pending_gc` and `wals` ([#1495](https://github.com/yugabyte/meko/pull/1495)): a database from
before it reads as fully migrated, and the daemons fail their first query on the missing
column. Same fix — a primary key cannot be altered in place.

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

On the **pageserver and safekeeper**, `sslmode` alone is not enough: they open the thin
lane at boot, and the shim has no accept-all verifier, so any TLS `sslmode` without
`sslrootcert` fails at startup with `enables TLS but sslrootcert is unset`. Set both.

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

## H. Compute postmaster dies with EADDRINUSE
Two causes, and the log tells them apart.

**Port collision (phantom).** yb_amp_local can set the endpoint's `internal_http_port` **equal to**
its `pg_port`; `compute_ctl` then binds `*:pg_port` and the postmaster fails to bind the same port.
Fix: ensure the endpoint's `internal_http_port` ≠ `pg_port` (edit the endpoint spec / `.yb_amp`
config), then restart the endpoint.

**An orphaned postmaster still holding the port** ([#898]). An earlier `compute_ctl` died without
reaping its postgres, which keeps serving on `pg_port`; the replacement cannot bind, and each retry
takes a fresh basebackup that cannot help. Two mechanisms now handle this without an operator:
`PR_SET_PDEATHSIG` has the kernel SIGQUIT the postmaster when `compute_ctl` dies, and
`reclaim_pgdata` shuts down any postmaster still running in `pgdata` before the data directory is
rebuilt — look for `postmaster <pid> is still running in <pgdata>` in `compute.log`. If you see the
bind failures **without** that line, the process holding the port is not a postmaster running out of
that pgdata: find it with `ss -lptn 'sport = :<pg_port>'` before killing anything, because the
reclaim deliberately does not signal a process it cannot identify.

[#898]: https://github.com/yugabyte/meko/issues/898

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
PS+SK pairs run on one host: each daemon also
opens ThinClientService RPC connections to the tserver RPC port, adding worker/reactor
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

Read the error chain in `failed to load timeline <branch>` to tell them apart:

| Chain mentions | Cause | Recovery |
|---|---|---|
| `end-of-WAL scan: reading wals` + a YB error | transient query failure | none needed — the next handshake retries and succeeds |
| `decoding stored WAL chunk` / `unknown payload codec tag 0x<NN>` | the row was written by a build whose codec this one doesn't know (version skew), or the payload is damaged | run a safekeeper build that understands the tag — usually roll forward to the version that wrote it. The offending row is named in the chain (`segno=N offset=M`); inspect it with `dump-wals --branch <id>`, which reports each chunk's stored codec |
| `no fresh-record WAL page found within 64 MiB below` | `find_anchor` hit `MAX_ANCHOR_SCAN_BYTES` with no floor to fall back on, which needs `retention_floor` to be `Lsn(0)` — i.e. a branch with WAL rows but no `timeline_start_lsn` and no `consistent_lsn` | check `min(segno)`/`max(segno)` in `wals` against `branch_state`; rows present while both columns are unset means the row set and the branch state disagree, which is the thing to investigate |
| `chunk at <LSN> (+N bytes) does not cover <LSN>` | a `WalSource` handed `find_end_of_wal` a batch that is not contiguous from where the scan had reached. `YbWalSource::contiguous_chunks_from` cuts its run at the first byte it does not hold, so this is a bug in a `WalSource` implementation, not a state the WAL can be in | not an operator-recoverable condition and not a reason to delete rows — the WAL above is intact and the scan simply refused to guess. Escalate with the branch id and the two LSNs from the message |
| `reading WAL page header at <LSN>: WAL hole at <LSN>: no covering chunk` | the anchor walk descended past the lowest surviving `segno`, into WAL retention already trimmed. Fixed in #788 by flooring the walk at `branch_state.consistent_lsn`, which GC never trims below; before that fix the floor was `timeline_start_lsn`, which sits below the retained window | on a build carrying #788 this means `consistent_lsn` itself is below the surviving WAL — compare it with `min(segno) * wal_seg_size` from `wals`. Escalate; deleting rows only removes more WAL |

If a branch must be brought up before the underlying cause is fixed, the only safe
lever is to make the scan succeed — not to delete rows. Escalate rather than
improvise: the acked WAL above the failure point is still there, and any manual
truncation forfeits it permanently.

---

## Q. SK: `safekeeper_timelines` is 0 and `load_timeline` logged nothing further

Distinct from **P**: there is no error. The safekeeper is up, serves HTTP and
accepts TCP on 5454, and never answers on it. `load_timeline` logged its last
line and stopped. Reads keep working from the pageserver; every commit blocks
on `SyncRep` forever, and both the walproposer and the pageserver's
`wal_connection_manager` sit in `connecting` and time out in a loop. CPU is
idle: it is waiting on YB, not working. Cancelling a stuck commit does not
release it; see
[throughput-troubleshooting Step 2](throughput-troubleshooting.md#step-2--commit-durability-latency-ipcsyncrep).

The tell is which `load_timeline` line came last, since the load is a straight
line of YB statements:

| Last line | Waiting on |
|---|---|
| `truncated torn WAL tail to the last valid record boundary` | the torn-tail cleanup delete (`truncate_wals_above`) |
| `rehydrated write_lsn from amp_storage.wals` | on a build carrying #874, **nothing on this path** — the load is done; look at what follows it (below) |
| neither | the end-of-WAL scan (`load_write_lsn` / `find_anchor` / `find_end_of_wal`) |

For that last row, `end-of-WAL scan finished` reports each phase's queries and
elapsed time. Divide one by the other: the two phases fail for opposite reasons,
and so does a slow cluster.

| reading | cause |
|---|---|
| `find_anchor_ms` high, `find_anchor_queries` low | slow round trips — a YB problem, not a WAL one |
| `find_anchor_ms` high, `find_anchor_queries` high | no fresh-record page near the tip, so the walk ran to `retention_floor`. Expected on WAL made of large records; check how far the pageserver's `consistent_lsn` is behind |
| `find_end_of_wal_ms` high, `find_end_of_wal_queries` ≈ 1 per 16 MiB replayed | batching is working; the cluster is slow |
| `find_end_of_wal_ms` high, `find_end_of_wal_queries` far above that | the fetch is back to a query per chunk. `contiguous_chunks_from` cuts its run at the first row that does not continue the last one, so a batch that comes back mis-ordered degrades to roughly one chunk per query — slow, but not wrong |
| `find_end_of_wal_queries` ≈ 1 **and** `valid_end` equal to the anchor in `truncated torn WAL tail` | the opposite and far worse reading: the first batch came back empty and ended the scan, so the tail below `torn_end` was deleted rather than replayed. This was #938 — a mis-seeded covering-chunk lookup. A one-query scan is only healthy when `valid_end` moved |

A load that appeared stuck right after `rehydrated write_lsn from amp_storage.wals`
was the second known instance (#874): the next statement was `min(segno)`
(`load_min_segno`), seeding the GC DELETE bound. It seeks forward from the bottom
of the branch's key range — the span WAL GC has emptied — so DocDB walks every
deleted key to reach the first live one, and YB's counters hide it: 51 s reporting
`Storage Rows Scanned: 63` on a branch that had churned ~260 GB of WAL. The load
then outran the branch-create deadline, the supervisor took that as fatal, and the
endpoint crashlooped. Builds carrying the fix do not run that query at startup at
all, so a load stuck at this line on a current build is a **new** cause.

The known instance (#812) was the first row: before the fix, `truncate_wals_above`
filtered on a computed expression over the key columns, so it scanned the
branch's entire WAL to delete a handful of rows at the tail. On a busy timeline
that never came back. Builds carrying the fix bound that cleanup
(`YB_TORN_TAIL_CLEANUP_MAX`) and log `failed to delete torn WAL tail` on
timeout rather than hanging, so a load stuck at that line on a current build is
a **new** cause, not this one.

Triage:

1. Confirm the shape — `safekeeper_timelines 0` with a live `/metrics`, and no
   `failed to load timeline` anywhere in the log. An error means **P**, not this.
2. Find the statement on the YB side — the load's statements run on the system
   lane:
   `SELECT query, state, xact_start FROM pg_stat_activity WHERE application_name = 'safekeeper:yb_wal_system'`.
   A long-running statement there names the culprit directly. WAL GC uses the same
   lane, so match it against the load's statements above before concluding this is
   a load at all — a `min(segno)` or a `DELETE … segno >= …` well after startup is
   §R.
3. `EXPLAIN` that statement against the branch. `Storage Table Rows Scanned`
   near the branch's whole row count in `wals`, with the filter under
   `Storage Filter:` rather than `Index Cond:`, is the same class of bug.

Restarting the safekeeper (the supervisor, or the pod) clears the state but does
not fix the cause; it recurred on every restart that hit the torn-tail path and
cleared on restarts that didn't.

## R. SK: a long-running WAL GC statement

WAL GC's two statements can take tens of seconds. Both address the span GC has
emptied, where DocDB walks deleted keys that YB's `Storage Rows Scanned` does not
count:

| Statement | What it is |
|---|---|
| `SELECT min(segno) …` | the first removal after a load resolving `delete_lower_bound`. Once per load. 51 s on a branch that had churned ~260 GB of WAL. |
| `DELETE FROM … segno >= $2 AND segno < $3` | the removal itself. Wide only if the resolve above failed (the warning `could not resolve the WAL DELETE lower bound` says so); tight afterwards. |

The removal is a spawned task holding no timeline lock, so a slow statement here
is not by itself an incident.

It becomes one if it never finishes — GC then stops advancing and `wals` grows
without bound. Check `safekeeper_timelines`, the branch's row count trend in
`amp_storage.wals`, and whether the removal task logs `WAL removal` completions.
Before #874 this query ran on the startup load instead, where the same cost took
the branch's endpoint down (§Q).

## S. Compute postmaster exits: `could not access file "<lib>"`
`shared_preload_libraries` is applied at postmaster start, and a name in it whose library is missing
stops the postmaster instead of being skipped. The compute's log (the endpoint container, or
`compute.log` under yb_amp_local) ends with
`FATAL:  could not access file "<lib>": No such file or directory`, just after a
`WARNING:  yb_amp_download_extension_file_http failed` from yb_amp asking compute_ctl's extension
server for the library; that warning is a consequence, not the cause. The supervisor restarts the
compute within its budget, then exits so the kubelet recreates the pod, which repeats the cycle.

**The base list is newer than the image.** The chart's `endpoint.sharedPreloadLibraries` ships in
the controller image, while the `.so` files ship in the endpoint image, so a controller rendering
an endpoint whose `spec.Version` points at an older image fails this way when a library joins the
default list (`pgsentinel` did in [#1446]; `pg_stat_statements` before it). This happens on a
re-render of an endpoint still recorded at the older version, and on a rollback of the endpoint
image alone. Roll the endpoint image out before the controller, and upgrade or roll back the two
together. Tracked in [#1572].

[#1446]: https://github.com/yugabyte/meko/issues/1446
[#1572]: https://github.com/yugabyte/meko/issues/1572
