# Compute

The compute is a PostgreSQL backend, built from the source tree in
[`compute/vendor`](../vendor), that stores all of its
permanent relation data remotely rather than in local files. It is stateless:
pages are served by a pageserver and WAL streams to a safekeeper, and both
persist everything into YugabyteDB — the underlying store that provides
durability and resilience. A local disk holds only a
working `pgdata` (generated config, a local `pg_wal/` the walproposer
streams from, and catalog files pulled at startup) plus an optional page
cache.

Two pieces make this work:

- The **`yb_amp` extension** ([`compute/pgxn/yb_amp/`](../pgxn/yb_amp)), which supplies a storage manager that reads
  and writes pages over the network, the walproposer that ships WAL, the local
  file cache, and a set of supporting subsystems.
- **`compute_ctl`** ([`compute/compute_tools/`](../compute_tools)), the
  per-endpoint supervisor that generates `postgresql.conf`, initializes
  `pgdata` from a basebackup, launches Postgres, and manages its lifecycle. Its
  spec, lifecycle, and HTTP control API are documented separately in
  [`compute_ctl.md`](compute_ctl.md).

## The `yb_amp` extension

`yb_amp` is defined by
[`yb_amp.control`](../pgxn/yb_amp/yb_amp.control).

The extension is loaded as a preload library (`shared_preload_libraries=yb_amp`)
so that it is present in the postmaster before any backend forks. Its
[`_PG_init`](../pgxn/yb_amp/yb_amp.c) also calls `load_file("$libdir/yb_amp_rmgr")`,
so the companion resource-manager library
([`compute/pgxn/yb_amp_rmgr`](../pgxn/yb_amp_rmgr)) does not need a separate
entry in `shared_preload_libraries`.

### Initialization

A preloaded extension initializes in three stages, and `yb_amp` uses all three
(see the long comment in `_PG_init`):

1. **`_PG_init`** — runs early in postmaster startup, before shared memory
   exists. It defines every extension GUC and initializes each subsystem's
   early state: `pg_init_libpagestore()`, `relsize_hash_init()`, `lfc_init()`,
   `pg_init_walproposer()`, `init_lwlsncache()`, `pg_init_communicator_process()`,
   `pg_init_communicator()`, `InitUnstableExtensionsSupport()`,
   `InitLogicalReplicationMonitor()`, `InitDDLHandler()`, and
   `pg_init_extension_server()`. It also installs
   `Custom_XLogReaderRoutines = YbAmpOnDemandXLogReaderRoutines` and
   `restore_running_xacts_callback = RestoreRunningXactsFromClog`.
2. **`yb_amp_shmem_request_hook`** — after `MaxBackends` is known, reserves shared
   memory for each subsystem: `LfcShmemRequest()`, `YbAmpPerfCountersShmemRequest()`,
   `RelsizeCacheShmemRequest()`, `WalproposerShmemRequest()`, `LwLsnCacheShmemRequest()`.
3. **`yb_amp_shmem_startup_hook`** — allocates that shared memory and its LWLocks,
   and registers the extension's wait events (`YbAmp/FileCache_*`,
   `YbAmp/PS_*`, `YbAmp/WAL_Download`).

`_PG_init` also forces `io_method=sync` via
`SetConfigOption("io_method", "sync", PGC_POSTMASTER, PGC_S_OVERRIDE)`. This is
both a correctness and a performance requirement:

- **Correctness.** The `yb_amp` storage managers are fd-less — pages come over
  the network, not from an open file descriptor. Synchronous reads complete
  through the `fd = -1` sentinel gate in `pgaio_io_perform_synchronously()`. The
  asynchronous methods (`worker`, `io_uring`) dispatch reads to a worker or the
  kernel, which reopens the relation through the smgr's `smgr_fd` callback;
  `yb_amp`'s `smgr_fd` raises an error, so those reads would fail.
- **Performance.** Read concurrency comes from PostgreSQL's `read_stream`
  prefetch (advice) path, which the core only enables when `io_method == sync`.
  That path feeds the compute→pageserver prefetch ring so getpage requests
  pipeline. The non-sync methods drive reads through the AIO path, where each
  getpage is satisfied synchronously and the pipelining collapses.

`PGC_S_OVERRIDE` ensures a user-set `io_method` cannot re-introduce the problem.

### GUCs defined in the extension

`yb_amp.c` defines these (subsystems define more — see the sections below):

| GUC | Context | Default | Purpose |
| --- | --- | --- | --- |
| `yb_amp.disable_logical_replication_subscribers` | SIGHUP | `false` | Disable incoming logical replication |
| `yb_amp.disable_wal_prevlink_checks` | SIGHUP | `false` | Skip prev-link validation of WAL records |
| `yb_amp.monitor_query_exec_time` | USERSET | `false` | Accumulate per-query execution time |
| `yb_amp.allow_replica_misconfig` | POSTMASTER | `true` | Allow a replica to start with some critical GUCs smaller than the primary |
| `yb_amp.running_xacts_overflow_policy` | POSTMASTER | `ignore` | Action on snapshot overflow when restoring running xacts from CLOG (`ignore`/`skip`/`wait`) |
| `yb_amp.pgstat_file_size_limit` | SIGHUP | `1 MB` | Cap on the `pgstat.stat` file WAL-logged on clean shutdown; `0` disables persisting it |
| `yb_amp.debug_compare_local` | POSTMASTER | `none` | Debug mode comparing prefetch-ring / LFC / pageserver pages against local disk |
| `yb_amp.privileged_role_name` | POSTMASTER | `yb_amp_superuser` | Name of the weak-superuser role granted to users |
| `yb_amp.lakebase_mode` | POSTMASTER | `false` | Enable Lakebase-specific behavior (e.g. the SQL-error metrics hook) |
| `databricks.test_hook` | SUSET | `0` | Test-only hook used by SQL regression tests |

### SQL-visible functions

The extension exposes utility functions (declared across
[`yb_amp--*.sql`](../pgxn/yb_amp) and created in every database):
`pg_cluster_size()`, `backpressure_lsns()`, `backpressure_throttling_time()`,
`approximate_working_set_size()` / `approximate_working_set_size_seconds()`,
`yb_amp_get_lfc_stats()`, `local_cache_pages()` (with the `local_cache` view),
and the prewarm functions `get_local_cache_state()`, `prewarm_local_cache()`,
`get_prewarm_info()` (extension version 1.6).

Supporting subsystems initialized alongside these: the **communicator process**
([`communicator_process.c`](../pgxn/yb_amp/communicator_process.c)), a background
worker that owns the pageserver connection and prefetch pipeline; the **relation
size cache** ([`relsize_cache.c`](../pgxn/yb_amp/relsize_cache.c)); the **remote
extension server** ([`extension_server.c`](../pgxn/yb_amp/extension_server.c)),
which asks `compute_ctl` to download extension files on demand; a **DDL handler**
that captures role/database updates for the control plane; and a **logical
replication monitor**.

## Storage manager hooks (`NEON_SMGR`)

PostgreSQL routes all relation-file I/O through a pluggable storage manager
(`smgr`). The header
[`storage/smgr.h`](../vendor/postgres-v18/src/include/storage/smgr.h) defines the
build-time marker

```c
#define NEON_SMGR 1
```

which lets extensions and contrib modules detect that this storage-manager API
is in effect. The one place the marker gates in-tree code is
[`read_stream.c`](../vendor/postgres-v18/src/backend/storage/aio/read_stream.c),
where `ASSUME_KERNEL_READAHEAD` is set to `0`: pages served over the network get
no benefit from OS readahead, so the read-stream machinery does not assume it.
The contrib index AMs `pgvector` and `rum` also branch on `NEON_SMGR` (see their
patches under [`compute/image/patches`](../image/patches)).

### The `yb_amp` storage manager

[`pagestore_smgr.c`](../pgxn/yb_amp/pagestore_smgr.c) defines
`static const struct f_smgr yb_amp_smgr` with `.smgr_name = "yb_amp"`. It
is registered by `smgr_register_yb_amp()`, which calls `smgrregister(&yb_amp_smgr)`
and installs the global unlogged-build hooks. Registration happens from
`pg_init_libpagestore()` **only when `yb_amp.pageserver_connstring` is set** — a
compute with no pageserver stays on the default md storage manager and purely
local files. Registering from the postmaster means the malloc'd smgr table is
already in place in every backend the postmaster later starts.

Relation ownership is decided by `yb_amp_owns()`:

```c
bool
yb_amp_owns(RelFileLocator rlocator, ProcNumber backend, char relpersistence)
{
    return backend == INVALID_PROC_NUMBER;
}
```

The `yb_amp` smgr is registered after md, so `smgropen()` consults it first;
non-temporary relations are claimed by `yb_amp`, while temporary relations
(`backend != INVALID_PROC_NUMBER`) fall through to md and stay on local disk.

The smgr implements the full `f_smgr` vtable — `open`, `close`, `create`,
`exists`, `unlink`, `extend`, `zeroextend`, `prefetch`, `readv`, `startreadv`,
`writev`, `writeback`, `nblocks`, `truncate`, `immedsync`, `registersync`,
`maxcombine`, and `fd` — plus `smgr_init_yb_amp()`, which registers the
end-of-transaction callback `AtEOXact_yb_amp` and calls `communicator_init()`.

### Read path

`yb_amp_readv()` decides the source of each requested page:

- **Temporary and unlogged relations** (and an in-progress permanent-relation
  unlogged build) go straight to `mdreadv()` — they are ordinary local files.
- **Permanent relations** are read in three tiers, cheapest first:
  1. `communicator_prefetch_lookupv()` — the prefetch ring, holding pages
     already requested from the pageserver via the read-stream advice path.
  2. `lfc_readv_select()` — the local file cache.
  3. `communicator_read_at_lsnv()` — a synchronous getpage request to the
     pageserver for any block still missing.

Each tier fills a bitmap of the blocks it satisfied, so only the remaining
blocks fall through to the next tier. All buffer reads arrive through
the AIO framework via `smgr_startreadv`; `yb_amp_startreadv()` fills the buffers
synchronously (through the same `yb_amp_readv()` path), sets the `fd = -1`
sentinel on the AIO handle, and stages it so the AIO layer completes it in place
without a `preadv`. Latency hiding still comes from the communicator's prefetch
pipeline.

`yb_amp_maxcombine()` returns `PG_IOV_MAX` (the storage is not segmented, so
combining is bounded only by the batch size), and `yb_amp_fd()` raises an error
because there is no file descriptor to hand back.

### Write path

`yb_amp_writev()` mirrors the read path:

- Temporary and unlogged relations (and the unlogged-build case) go to
  `mdwritev()`.
- For permanent relations the dirty page is **not written to a local data
  file**. Instead `yb_amp_wallog_pagev()` emits the WAL that makes the change
  durable, and `lfc_writev()` populates the local file cache with the new page
  contents. The WAL flows through the walproposer to the safekeeper, which
  persists it into YugabyteDB — that write is what makes the change durable.

Because permanent-relation writes never touch a local data file, the
`base/<dboid>/<relfilenode>` files that a self-contained Postgres materializes on
disk are absent on a `yb_amp` compute; the pageserver reconstructs them on
demand from ingested WAL.

### Read LSNs

Every getpage request carries a pair of LSNs computed by
`yb_amp_get_request_lsns()`: a `request_lsn` (the version of the page to read)
and a `not_modified_since` LSN. These derive from the last-written-LSN cache
([`yb_amp_lwlsncache.c`](../pgxn/yb_amp/yb_amp_lwlsncache.c)), which tracks the
highest LSN at which each block (or relation) was written locally. The
`not_modified_since` value lets the pageserver return an older materialized page
when nothing newer has been written, and it is what makes a prefetched response
safe to reuse: a prefetch result is only accepted if the block has not been
written past its `not_modified_since` since the request was issued. This LSN
bookkeeping is why the prefetch, LFC, and getpage tiers can be layered without
ever returning a stale page.

## Talking to the pageserver

[`libpagestore.c`](../pgxn/yb_amp/libpagestore.c) owns the pageserver connection
and the getpage protocol. The connection query is `pagestream_v3 <branch_id>`;
the protocol version is `yb_amp.protocol_version`, which accepts `2` or `3` and
defaults to `3`. The communicator background process pumps the request/response
stream and the prefetch ring; `communicator.h` exposes the operations the smgr
calls: `communicator_read_at_lsnv()` (getpage), `communicator_prefetch_*`,
`communicator_nblocks()`, `communicator_dbsize()`, and
`communicator_read_slru_segment()`. Relation-size, database-size, and SLRU
lookups are answered by the pageserver too — `dbsize_hook` and
`read_slru_segment_hook` are wired up next to the smgr registration.

The pageserver-facing GUCs (all in `pg_init_libpagestore()`):

| GUC | Context | Default | Purpose |
| --- | --- | --- | --- |
| `yb_amp.pageserver_connstring` | SIGHUP | `""` | Pageserver connection string; also the switch that installs the smgr |
| `yb_amp.branch_id` | POSTMASTER | `""` | Branch identifier sent in `pagestream_v3` and to the safekeeper |
| `yb_amp.project_id` | POSTMASTER | `""` | Project the compute belongs to |
| `yb_amp.endpoint_id` | POSTMASTER | `""` | Endpoint identifier |
| `yb_amp.protocol_version` | SU_BACKEND | `3` | compute↔pageserver protocol version (`2`–`3`) |
| `yb_amp.max_cluster_size` | SIGHUP | `-1` | Cluster size limit (MB); `-1` disables |
| `yb_amp.flush_output_after` | USERSET | `8` | Flush the request buffer after N unflushed requests |
| `yb_amp.max_reconnect_attempts` | USERSET | `60` | Reconnect attempts (1s each) before giving up |
| `yb_amp.readahead_buffer_size` | USERSET | `128` | Size of the prefetch ring (16–1024) |
| `yb_amp.readahead_getpage_pull_timeout` | USERSET | `50 ms` | Interval to actively pull outstanding readahead responses |
| `yb_amp.pageserver_response_log_timeout` | SUSET | `10 s` | Log a message if the pageserver is silent this long |
| `yb_amp.pageserver_response_disconnect_timeout` | SUSET | `30 s` | Disconnect and reconnect after this much silence |
| `yb_amp.compute_mode` | POSTMASTER | `primary` | Endpoint node type (primary / replica / static) |

The auth token used for both pageserver and safekeeper connections is read from
the `YBAMP_AUTH_TOKEN` environment variable.

## Local File Cache (LFC)

The LFC ([`file_cache.c`](../pgxn/yb_amp/file_cache.c)) is a compute-local,
on-disk cache of relation pages that sits between `shared_buffers` and the
pageserver. It is optional: with the LFC off, every buffer miss goes to the
pageserver.

### On-disk layout

All blocks of all relations live in a **single file**
(`yb_amp.file_cache_path`, default `file.cache`), recreated `O_TRUNC` at every
startup — the cache is deliberately not persisted, so there is no cross-restart
consistency to maintain. Space is managed in **chunks**, not single pages:
`MAX_BLOCKS_PER_CHUNK = 128` blocks (a 1 MB chunk at the 8 KB block size). Larger-
than-page chunks keep the in-memory index small (a chunk-granular hash map for a
multi-terabyte database is hundreds of MB rather than tens of GB) and improve
locality for sequential scans.

A shared `HTAB` (`lfc_hash`) maps a chunk-aligned `BufferTag` to a
`FileCacheEntry`, which records the chunk's slot `offset` in the file, an
`access_count` (pin count), an LRU list node, and a two-bits-per-block `state[]`
array (`UNAVAILABLE` / `AVAILABLE` / `PENDING` / `REQUESTED`). The file offset of
a block is `(offset * lfc_blocks_per_chunk + chunk_offset) * BLCKSZ`. The shared
control struct `FileCacheControl` (`lfc_ctl`) tracks the LRU list, a free-slot
("holes") list, a generation counter, hit/miss/eviction counters, and the
working-set estimator.

### Sizing GUCs

| GUC | Context | Default | Meaning |
| --- | --- | --- | --- |
| `yb_amp.max_file_cache_size` | POSTMASTER | `0` | Maximum cache size (MB). Fixes how much index/shmem space is reserved. `0` means the LFC is never initialized |
| `yb_amp.file_cache_size_limit` | SIGHUP | `0` | Live-tunable soft limit (MB), capped by `max_file_cache_size`. `0` switches the cache off |
| `yb_amp.file_cache_path` | POSTMASTER | `file.cache` | Backing file (may be a raw device) |
| `yb_amp.file_cache_chunk_size` | POSTMASTER | `128` | Blocks per chunk (power of two, ≤ 128) |
| `yb_amp.file_cache_prewarm_limit` | SIGHUP | `INT_MAX` | Max chunks a prewarm will load |
| `yb_amp.file_cache_prewarm_batch` | SIGHUP | `64` | Prewarm request batch size |
| `yb_amp.store_prefetch_result_in_lfc` | SUSET | `false` | Also store prefetch results into the LFC |
| `yb_amp.prewarm_update_ws_estimation` | SUSET | `true` | Feed prewarmed pages into the working-set estimator |

`max_file_cache_size` is fixed at postmaster start because it determines the size
of the shared hash table. `file_cache_size_limit` is the runtime knob: raising
or lowering it resizes the cache live, and setting it to `0` switches the cache
off (`LFC_ENABLED()` is defined as `lfc_ctl->limit != 0`). A cache that is off
can be turned back on later by raising the limit.

### Read / write behavior

On a read, `lfc_readv_select()` looks up each chunk. A miss simply reports fewer
blocks satisfied and returns — the LFC does not fetch from the pageserver
itself; the smgr caller does. A hit pins the entry, waits out any `PENDING` /
`REQUESTED` blocks on a condition variable, does a single vectored `preadv` for
the contiguous run, and moves the entry to the LRU tail. On a write,
`lfc_writev()` allocates a chunk (reusing a hole, extending the file, or evicting
the LRU head), marks the target blocks `PENDING`, writes them outside the lock,
then flips them to `AVAILABLE`. Eviction is strict **LRU** over a doubly-linked
list. Shrinking the cache punches holes with
`fallocate(FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE)` — disk usage drops while
the file's nominal size stays put, and the holes are reused before the file is
extended again.

Any I/O error calls `lfc_disable()`, which switches the cache off and
invalidates its contents rather than raising an error to the query — the LFC is
an optimization, never a source of truth. A `generation` counter guards the
lock-free portions of each I/O: an operation that finds the generation advanced
after its I/O knows the cache was reset underneath it and aborts.

### Prewarm and working-set estimation

The cache can be snapshotted (`get_local_cache_state()`) and reloaded
(`prewarm_local_cache()`), which spawns up to `MAX_PREWARM_WORKERS = 8` dynamic
background workers to pull the recorded pages from the pageserver — the one place
the LFC drives pageserver fetches directly. A prewarm never evicts live entries;
it stops when the cache is full.

Every read, write, and (optionally) prefetch feeds a HyperLogLog estimator
(`wss_estimation`) keyed on the block's hashed `BufferTag`. This backs
`approximate_working_set_size()` / `approximate_working_set_size_seconds()`,
which return the estimated number of distinct pages touched over a time window.
`lfc_get_stats()` exposes counters (`file_cache_hits`, `file_cache_misses`,
`file_cache_used`, `file_cache_evicted_pages`, `file_cache_limit`, and more).

### Concurrency

A single global LWLock (`lfc_lock`) protects the hash table and LRU list; reads
take it exclusively too, because moving an entry to the LRU tail mutates the
shared list. Per-block loading is coordinated outside the lock with a bank of 64
condition variables indexed by chunk hash, using the
`UNAVAILABLE → PENDING → REQUESTED → AVAILABLE` protocol so a backend that finds
a block being loaded can sleep until it is ready. The relevant wait events are
`YbAmp/FileCache_Read`, `YbAmp/FileCache_Write`, and `YbAmp/FileCache_CvWait`.

## Shipping WAL: the walproposer

The walproposer ([`walproposer.c`](../pgxn/yb_amp/walproposer.c) for the state
machine, [`walproposer_pg.c`](../pgxn/yb_amp/walproposer_pg.c) for the
Postgres-specific I/O) is how the compute gets WAL out to the safekeeper. It runs
in two modes:

1. As a background worker (registered by `walprop_register_bgworker` with library
   `yb_amp` and entry `WalProposerMain`) that impersonates a physical walsender,
   is woken on every WAL flush, and broadcasts WAL to the safekeeper.
2. As `postgres --sync-safekeepers` (`WalProposerSync`), to establish a safe
   start LSN.

`pg_init_walproposer()` registers the walproposer's GUCs, installs the
backpressure hooks (`delay_backend_us` and `ProcessInterruptsCallback`), and
registers the background worker (started even on replicas, so a replica can be
promoted). `WalProposerMain` returns immediately if `yb_amp.safekeepers` is
empty; otherwise it creates the walproposer, becomes a walsender
(`walprop_pg_init_walsender()` creates a persistent physical slot named
`wal_proposer_slot`), and enters the streaming loop. Each iteration reads the
flushed end of WAL with `GetFlushRecPtr`, applies the rate limiter, and calls
`WalProposerBroadcast`.

### Protocol and safekeeper count

Only protocol **v3** travels the wire: `WalProposerCreate`, `PAMessageSerialize`,
and `AsyncReadMessage` each raise `FATAL` on any other version, and the GUC
`yb_amp.safekeeper_proto_version` defaults to `3`. The handshake is:
`START_WAL_PUSH (proto_version '3', allow_timeline_creation 'true')` → expect
`COPY BOTH` → send `ProposerGreeting` (branch id, PG version, system id, WAL
segment size) → receive `AcceptorGreeting` (node id, flush LSN) → send
`ProposerElected` (start-streaming LSN) → stream `AppendRequest` messages
(`beginLsn`, `endLsn`, raw WAL, capped at `MAX_SEND_SIZE = XLOG_BLCKSZ * 16`).
The safekeeper replies with `AppendResponse` carrying its `flushLsn` and an
optional `PageserverFeedback` block.

A branch is served by a **single safekeeper**, whose WAL is durably backed by
YugabyteDB, so there is no election or voting: receiving the acceptor greeting
elects the proposer directly, and the committed position from
`GetAcknowledgedWALPosition()` is simply `safekeeper[0].flushLsn`. The
data structures accommodate more (`MAX_SAFEKEEPERS = 32`, a
`Safekeeper safekeeper[]` array, and comma-separated parsing of
`yb_amp.safekeepers`), and `BroadcastAppendRequest` iterates every active
safekeeper, but production runs exactly one and the multi-safekeeper code paths
are not exercised.

### Commit durability and backpressure

For each `AppendResponse`, `walprop_pg_process_safekeeper_feedback()` sets the
commit LSN to the safekeeper's `flushLsn` and calls `ProcessStandbyReply()`,
which advances `wal_proposer_slot` and wakes synchronous-replication waiters — a
commit blocks until the safekeeper reports the WAL flushed (persisted to
YugabyteDB, its durable store). The walproposer
registers with `synchronous_standby_names` under application name `walproposer`.
`PageserverFeedback` LSNs are recorded in shared memory and read back by
`replication_feedback_get_lsns()` (surfaced through the `backpressure_lsns()` SQL
function as `received_lsn` / `disk_consistent_lsn` / `remote_consistent_lsn`).

Backpressure throttles backends when the pageserver or safekeeper falls behind:
`backpressure_lag_impl()` compares the local flush pointer against the feedback
LSNs using `max_replication_write_lag` / `max_replication_flush_lag` /
`max_replication_apply_lag`, and `backpressure_throttling_impl()` sleeps backends
and accumulates the delay (readable via `backpressure_throttling_time()`). A
send-side WAL rate limiter (`WalRateLimiter`, GUCs `databricks.max_wal_mb_per_second`
and `databricks.throttled_max_wal_bytes_per_second`) bounds broadcast throughput.

The safekeeper connection GUCs (from `nwp_register_gucs`):

| GUC | Context | Default | Purpose |
| --- | --- | --- | --- |
| `yb_amp.safekeepers` | SIGHUP | `""` | Comma-separated `host:port` list of safekeepers |
| `yb_amp.safekeeper_conninfo_options` | POSTMASTER | `""` | Extra libpq keywords for safekeeper connections |
| `yb_amp.safekeeper_reconnect_timeout` | SIGHUP | `1000 ms` | Reconnect interval |
| `yb_amp.safekeeper_connect_timeout` | SIGHUP | `10000 ms` | Connection timeout |
| `yb_amp.safekeeper_proto_version` | POSTMASTER | `3` | Safekeeper protocol version |

Editing the safekeeper set (`assign_yb_amp_safekeepers`) restarts the walproposer
via `FATAL`.

### On-demand WAL reading

A replica, or the proposer's own send path, sometimes needs WAL that is not on
the local disk. [`yb_amp_walreader.c`](../pgxn/yb_amp/yb_amp_walreader.c)
implements a `YbAmpWALReader` with a three-level read: shared WAL buffers
first (`WALReadFromBuffers` — in steady state the proposer streams the
just-inserted tail, so the hot path is a shared-memory copy with no file
I/O), then the local `pg_wal/` segment (`pg_pread`; the catch-up path when
the buffer ring has wrapped, usually still served from the OS page cache),
and otherwise it downloads the missing range from the donor safekeeper over
a libpq replication connection (`START_REPLICATION PHYSICAL`), driven by a
non-blocking state machine. The walsender integration in
[`walsender_hooks.c`](../pgxn/yb_amp/walsender_hooks.c) installs these routines as
`XLogReaderRoutine` callbacks (a no-op when `yb_amp.safekeepers` is empty); when a
remote fetch would block, the reader waits on its socket under the
`YbAmp/WAL_Download` wait event.

## Replica startup from an arbitrary LSN

A replica can begin WAL replay at any LSN, not only at a checkpoint record. That
means there is no guarantee a running-xacts WAL record appears soon after the
start point, so a replica could otherwise sit unable to accept queries until the
primary happens to emit one. `RestoreRunningXactsFromClog()` (installed as
`restore_running_xacts_callback`) mitigates this by scanning the CLOG from the
checkpoint's `oldestActiveXid` and collecting the XIDs that are neither committed
nor aborted into the known-assigned-XIDs array. Because that array is bounded, the
scan caps itself at half of `TOTAL_MAX_CACHED_SUBXIDS`; on overflow it follows
`yb_amp.running_xacts_overflow_policy` (`ignore` / `skip` / `wait`). If the scan
cannot produce a complete list, Postgres waits for a running-xacts record as it
normally would.

## What is on local disk

`compute_ctl` builds `pgdata` fresh at every start (`create_pgdata()` wipes it),
then fills it with a basebackup. What ends up there:

- `postgresql.conf`, `pg_hba.conf`, `pg_ident.conf` — generated from the compute
  spec.
- `PG_VERSION`, `global/pg_control`, and the system catalog files — pulled from
  the pageserver by basebackup.
- `pg_wal/` — WAL segments produced here before they are shipped to the
  safekeeper. Ephemeral working state, reclaimed by normal checkpoint
  recycling: the walproposer's `wal_proposer_slot` (advanced by
  safekeeper acks, i.e. WAL durable in YB) is what holds segments until
  they are safe to recycle.
- `pg_xact/`, `pg_subtrans/`, `pg_multixact/` — SLRU files, also reconstructable
  from basebackup.
- Temporary and unlogged relation files — the only relation data written locally.
- `file.cache` — the LFC backing file, when the LFC is enabled.

Permanent user-table heap and index files are **not** present; reads go through
the `yb_amp` smgr to the pageserver.

The basebackup path is selected by the pageserver's preferred protocol
(`PageserverProtocol`): `Grpc` uses `page_api::Client` and a
`GetBaseBackupRequest`; `Libpq` issues a `basebackup <branch_id> [<lsn>]` command
over a libpq `COPY OUT`. Both address storage by `branch_id` and untar a gzip
stream into `pgdata`. A connection string parsed from the `postgresql://` /
`postgres://` scheme prefers `Libpq`; a `grpc://` scheme prefers `Grpc`.

## Launch and configuration

`compute_ctl` generates `postgresql.conf` in `write_postgres_conf`
([`config.rs`](../compute_tools/src/config.rs)). It writes, in order: the raw
`cluster.postgresql_conf` blob from the spec; the storage-connection GUCs
(`yb_amp.pageserver_connstring`, `yb_amp.safekeepers`, `yb_amp.branch_id`,
`yb_amp.project_id`, `yb_amp.endpoint_id`, `yb_amp.compute_mode`,
`yb_amp.privileged_role_name`, `yb_amp.extension_server_port`); the generic GUC
block from `cluster.settings`; audit settings; and finally an
`include_if_exists` for a temp override file that wins over everything.

`shared_preload_libraries` (which must contain `yb_amp`) is expected to arrive in
the compute spec's `cluster.settings`; `compute_ctl` appends the audit libraries
(`pgaudit`, `pgauditlogtofile`) when audit logging is on, and carries a
`yb_amp,pgaudit` default only as a defensive fallback. The whole configuration
namespace on the wire is `yb_amp.*`; there is no separate `tenant_id` /
`timeline_id` — a branch is identified by `branch_id`.

Startup ([`compute.rs`](../compute_tools/src/compute.rs)):
`start_compute()` runs a pre-startup phase whose tasks include `prepare_pgdata()`
(which calls `write_postgres_conf`, resolves the start LSN for the compute mode,
and runs `get_basebackup`), then `start_postgres()` launches
`postgres -D <pgdata>` with `YBAMP_AUTH_TOKEN` set in the environment (inside the
`yb-amp-postgres` cgroup when autoscaling). After Postgres is up, a primary runs
`configure_as_primary`, reports installed extensions, and moves to the `Running`
state. `post_apply_config` runs `ALTER EXTENSION yb_amp UPDATE` when the extension
is present so the installed SQL definitions track `default_version`.

## Where the code lives

- **PostgreSQL source:** [`compute/vendor/`](../vendor)
  (git submodule). The patch set is catalogued in
  [`core_changes.md`](core_changes.md).
- **The extension** (`yb_amp.so`, smgr, LFC, walproposer, communicator):
  [`compute/pgxn/yb_amp/`](../pgxn/yb_amp); the resource manager in
  [`compute/pgxn/yb_amp_rmgr/`](../pgxn/yb_amp_rmgr).
- **`compute_ctl`** (the per-endpoint supervisor):
  [`compute/compute_tools/`](../compute_tools), documented in
  [`compute_ctl.md`](compute_ctl.md).
- **Endpoint creation in the dev wrapper:**
  [`controller/local/launch/endpoint.go`](../../controller/local/launch/endpoint.go).
