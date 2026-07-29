-- 0002: provision the compute-facing (data-plane) schema of the shared
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
-- The controller's own registry (generation authority + node/branch/endpoint
-- records) lives in the `amp_registry` schema, provisioned by 0003. Every
-- statement is IF-NOT-EXISTS so the migration is idempotent. Objects are
-- schema-qualified (the migrator's search_path is `controller`).

CREATE SCHEMA IF NOT EXISTS amp_storage;

-- =====================================================================
-- Blob namespace + blobs storage table.
--
-- Shared by pageserver and safekeeper. The `blob_namespace` ENUM
-- discriminates between which service a row belongs to; its values name
-- the writing service, not a schema. (Live WAL chunks live in
-- `amp_storage.wals`, defined below.)
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
    PRIMARY KEY ((branch_id, key) HASH, namespace, chunk_idx)
);

-- =====================================================================
-- Page-store tables — written and read by yb-pageserver.
-- =====================================================================

-- Relation pages only: keys where `Key::is_rel_block_key()` holds
-- (field1 == 0x00, field4 != 0, field6 != 0xffffffff). Hash partition
-- on (branch, spcnode, dbnode, relnode, forknum) keeps a
-- relation's whole block-set on one tablet; range scans within a
-- relfork are tablet-local. The trailing `is_image` breaks `lsn`
-- ties; it is DESC so the stored order matches the read-path
-- `ORDER BY lsn DESC, is_image DESC` (image before delta at a tie).
CREATE TABLE IF NOT EXISTS amp_storage.pages (
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
    PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                 blknum, lsn DESC, is_image DESC)
);

-- SLRU pages — pg_xact, pg_multixact_offsets, pg_multixact_members.
-- `kind` matches `Key::field2` byte ordering: 0=Clog,
-- 1=MultiXactMembers, 2=MultiXactOffsets. Hash partition keeps a
-- whole SLRU segment on one tablet.
CREATE TABLE IF NOT EXISTS amp_storage.slru_pages (
    branch_id    TEXT     NOT NULL,
    kind         SMALLINT NOT NULL,
    segno        BIGINT   NOT NULL,
    blknum       BIGINT   NOT NULL,
    is_image     BOOLEAN  NOT NULL,
    lsn          BIGINT   NOT NULL,
    will_init    BOOLEAN  NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, kind, segno) HASH,
                 blknum, lsn DESC, is_image DESC)
);

-- Metadata blobs — everything that isn't a relation page or SLRU
-- block: relsize sentinels, directory listings (TenantDir/DbDir/
-- RelDir/SlruDir), aux files (yb_amp.signal, postgresql.auto.conf,
-- replication-slot state), two-phase commit files, replication
-- origins, SLRU segment-size sentinels. Full-overwrite semantics:
-- read = latest row ≤ target_lsn. No walredo, no chains.
CREATE TABLE IF NOT EXISTS amp_storage.meta (
    branch_id    TEXT     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, key) HASH, lsn DESC)
);

-- =====================================================================
-- Per-branch state — one row per branch, shared by both daemons.
-- =====================================================================

-- The pageserver and the safekeeper each own a group of columns here and write
-- only their own group. The other group is NULL until its owner fills it in, so
-- **row presence is not a signal**: what tells a daemon whether its own state
-- exists is whether its group is populated.
--
--   pageserver present  <=>  consistent_lsn     IS NOT NULL
--   safekeeper present  <=>  timeline_start_lsn IS NOT NULL
--
-- Those two predicates are load-bearing. The pageserver decides bootstrap
-- (initdb + import) vs load from its predicate, and the safekeeper decides
-- restore-control-file vs create-on-first-handshake from its own.
--
-- The row itself is created by the CONTROLLER at branch-create time
-- (Store.RecordBranch), before any pageserver, safekeeper, or endpoint
-- supervisor for the branch starts. Neither daemon creates it: each fills its
-- own group with `UPDATE ... WHERE <own discriminator> IS NULL`, so the write
-- lands exactly once and a retry is idempotent. A daemon that finds no row
-- fails loudly rather than inventing one.
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
-- stream. Partitioned into buckets to spread the load across multiple DocDB
-- tablets.
CREATE TABLE IF NOT EXISTS amp_storage.wals (
    branch_id        TEXT     NOT NULL,
    bucket_id        SMALLINT NOT NULL,
    segno            BIGINT   NOT NULL,
    chunk_offset     BIGINT   NOT NULL,
    chunk_size       INT      NOT NULL,
    data             BYTEA    NOT NULL,
    last_modified_us BIGINT   NOT NULL,
    -- (segno, chunk_offset) is the chunk's natural identity within the
    -- branch. The writer never produces overlapping chunks
    -- (truncate_wal aligns the next write at exactly the truncation
    -- point), so a same-PK INSERT is necessarily an idempotent retry of
    -- the same bytes. `bucket_id` is placement only — it carries no
    -- identity (retries replay the same stamped batch, keeping its
    -- original bucket).
    PRIMARY KEY ((branch_id, bucket_id) HASH, segno, chunk_offset)
) SPLIT INTO 10 TABLETS;

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
