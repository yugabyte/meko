-- 0003: provision the compute-facing (data-plane) schema of the shared
-- `yb_storage` database: `amp_storage`, holding the blobs table + its enum, the
-- page-store tables, the WAL mirror, and the per-branch state row shared by the
-- pageserver and the safekeeper.
--
-- These schema/table names are HARDCODED by the pageserver/safekeeper/
-- endpoint_storage daemons (e.g. compute/pageserver/src/page_store.rs
-- DEFAULT_SCHEMA="amp_storage", compute/safekeeper/src/yb_wal_storage.rs
-- "amp_storage.wals") and bound immediately at daemon startup — they never create
-- their own schema. So this migration must run before those daemons start.
--
-- The write-ahead task log lives in the `controller` schema (0001) and the
-- control plane's resource model — users, storage pools, projects, branches,
-- endpoints — in `amp_registry` (0002). Every statement here is IF-NOT-EXISTS so
-- the migration is idempotent. Objects are schema-qualified (the migrator's
-- search_path is `amp_registry`).

CREATE SCHEMA IF NOT EXISTS amp_storage;

-- =====================================================================
-- Blob namespace + blobs storage table.
--
-- Shared by pageserver and safekeeper. The `blob_namespace` ENUM
-- discriminates between which service a row belongs to; its values name
-- the writing service, not a schema. (Live WAL chunks live in
-- `amp_storage.wals`, defined below.)
--
-- `branch_id` alone is the hash key — here and on `slru_pages`/`meta`
-- below — so branch-scoped operations (prefix LIST, branch-drop GC) are
-- single-tablet and key-bound. One hash value never auto-splits: a hard
-- per-branch ceiling, safe because per-branch data in these tables is
-- bounded. `pages`/`wals` are unbounded per branch and lead with a
-- range `bucket_id` instead.
-- =====================================================================

-- CREATE TYPE has no IF NOT EXISTS; guard against a pre-existing type.
DO $$ BEGIN
    CREATE TYPE amp_storage.blob_namespace AS ENUM ('pageserver', 'safekeeper');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS amp_storage.blobs (
    branch_id        TEXT                       NOT NULL,
    namespace        amp_storage.blob_namespace NOT NULL,
    key              TEXT                       NOT NULL,
    generation       INT,
    chunk_idx        INT                        NOT NULL,
    data             BYTEA                      NOT NULL,
    size_bytes       BIGINT                     NOT NULL,
    etag             TEXT                       NOT NULL,
    last_modified_us BIGINT                     NOT NULL,
    metadata_json    TEXT,
    -- namespace (always equality-bound) before `key` (prefix-scanned):
    -- a LIST stays inside one service's span; chunks stay adjacent.
    PRIMARY KEY ((branch_id) HASH, namespace ASC, key ASC, chunk_idx ASC)
);

-- =====================================================================
-- Page-store tables — written and read by yb-pageserver.
-- =====================================================================

-- Relation pages only: keys where `Key::is_rel_block_key()` holds
-- (field1 == 0x00, field4 != 0, field6 != 0xffffffff).
--
-- ALL-RANGE key led by `bucket_id`, an m-way (m = 6) placement bucket.
-- The old form hash-partitioned on (branch, spcnode, dbnode, relnode,
-- forknum), which pinned a whole relfork to ONE tablet: a single hot
-- relation could not use more than one tablet's write throughput, and
-- the tablet count for a branch was whatever the hash space gave us.
-- Range keys let us PRE-SPLIT at known boundaries instead.
--
-- bucket_id = ((relnode / 7) + (relnode / 13) + (blknum / 256)) % 6
--   * `blknum / 256` keeps 2MB stretches (256 x 8KB) contiguous, so a
--     sequential scan walks whole stretches instead of spraying every
--     block across buckets. A relation larger than 6 x 2MB spreads over
--     all six buckets by this term alone.
--   * `relnode / 7 + relnode / 13` salts small (< 2MB) relations, which
--     are one stretch and would otherwise ALL land in bucket 0. Two
--     coprime divisors, because a single one collapses when the OID
--     stride resonates with m: with `relnode / 7` alone, a stride of 42
--     puts every relation in one bucket. Both terms have to degenerate
--     at once, which first happens at stride 273. Worst measured
--     imbalance for any stride <= 200 is 2.0x, and 1.39x for <= 30.
--   * forknum is deliberately NOT in the salt: the main fork (0) holds
--     nearly all the data, so mixing it in only moves FSM/VM around.
--     A relation's forks therefore share a bucket, which also keeps its
--     metadata forks on the same tablet as its data.
--
-- WRITER-computed (page_store::page_bucket_id, FROZEN, modulus =
-- PAGE_BUCKETS) and deliberately not a generated column — the
-- thin-client write path bypasses the YSQL layer entirely. The CHECK
-- below is real enforcement only on the YSQL lane; it does not fire for
-- thin-client writes, so it documents the contract rather than
-- guaranteeing it. NB: a mis-computed bucket here is worse than in
-- `pending_gc` (a missed candidate) — the row becomes unreachable at
-- read time, i.e. a lost page. Queries MUST bind bucket_id as a VALUE;
-- without bucket_id equality a query scans all six buckets.
--
-- Pre-split into exactly m tablets so writes spread from the first row.
-- Tablets can auto-split further later; m bounds the initial fan-out,
-- not the eventual tablet count. Whole-branch / whole-db / whole-fork
-- operations (drops, dumps) must fan out across all six buckets.
--
-- The trailing `is_image` breaks `lsn` ties; it is DESC so the stored
-- order matches the read-path `ORDER BY lsn DESC, is_image DESC`
-- (image before delta at a tie).
CREATE TABLE IF NOT EXISTS amp_storage.pages (
    bucket_id    SMALLINT NOT NULL,
    branch_id    TEXT     NOT NULL,
    spcnode      BIGINT   NOT NULL,
    dbnode       BIGINT   NOT NULL,
    relnode      BIGINT   NOT NULL,
    forknum      SMALLINT NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    -- Subsumes a plain 0..5 range check: the expression cannot leave
    -- that interval for non-negative relnode/blknum. Integer division
    -- throughout — `/` and `%` are truncated in both PostgreSQL and
    -- Rust, so this is character-for-character the Rust function. A
    -- float (`/ 256.0`, `floor(...)`) would let the two sides diverge.
    CONSTRAINT pages_bucket_id_matches_formula CHECK (
        bucket_id = ((relnode / 7) + (relnode / 13) + (blknum / 256)) % 6
    ),
    PRIMARY KEY (bucket_id ASC, branch_id ASC, spcnode ASC, dbnode ASC,
                 relnode ASC, forknum ASC, blknum ASC, lsn DESC, is_image DESC)
) SPLIT AT VALUES ((1), (2), (3), (4), (5));

-- SLRU pages — pg_xact, pg_multixact_offsets, pg_multixact_members.
-- `kind` matches `Key::field2` byte ordering: 0=Clog,
-- 1=MultiXactMembers, 2=MultiXactOffsets.
--
-- `branch_id`-only hash (see the `blobs` header). Size is protocol-capped
-- (2 bits/xact clog; vacuum horizons truncate; the 32-bit xid/mxid spaces
-- bound the worst case below a tablet), and the hot key is the single
-- head page per kind — one key, which no bucketing can spread. A
-- `(kind, segno)` segment stays contiguous; versions `lsn DESC`, image
-- first on ties (matching `pages`).
CREATE TABLE IF NOT EXISTS amp_storage.slru_pages (
    branch_id    TEXT     NOT NULL,
    kind         SMALLINT NOT NULL,
    segno        BIGINT   NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id) HASH,
                 kind ASC, segno ASC, blknum ASC, lsn DESC, is_image DESC)
);

-- Metadata blobs — everything that isn't a relation page or SLRU
-- block: relsize sentinels, directory listings (TenantDir/DbDir/
-- RelDir/SlruDir), aux files (yb_amp.signal, postgresql.auto.conf,
-- replication-slot state), two-phase commit files, replication
-- origins, SLRU segment-size sentinels. Full-overwrite semantics:
-- read = latest row ≤ target_lsn. No walredo, no chains.
--
-- `branch_id`-only hash (see the `blobs` header); `key` as a range
-- column makes `scan_meta_range` contiguous.
CREATE TABLE IF NOT EXISTS amp_storage.meta (
    branch_id    TEXT     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC)
);

-- =====================================================================
-- Per-branch state — one row per branch, shared by both daemons.
-- =====================================================================

-- The pageserver and the safekeeper each own a group of columns here and write
-- only their own group. The other group is NULL until its owner fills it in, so
-- **row presence is not a signal**: what tells a daemon whether its own state
-- exists is whether its group is populated.
--
--   pageserver present  <=>  pageserver_initialized
--   safekeeper present  <=>  safekeeper_initialized
--
-- Those two flags are load-bearing. The pageserver decides bootstrap (initdb +
-- import) vs load from its flag, and the safekeeper decides
-- restore-control-file vs create-on-first-handshake from its own. Each is set
-- by exactly one statement — that daemon's creation write — in the same
-- statement that fills its group, so flag and group cannot disagree. A data
-- column cannot carry this meaning: `consistent_lsn` used to, and another
-- writer set it first (#552).
--
-- The row itself is created by the CONTROLLER at branch-create time
-- (Store.RecordBranch), before any pageserver, safekeeper, or endpoint
-- supervisor for the branch starts. Neither daemon creates it: each fills its
-- own group once, and a retry is idempotent. A daemon that finds no row fails
-- loudly rather than inventing one.
--
-- NB: the flag columns were added to this migration in place. It is `CREATE
-- TABLE IF NOT EXISTS` and golang-migrate records 0003 as applied, so a
-- database provisioned before they existed does NOT get them and every daemon
-- fails on its first query — recreate it (`yb_amp_local init --force
-- remove-all-contents`).
--
-- `pg_version` is the one shared column, and it holds a full `PgVersionId`
-- (e.g. 180002), not a major version. What makes a single shared column safe is
-- that it has exactly one writer: the CONTROLLER seeds it at branch create with
-- the full id of the PostgreSQL amp ships (registry.FullPgVersionID; 180002 for
-- PG 18), and nothing writes it again.
--
-- Both daemons only read it. The pageserver derives the major it needs by
-- integer division by 10000. The safekeeper loads it into ServerInfo and checks
-- it against every walproposer greeting — a check on the *major* version only,
-- so the seeded value needs no later refinement.
--
-- The pageserver group carries every load-bearing field from the page-store
-- index: consistent/gc/initdb LSNs, generation, archive/delete flags, rel-size
-- schema-migration state, plus a JSON `lineage` column for future
-- ancestor-detach support (always `Default` in yb-mode today). The safekeeper
-- group is its control file: hot-update LSN scalars get their own BIGINT
-- columns so a control-file save is a column-scoped UPDATE rather than a
-- full-row rewrite. yb-amp runs a single safekeeper per branch with WAL
-- durability provided by YugabyteDB, so consensus state (term / term_history)
-- and membership (mconf) are absent.
CREATE TABLE IF NOT EXISTS amp_storage.branch_state (
    branch_id              TEXT        NOT NULL,
    pg_version             INT         NOT NULL,

    -- pageserver-owned group; NULL until the pageserver creates its state.
    pageserver_initialized BOOLEAN     NOT NULL DEFAULT false,
    consistent_lsn         BIGINT,
    applied_gc_cutoff_lsn  BIGINT,
    ancestor_branch_id     TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT,
    initdb_lsn             BIGINT,
    prev_record_lsn        BIGINT,
    archived_at            TIMESTAMP,
    deleted_at             TIMESTAMP,
    rel_size_migration     TEXT,
    rel_size_migrated_at   BIGINT,
    lineage                JSONB,

    -- safekeeper-owned group; NULL until the safekeeper creates its control file.
    safekeeper_initialized BOOLEAN     NOT NULL DEFAULT false,
    timeline_start_lsn     BIGINT,
    local_start_lsn        BIGINT,
    remote_consistent_lsn  BIGINT,
    system_id              BIGINT,
    wal_seg_size           INT,
    creation_ts            TIMESTAMPTZ,

    PRIMARY KEY ((branch_id) HASH)
);

-- GC candidate queue: one row per garbage-producing event, drained once
-- the branch's cutoff passes `due_lsn`. A candidate names its garbage by
-- the encoded `Key` (`key`, the same 18-byte layout `meta` uses) plus a
-- `gc_kind` that names both the operation and its target table
-- (0=trim_pages, 1=trim_slru, 2=trim_meta, 3=shrink_pages, 4=drop_pages,
-- 5=drop_slru, 6=drop_meta); the drain dispatches on gc_kind alone. One
-- keying convention covers rel blocks, SLRU blocks, and meta keys alike.
-- Replayed duplicates dedup: the
-- candidate's identity is its PK and writers run upsert-mode, so a replay
-- overwrites in place. `bucket_id` is placement only, WRITER-computed
-- (page_store::gc_bucket_id, frozen hash of `key`, modulus = GC_BUCKETS) —
-- deliberately not a generated column (the yb-client write path bypasses
-- the YSQL layer). NB: queries must bind bucket_id as a VALUE; without
-- bucket_id equality a query seq-scans. Explicit SPLIT into 3 tablets: the
-- queue stays small (continuously drained); tablets (3) < buckets (10)
-- bounds a statement's distributed-txn fan-out.
CREATE TABLE IF NOT EXISTS amp_storage.pending_gc (
    branch_id  TEXT     NOT NULL,
    bucket_id  INT      NOT NULL,
    due_lsn    BIGINT   NOT NULL,
    key        BYTEA    NOT NULL,   -- encoded Key: block (trim) or object (drop/shrink)
    gc_kind    SMALLINT NOT NULL,   -- 0=trim_pages 1=trim_slru 2=trim_meta 3=shrink_pages 4=drop_pages 5=drop_slru 6=drop_meta
    -- Rel size (blocks) as of a shrink. Set on shrink candidates only
    -- (NULL for trim/drop; shrink is rel-only).
    relsize    BIGINT,
    PRIMARY KEY ((branch_id, bucket_id) HASH, due_lsn, key, gc_kind)
) SPLIT INTO 3 TABLETS;

-- =====================================================================
-- WAL mirror — written and read by yb-safekeeper.
-- =====================================================================

-- Live WAL chunks the safekeeper mirrors into YB — the durable copy of the WAL
-- stream. Partitioned into m = 6 buckets to spread the load across DocDB
-- tablets, matching `pages`.
--
-- ALL-RANGE key led by `bucket_id`, pre-split into exactly m tablets.
-- Unlike `pages`, this bucket is NOT a function of the row: it is a
-- rotating per-batch counter (`(n + 1) % WALS_BUCKETS`), so one batch
-- lands on one tablet and consecutive batches move on. Reads therefore
-- cannot compute it and always fan out across all six buckets — which
-- the read path already did under the old hash key.
--
-- `bucket_id` leads rather than `branch_id` for two reasons. Pre-splitting
-- on a leading `branch_id` would need literal branch boundaries, but
-- branch IDs are minted at runtime; and it would put one branch's entire
-- WAL stream in a single contiguous key span — one tablet until it
-- auto-splits — which is the exact write hotspot `bucket_id` exists to
-- avoid, since one branch streaming WAL hard is the hot write pattern.
-- Bucket-leading spreads a single branch's WAL over all six tablets from
-- the first row while keeping each batch on one tablet.
CREATE TABLE IF NOT EXISTS amp_storage.wals (
    bucket_id        SMALLINT NOT NULL,
    branch_id        TEXT     NOT NULL,
    segno            BIGINT   NOT NULL,
    chunk_offset     BIGINT   NOT NULL,
    chunk_size       INT      NOT NULL,
    data             BYTEA    NOT NULL,
    last_modified_us BIGINT   NOT NULL,
    -- Placement only, so unlike `pages` there is no formula to check —
    -- just the modulus bound. Enforced on the YSQL lane only (the
    -- thin-client write path bypasses it).
    CONSTRAINT wals_bucket_id_in_range CHECK (bucket_id >= 0 AND bucket_id < 6),
    -- (segno, chunk_offset) is the chunk's natural identity within the
    -- branch. The writer never produces overlapping chunks
    -- (truncate_wal aligns the next write at exactly the truncation
    -- point), so a same-PK INSERT is necessarily an idempotent retry of
    -- the same bytes. `bucket_id` is placement only — it carries no
    -- identity (retries replay the same stamped batch, keeping its
    -- original bucket).
    PRIMARY KEY (bucket_id ASC, branch_id ASC, segno ASC, chunk_offset ASC)
) SPLIT AT VALUES ((1), (2), (3), (4), (5));

-- =====================================================================
-- Functions.
-- =====================================================================

-- Inspection helper: decode a `pending_gc.key` (or any `meta.key`) by
-- unpacking the fixed 18-byte Key and formatting by field1. Mirrors the
-- Rust Key codec (compute/libs/pageserver_api/src/key.rs); the
-- `gc_key_decode_matches_key_codec` test cross-checks the two, so keep them
-- in sync if the layout changes. Handy in psql:
--   SELECT gc_kind, amp_storage.gc_key_decode(key), due_lsn FROM amp_storage.pending_gc ...
CREATE OR REPLACE FUNCTION amp_storage.gc_key_decode(k BYTEA) RETURNS TEXT AS $$
DECLARE
    f1 INT;
    f2 BIGINT; f3 BIGINT; f4 BIGINT; f6 BIGINT;
    f5 INT;
BEGIN
    IF k IS NULL OR octet_length(k) <> 18 THEN
        RETURN format('raw(%s bytes)', COALESCE(octet_length(k), 0));
    END IF;
    f1 := get_byte(k, 0);
    f2 := (get_byte(k,1)::bigint<<24)|(get_byte(k,2)::bigint<<16)|(get_byte(k,3)::bigint<<8)|get_byte(k,4)::bigint;
    f3 := (get_byte(k,5)::bigint<<24)|(get_byte(k,6)::bigint<<16)|(get_byte(k,7)::bigint<<8)|get_byte(k,8)::bigint;
    f4 := (get_byte(k,9)::bigint<<24)|(get_byte(k,10)::bigint<<16)|(get_byte(k,11)::bigint<<8)|get_byte(k,12)::bigint;
    f5 := get_byte(k, 13);
    f6 := (get_byte(k,14)::bigint<<24)|(get_byte(k,15)::bigint<<16)|(get_byte(k,16)::bigint<<8)|get_byte(k,17)::bigint;
    IF f1 = 0 THEN            -- rel keyspace
        IF f6 = 4294967295 THEN
            RETURN format('rel_size spc=%s db=%s rel=%s fork=%s', f2, f3, f4, f5);
        END IF;
        RETURN format('rel_block spc=%s db=%s rel=%s fork=%s blk=%s', f2, f3, f4, f5, f6);
    ELSIF f1 = 1 THEN        -- SLRU keyspace (field2: 0=clog,1=mx_members,2=mx_offsets)
        IF f3 = 0 THEN
            RETURN format('slru_dir kind=%s', f2);
        ELSIF f6 = 4294967295 THEN
            RETURN format('slru_seg_size kind=%s seg=%s', f2, f4);
        END IF;
        RETURN format('slru_block kind=%s seg=%s blk=%s', f2, f4, f6);
    END IF;
    RETURN format('meta f1=%s f2=%s f3=%s f4=%s f5=%s f6=%s', f1, f2, f3, f4, f5, f6);
END;
$$ LANGUAGE plpgsql IMMUTABLE;
