-- 0002: provision the compute-facing (data-plane) schemas of the shared
-- `yb_storage` database: `public.blobs` + its enum, the `pageserver.*` page-store
-- tables, and the `safekeeper.*` WAL/control tables.
--
-- These schema/table names are HARDCODED by the pageserver/safekeeper/
-- endpoint_storage daemons (e.g. compute/pageserver/src/page_store.rs
-- DEFAULT_SCHEMA="pageserver", compute/safekeeper/src/yb_wal_storage.rs
-- "safekeeper.wals") and bound immediately at daemon startup — they never create
-- their own schema. So this migration must run before those daemons start.
--
-- The controller's own registry (generation authority + node/branch/endpoint
-- records) lives in the `registry` schema, provisioned by 0003. Every statement
-- is IF-NOT-EXISTS so the migration is idempotent. Objects are schema-qualified
-- (the migrator's search_path is `controller`).

CREATE SCHEMA IF NOT EXISTS pageserver;
CREATE SCHEMA IF NOT EXISTS safekeeper;

-- =====================================================================
-- public.* — blob namespace + blobs storage table.
--
-- Shared by pageserver and safekeeper. The `blob_namespace` ENUM
-- discriminates between which service a row belongs to. (Live WAL
-- chunks live in `safekeeper.wals`, defined in the safekeeper section.)
-- =====================================================================

-- CREATE TYPE has no IF NOT EXISTS; guard against a pre-existing type.
DO $$ BEGIN
    CREATE TYPE public.blob_namespace AS ENUM ('pageserver', 'safekeeper');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS public.blobs (
    branch_id        TEXT                  NOT NULL,
    namespace        public.blob_namespace NOT NULL,
    key              TEXT                  NOT NULL,
    generation       INT,
    chunk_idx        INT                   NOT NULL,
    data             BYTEA                 NOT NULL,
    size_bytes       BIGINT                NOT NULL,
    etag             TEXT                  NOT NULL,
    last_modified_us BIGINT                NOT NULL,
    metadata_json    TEXT,
    PRIMARY KEY ((branch_id, key) HASH, namespace, chunk_idx)
);

-- =====================================================================
-- pageserver.* — yb-pageserver page-store tables.
-- =====================================================================

-- Relation pages only: keys where `Key::is_rel_block_key()` holds
-- (field1 == 0x00, field4 != 0, field6 != 0xffffffff). Hash partition
-- on (branch, spcnode, dbnode, relnode, forknum) keeps a
-- relation's whole block-set on one tablet; range scans within a
-- relfork are tablet-local. The trailing `is_image` breaks `lsn`
-- ties; it is DESC so the stored order matches the read-path
-- `ORDER BY lsn DESC, is_image DESC` (image before delta at a tie).
CREATE TABLE IF NOT EXISTS pageserver.pages (
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
CREATE TABLE IF NOT EXISTS pageserver.slru_pages (
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
CREATE TABLE IF NOT EXISTS pageserver.meta (
    branch_id    TEXT     NOT NULL,
    key          BYTEA    NOT NULL,
    lsn          BIGINT   NOT NULL,
    payload      BYTEA    NOT NULL,
    PRIMARY KEY ((branch_id, key) HASH, lsn DESC)
);

-- Per-branch state row — one durable row in YB carrying every load-bearing
-- field from the page-store index: consistent/gc/initdb LSNs, generation,
-- archive/delete flags, rel-size schema-migration state, plus a JSON `lineage`
-- column for future ancestor-detach support (always `Default` in yb-mode today).
CREATE TABLE IF NOT EXISTS pageserver.timeline_state (
    branch_id              TEXT      NOT NULL,
    consistent_lsn         BIGINT    NOT NULL,
    applied_gc_cutoff_lsn  BIGINT    NOT NULL,
    ancestor_branch_id     TEXT,
    ancestor_lsn           BIGINT,
    current_generation     INT       NOT NULL,
    pg_version             INT       NOT NULL,
    initdb_lsn             BIGINT,
    prev_record_lsn        BIGINT,
    archived_at            TIMESTAMP,
    deleted_at             TIMESTAMP,
    rel_size_migration     TEXT,
    rel_size_migrated_at   BIGINT,
    lineage                JSONB,
    PRIMARY KEY ((branch_id) HASH)
);

-- =====================================================================
-- safekeeper.* — yb-safekeeper persistent state.
-- =====================================================================

-- Per-branch safekeeper control-file row. One row per branch, hash-partitioned
-- on `branch_id` so the row lives on a single tablet. Hot-update LSN scalars get
-- their own BIGINT columns so a control-file save is a column-scoped UPDATE
-- rather than a full-row rewrite. yb-amp runs a single safekeeper per branch with
-- WAL durability provided by YugabyteDB, so consensus state (term / term_history)
-- and membership (mconf) are absent.
CREATE TABLE IF NOT EXISTS safekeeper.timeline_state (
    branch_id              TEXT        NOT NULL,
    timeline_start_lsn     BIGINT      NOT NULL,
    local_start_lsn        BIGINT      NOT NULL,
    remote_consistent_lsn  BIGINT      NOT NULL,
    pg_version             INT         NOT NULL,
    system_id              BIGINT      NOT NULL,
    wal_seg_size           INT         NOT NULL,
    creation_ts            TIMESTAMPTZ NOT NULL,
    PRIMARY KEY ((branch_id) HASH)
);

-- Live WAL chunks the safekeeper mirrors into YB — the durable copy of the WAL
-- stream. Partitioned into buckets to spread the load across multiple DocDB
-- tablets.
CREATE TABLE IF NOT EXISTS safekeeper.wals (
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
