# `yb_tsclient` design: pure-Rust client for `yb.tserver.PgClientService`

A direct-to-tserver YugabyteDB client speaking the pggate RPC protocol,
with no native-library dependency: connect config, Arc'd `TableHandle`,
`upsert_batch`, `RangeRead` builder, and the `ysql_client`-style error
taxonomy. It is the default non-YSQL hot-path transport
(`hot_path = "tsclient"`) for the safekeeper and pageserver.

Everything below was verified empirically against a live YB 2025.2.3.0
dev cluster and/or read from the authoritative server code (file
references are into the YB source tree; the serial-number findings from
`pg_client_session.cc`). This document supersedes the M0 SPIKE-NOTES.

## Wire protocol

- **Preamble**: the client sends exactly 3 bytes, `"YB\x01"`
  (`kConnectionHeaderBytes`, `yb/rpc/yb_rpc.cc`). That is all the
  negotiation there is: `YBInboundConnectionContext::ProcessCalls`
  strips the preamble and goes straight to parsing calls. No
  connection-context call (Kudu's call_id -3) exists in YB.
- **Framing** (`yb/rpc/serialization.cc`):
  `[u32 BE total_len][varint header_len][RequestHeader][varint
  msg_len][msg pb][sidecars]`, where `total_len` excludes its own 4
  bytes and `msg_len` covers message pb *plus* sidecar bytes. Responses
  are identical with `ResponseHeader`.
- **Sidecar offset base**: the *last `msg_len` bytes* of the frame are
  pb + sidecars; `sidecar_offsets[i]` are relative to the start of the
  message pb. With sidecars present the response pb is
  `entire[..offsets[0]]`, sidecar `i` spans `offsets[i]..offsets[i+1]`,
  last one to the end. Row data returns as a `pg_doc_data` sidecar
  (`rows_data_sidecar` names its index).
- **Errors**: `ResponseHeader.is_error` → body is `ErrorStatusPB`.
- CRC fields exist in both headers but are optional; the server never
  requires them.

Implementation: `src/framing.rs` (pure codec) + `src/connection.rs`
(socket + demultiplexer).

## Connection & multiplexing

One TCP connection carries everything. `Connection` splits the stream:

- a writer task drains an mpsc of encoded frames (no interleaved
  partial writes between concurrent callers);
- a reader/demux task decodes each response frame and completes the
  in-flight call's oneshot, matched by `call_id`. Responses may arrive
  in any order.

Per-call timeouts abandon the waiter (a late response for an unknown
call_id is dropped); any transport failure — connect/read/write error,
EOF, corrupt frame — closes the connection and fails every in-flight
and future call with a `Network`-class error, which triggers session
recovery in the layer above.

## Service, session, lifecycle

- Full service name `yb.tserver.PgClientService`, methods `Heartbeat`,
  `OpenTable`, `Perform`, registered on the **tserver RPC port
  (9100)**.
- `Heartbeat{session_id: 0}` creates a server-side session and returns
  its id; `Heartbeat{session_id: N}` keeps it alive. Sessions expire
  after `pg_client_session_expiration_ms` (60s default); the client
  heartbeats every 10s from a background task.
- A lost session surfaces as `InvalidArgument "Connection terminated
  unexpectedly due to unknown session N"` (observed live). It is
  classified like a connection drop; recovery reconnects (walking the
  endpoint list), opens a fresh session, and restarts the per-session
  counters. It does **not** re-open cached tables — the hot tables'
  schema is immutable (see below), so the cached `TableHandle` stays
  valid across a reconnect and `Perform` uses it directly. Recovery is
  epoch-guarded: concurrent failures on the same session produce
  exactly one recovery.
- **`PgPerformRequestPB.serial_no`**: the session queues Performs by
  strictly increasing serial ("Too old request" on reuse). The client
  allocates the serial and enqueues the frame under one lock, so wire
  order always matches serial order; a fresh session restarts at 0.

Endpoint selection: the config takes a list of tserver `host:port`s and
an optional preferred address. The list is reordered once at
construction so the preferred address (else the first loopback address,
else the original first entry) leads — that lead endpoint is what
`connected_addr()`/`session_id()` report and where recovery re-validates
tables. Every endpoint still carries traffic (see the pool below);
ordering only decides which endpoint each positional session prefers and
the fallback order when a session must fail over.

### Session pool — spread across all tservers

The pool holds sessions distributed across EVERY tserver endpoint, and
both reads and writes round-robin over them so no single tserver takes
all the traffic:

- **one read session per endpoint.** Reads, `OpenTable`, and the first
  page of every scan pick a read session round-robin, so single-page
  reads — the getpage hot path — fan out over all nodes.
- **`TsClientConfig::write_sessions` write sessions** (default 0),
  distributed round-robin across the endpoints at establish time, each
  its own TCP connection + server session; `upsert_batch` round-robins
  over them (falling back to the read sessions when 0). Writes are
  stateless per Perform (fresh `txn_serial_no`, no continuations, no
  read point), so any session on any node serves any write.

Each session is its own `Connection` to a specific endpoint index: read
session `i` (and write session `i mod n`) targets endpoint `i`, failing
over to the next only if that node is down at open time.

**Paged scans pin to their session.** The server keeps read-point
history per session, so a scan's continuations must return to the
session that pinned page 1's snapshot (a continuation on any other
session would silently re-read at a fresh read time — the paging
envelope carries the originating server-session id and the client
rejects a mismatch loudly). So while the FIRST page round-robins, every
continuation re-pins to that exact read session (looked up by session id
in the pool). Single-page reads — no continuation — round-robin freely;
that is the win. On a recovered pool the pinned session id is gone, so
the continuation surfaces a read-restart (`YbError::is_read_restart`) and
`read_scan` restarts the whole scan on a fresh read point (see below).

Why spread writes at all (the earlier M4 bulk-ingest finding still
holds): concurrent multi-megabyte write Performs on ONE connection
barely overlap — the frame encode/enqueue serializes under the client's
perform lock, the server reads and protobuf-decodes all of that
connection's frames on one reactor thread, and Perform starts are
sequenced per session — and reads sharing the connection queue behind
the write frames. Spreading over sessions AND nodes fixes both the
serialization and the single-tserver hotspot (a TPC-C parity run showed
one tserver saturated at 0.7% idle while the other two sat idle when the
pool pinned everything to one node).

The pool is one failure unit: every session shares one epoch, so a
Network-class failure on any session recovers the WHOLE pool (heartbeats
keep every session alive and trigger eager recovery between calls). But
each new session prefers its predecessor's endpoint index, so a recovery
triggered by one dead node keeps the healthy nodes' sessions on those
nodes and the pool re-spreads as nodes return.

### Idle-connection closing & reopen-without-OpenTable

Keeping every session warm forever means a branch that goes quiet still
holds a fan of tserver TCP connections + server-side sessions and pays
10s heartbeat chatter on each — fleet-wide that adds up (one pageserver
was seen holding 131 connections to a single tserver). So each session's
connection is **re-establishable**: it lives behind a per-session
`tokio::sync::Mutex<ConnState>` that is either `Warm(Connection)` or
`Cold`, and the client tracks each session's `last_used` timestamp
(refreshed on every Perform and every keep-alive).

- **Config knob**: `TsClientConfig::idle_timeout: Option<Duration>`,
  default `Some(60s)`. `None` = never close = the previous always-warm
  behavior.
- **Idle sweep**: the heartbeat task, each tick, per session — if `Cold`,
  skip; if `Warm` and idle ≥ `idle_timeout`, transition `Warm → Cold`
  (drop the `Connection`, which aborts its reader/writer and closes the
  socket; the server-side session then lapses on its own expiry) instead
  of heartbeating; if `Warm` and recently used, heartbeat as before.
- **Reopen on use**: before a `Perform` on a `Cold` session, the caller
  takes the session lock and re-establishes via the single
  `establish_session_conn` primitive — **connect + `Heartbeat` only, NO
  `OpenTable`**. The fresh server session id replaces the old
  (`SessionMeta::reopened_with`, restarting Perform serials at 0). If
  another task already re-warmed it, the caller just reuses that
  connection. The async mutex funnels a concurrent burst so exactly one
  task reconnects (no thundering herd), and it is the same lock the
  sweeper takes to go `Warm → Cold`, so the sweeper can never close a
  connection out from under an in-flight caller (and re-checks idle under
  the lock — a caller that just touched `last_used` is no longer idle).

**Why reopen skips OpenTable (schema-immutable rationale).** The hot
tables (`pageserver.{pages,slru_pages,meta}`, `safekeeper.wals`) are
created once and never altered, so their `schema_version` is immutable.
The cached `TableHandle` (DocDB table id + schema version + column ids)
therefore stays valid across any reconnect, and `Perform` uses it
directly — a reopen (and, now, a recovery) needs only a fresh server
session id, which the `Heartbeat` returns. The old recovery path re-ran
`OpenTable` on every cached table purely to refresh an unchanging schema;
that loop is gone. `establish_session_conn` is the one primitive shared
by initial pool build, recovery, and idle-reopen.

**Observability**: an idle-reopen fires the same
`ClientObserver::on_session_recovery` hook as a pool recovery (both mint
a fresh server session). The two are not distinguished on the counter; a
reopen under a short idle timeout is expected background churn, not a
fault.

**Paged-scan interaction**: a `Cold`-then-reopened session gets a NEW
session id, so the server's read-point history for the old session is
gone. An *unpinned* paged continuation is bound to its originating
session (its snapshot lives only in that session's history), so the
client refuses to reopen a Cold session for such a continuation and
surfaces a read-restart (`ReadRestart`, `is_read_restart`) — the same
signal a mid-scan `restart_read_time` produces — so `read_scan` restarts
the whole scan on a fresh read point rather than silently serving a
fresh (wrong) snapshot. This is safe in practice because scans issue
pages back-to-back with no idle gap, so `idle_timeout` won't fire
mid-scan; the guard is a correctness backstop, not a hot path. (An
*explicitly* pinned read carries its own `read_time` and is restorable on
any session, so it may reopen freely.) Regressions:
`idle_close_then_reopen_without_opentable`,
`concurrent_burst_reopens_cold_session_once` (unit, fake tserver),
`idle_close_then_reopen_without_opentable` (live, incl. the guard).

## Routing: the client computes partition keys

The server-side `YBPgsqlWriteOp/ReadOp::GetPartitionKey` only *reads*
`request.partition_key` for ops that arrive via Perform
(`own_request_ == false`, `yb/client/yb_op.cc`); the batcher otherwise
fails with `IllegalState: Hash partition key is empty`. Required per op
on hash-partitioned tables:

- `hash_code` = 16-bit YB hash of the hash-column values;
- `partition_key` = those 2 bytes big-endian
  (`EncodeMultiColumnHashValue`).

The hash is Jenkins `Hash64StringWithSeed(compound, seed=97)` folded to
16 bits (`PartitionSchema::HashColumnCompoundValue`), where `compound`
concatenates each hash value in `AppendToKey` form
(`yb/common/ql_value.cc`): raw big-endian for ints/bool, raw bytes for
string/binary — *not* DocDB key encoding. Implemented in
`src/partition.rs`; unit tests pin golden vectors generated with YSQL's
`yb_hash_code()` (which computes the same function).

Scan continuations route with `partition_key =
paging_state.next_partition_key` (the server rewrites it only for
own-request ops, `InitHashPartitionKey`). Paging crosses tablet
boundaries correctly. Full scans start with an empty partition key.

Range-partitioned and colocated tables are out of scope (they need
DocDB key encoding client-side); only hash-partitioned tables are
supported, which covers all yb-amp hot tables.

## Reads, snapshots, and the Perform serial numbers

This is where multiplexing concurrent Performs over one session leaves
pggate's design envelope (pggate runs one statement at a time per
session). Verified against `pg_client_session.cc`:

1. **`txn_serial_no` must be fresh per Perform.** The plain session
   arms a "pending used read time" whenever a Perform starts with no
   read point set; a second Perform arriving with the *same*
   `txn_serial_no` while the first is still in flight fails with
   `IllegalState: Expecting a used read time from the previous RPC but
   didn't find one` (`CheckPlainSessionPendingUsedReadTime`). A
   different `txn_serial_no` is the documented escape hatch.

2. **The server never returns its chosen read time on this path.**
   `pg_client_session.cc` deliberately clears
   `paging_state.read_time` from every non-catalog Perform response
   ("Clear read time for the next page here unless absolutely
   necessary"), and `used_in_txn_limit_ht` is transaction-only. Any
   design that captures the used read time client-side and re-sends it
   via `options.read_time` is therefore impossible; pinning must run
   server-side.

3. **Server-side pinning is keyed by `read_time_serial_no`.** Each
   Perform's `options.read_time_serial_no` is compared to the session's
   current one; on change the read point resets, and once a read point
   is picked it is saved into the session's `read_point_history_` keyed
   by that serial. A later Perform with the same serial *restores* the
   exact read point (`ReadPointHistory::Restore`) even if other
   Performs interleaved.

The client therefore issues:

- **Reads**: fresh `read_time_serial_no` per logical scan plus
  `read_time_manipulation: ENSURE_READ_TIME_IS_SET`, which makes the
  proxy pick the read time at setup — before the flush — and save it
  under our serial. No pending-used-read-time is armed (the read point
  is already set), and continuations that carry the same serial restore
  the same snapshot. The scan's serial (and explicit pin, if any)
  travels inside the opaque `paging_state` bytes the client hands back
  (`PagingEnvelope`), so `read_page` continuations stay on their
  snapshot with no caller effort.

  The proxy-picked read point is **clamped**
  (`options.clamp_uncertainty_window`, the server's
  `yb_read_after_commit_visibility = relaxed` semantics): read ==
  global limit, no clock-skew uncertainty window. Unclamped, the
  window (`read .. read + max_clock_skew`, 500ms default) stays open
  for the WHOLE scan — a row committed into the scanned range while a
  paged scan runs fails a later continuation with `TryAgainCode
  restart_read_time`, and a continuation is pinned to its scan's
  snapshot, so no in-place retry can ever heal it; restarting the scan
  from scratch under sustained writes hits the same restart every
  cycle (this livelock is how chain reads over hot `pageserver.pages`
  blocks "never completed" in the field). The trade: commits acked
  through ANOTHER tserver in the last `max_clock_skew` may be
  invisible to a fresh scan. Writes acked through THIS client are
  always visible (hybrid-time propagation keeps the proxy's clock ≥
  every commit it acked), and explicitly pinned reads were already
  clamped (`read_ht == global_limit_ht`), so scans stay internally
  snapshot-consistent either way. Regression:
  `unpinned_reads_clamp_the_uncertainty_window` (unit),
  `hot_block_chain_reads_complete_under_writes` (live).

  A second guard rail: a continuation whose response reports the exact
  scan position it was sent (`next_partition_key` + `next_row_key`)
  can never advance; `do_read_page` fails it loudly (fatal, not
  retried) instead of letting the caller's paging loop spin silently.
  Regression: `stuck_continuation_fails_loudly`.

  The fresh serial is per *attempt*, not per logical call:
  `ENSURE_READ_TIME_IS_SET` only picks a new read time when the serial
  changes, so a retry that reuses its failed attempt's serial re-reads
  at the exact read time that just failed. For a read-restart
  (`TryAgainCode restart_read_time` / `PGSQL_STATUS_RESTART_REQUIRED_
  ERROR` — routine on multi-node clusters, where writes commit inside
  the uncertainty window above a proxy-picked read time) such a retry
  fails identically every time and the whole retry budget burns down
  into a persistent read failure. Regression:
  `read_retries_use_a_fresh_read_serial`.

  The envelope also records the server session id that created the
  scan. An unpinned continuation presented on a *different* session
  (after a session recovery) is rejected client-side as fatal: the
  server would not fail it — its `read_point_history_` simply doesn't
  know the serial, so `ENSURE_READ_TIME_IS_SET` would silently pick a
  fresh read time and switch the scan's snapshot mid-flight. Callers
  restart the scan (or fall back) instead of reading a torn cut.
  Regression: `continuation_from_lost_session_fails_loudly`.
- **Explicitly pinned reads** (`RangeRead::read_time_ht`):
  `options.read_time = {read_ht, global_limit_ht}` (mirroring
  `ReadHybridTime::FromUint64`), which sets the read point directly.
  Explicit read time and the manipulation are mutually exclusive on the
  wire.
- **Shared snapshots across reads**: several read ops in ONE Perform
  (`TsClient::read_pages`) — one setup, one read point, every op and
  every continuation shares it (a single snapshot by construction).
- **Writes**: fresh `read_time_serial_no` (forces a clean read-point
  reset, no entanglement with scans), no manipulation, no read time.

History trimming: every Perform sends
`read_time_serial_no_history_min = latest_read_serial − 65536`. A scan
that pages across more than 65k later read serials would silently lose
its pin (the server would pick a fresh read time); our callers' bounded
scans are orders of magnitude below that.

Read requests bind `targets[]` as column-id expressions plus matching
`col_refs[]`, `is_forward_scan`, `limit`, `return_paging_state`, and
range-column conditions as a `QL_OP_AND` condition tree
(`condition_expr`). Hash-eq reads carry
`partition_column_values` + computed `hash_code`/`partition_key`.

## Writes

`PGSQL_UPSERT` ops (`stmt_type`, `client: YQL_CLIENT_PGSQL`,
`table_id`, `schema_version`, client-unique `stmt_id`), hash key values
as bare `partition_column_values`, range keys as
`range_column_values`, non-key columns as `column_values{column_id,
expr.value}`. Omitting `ysql_catalog_version` skips catalog-version
checks. One Perform per `upsert_batch`.

**Atomicity**: ops that share a partition key land on one tablet and
the tserver session flushes them as one write batch → one Raft entry →
one commit hybrid time. A concurrent snapshot reader sees all rows of
such a batch or none — verified live (`single_tablet_batch_atomicity`:
a reader polling during 60 batched writes only ever observed multiples
of the batch size). Rows spanning partition keys may commit per tablet,
independently.

Value shapes supported (what the safekeeper/pageserver bind): NULL,
BOOL, INT2/4/8, TEXT (`string_value`), BYTEA (`binary_value`).

## Error taxonomy & retries

Mirrors the `ysql_client` retry classes:

| class            | sources                                                                                    | retry behavior |
|------------------|--------------------------------------------------------------------------------------------|----------------|
| `Network`        | transport errors, RPC timeouts, `FATAL_SERVER_SHUTTING_DOWN`, AppStatus NetworkError/TimedOut/Aborted, **unknown session** | session recovery, then retry |
| `TryAgain`       | AppStatus TryAgain/ServiceUnavailable/Busy/LeaderNotReadyToServe/LeaderHasNoLease/TabletSplit/ShutdownInProgress, `ERROR_SERVER_TOO_BUSY`, `PGSQL_STATUS_RESTART_REQUIRED_ERROR` | retry in place with backoff |
| `SchemaMismatch` | `PGSQL_STATUS_SCHEMA_VERSION_MISMATCH`                                                       | fatal; caller re-opens the table |
| `Invalid`        | InvalidArgument/NotSupported, bad API usage                                                  | fatal |
| `Other`          | everything else                                                                              | fatal |

The retry driver mirrors the `ysql_client` shape and constants: bounded
attempts (default 10, caller-tunable via `upsert_batch_bounded`),
exponential backoff 50ms → 1s cap, 0–25% jitter. Retries are safe by
repo contract: reads are snapshots, writes are idempotent upserts.

Retries are never silent: every failed attempt — including the last,
whose error the caller may absorb into a fallback lane — is WARNed with
its error class. Two health events are additionally surfaced through
[`ClientObserver`] (`TsClient::set_observer`), which the pageserver and
safekeeper wire to `*_session_recoveries_total` /
`*_call_timeouts_total` counters next to their lane-fallback metrics:
session recoveries, and per-attempt call timeouts
(`YbError::is_timeout` — the transport was healthy but the server never
answered within `rpc_timeout`, the signature of a server-side stall).
A field incident where reads stalled for hours with nothing on any
dashboard motivated all three.

## Vendored protos

30 files under `protos/yb/...` (transitive closure of
`pg_client.proto`, plus `rpc_header.proto`), pinned to 2025.2.3.0.
One edit: `default = DEFAULT_TABLE_TYPE` → `default = YQL_TABLE_TYPE`
(same value; prost doesn't emit `allow_alias` alias variants).
Compiled with prost-build + `protoc-bin-vendored`. Note the
`yb/common/*.proto` files declare package `yb`, not `yb.common`.

## API notes & gotchas

- `TableHandle::schema_version` is a method, not a field (it refreshes
  in place on session recovery).
- `ReadPage::used_read_time_ht` is 0 unless the read was explicitly
  pinned — the server withholds its read time (see above). Callers that
  need several reads under one snapshot (the safekeeper's two-arm WAL
  read) use `read_pages` (several reads, one Perform, one snapshot);
  same-scan paging needs nothing. Explicit pinning (`read_time_ht`)
  still works for hybrid times obtained elsewhere (e.g. a server clock).
- `open_table` returns a cached handle for an already-open table.
- Config takes tserver RPC endpoints (9100), not master addresses: the
  master is never contacted at run time (OpenTable resolves through the
  tserver). When the `[*.yb]` block leaves `tserver_rpc_addrs` unset,
  `YbConnConfig::effective_tserver_rpc_addrs` derives the list from
  `master_addrs` (which names ALL cluster nodes) on port 9100 — master
  and tserver are co-located per node in this deployment — rather than
  from the single-YSQL-host `host_ports`, which would pin the whole pool
  to one tserver. Per-node tserver discovery via a master
  `ListTabletServers` RPC is a future robustness improvement; until then
  a split master/tserver topology must set `tserver_rpc_addrs`
  explicitly.

## Out of scope / open items

- Range-partitioned & colocated tables (need DocDB key encoding
  client-side).
- TLS / auth: the plaintext preamble path only; the
  `use_client_to_server_encryption` handshake is unexplored.
- Distributed transactions, `ysql_catalog_version` checking, DDL.
- Follower reads (`options.read_from_followers`) — plumbing exists on
  the wire if ever needed.
- Abandoned `read_page` scans leak one small server-side history entry
  until the trim window passes or the session dies (bounded at 65536
  entries ≈ 4 MB per session worst case).
