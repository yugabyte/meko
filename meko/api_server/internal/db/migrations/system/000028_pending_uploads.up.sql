/*
 * Promised knowledge-base uploads, one row per presigned URL.
 *
 * A presigned upload takes api_server out of the transfer: the bytes go
 * client -> S3, so nothing reports the object landing. The browser closes that
 * gap itself by calling upload-complete then create. An agent has no such
 * contract: it can upload and stop there, leaving an object stored and
 * unindexed with nothing to notice. Every client's presign is recorded all the
 * same, the browser's included, because a row is also what the storage quota
 * counts an upload by until it is registered.
 *
 * Every component of the key ("<owner>/<datapack>/<sha256>/<filename>") is
 * known before a byte moves, so api_server writes down where the object will be
 * at presign time and a watcher goes looking for it. Registration then depends
 * on nothing the client does after the upload.
 *
 * Rows are bookkeeping, not history: a registered upload's row is deleted, its
 * contents now living in dist_rag.documents. Only failures are kept, as the one
 * piece of evidence that an upload was attempted, and only until they are
 * purged (PurgeTerminal).
 */
CREATE TABLE IF NOT EXISTS meko_system.pending_uploads (
    id               UUID        DEFAULT gen_random_uuid(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The address to look for. Unique because the key is content-addressed:
    -- re-requesting the same file derives the same key and must reuse the row
    -- rather than collide with it.
    s3_key           TEXT        NOT NULL,
    datapack_id      UUID        NOT NULL,
    -- The prefix is namespaced under the datapack OWNER, but on a shared
    -- datapack the uploader is someone else, and the uploader is what
    -- provenance records.
    owner_user_id    UUID        NOT NULL,
    uploader_id      UUID        NOT NULL,
    filename         TEXT        NOT NULL,

    declared_size    BIGINT      NOT NULL DEFAULT 0,  -- what the client claimed; never trusted
    -- The cap this URL was signed under, not the uploader's cap today: a tier
    -- change between presign and arrival must not retroactively reject an
    -- upload that was within its limit when authorized.
    max_size_bytes   BIGINT      NOT NULL,

    -- AWAITING_DISCOVERY is a row waiting on dist_rag's own CREATE_SOURCE
    -- worker: its source is new or still QUEUED/IN_PROGRESS, so that worker
    -- is listing the prefix, and registering alongside it is how duplicate
    -- documents appear. It stays claimable, on the same backoff as
    -- AWAITING_UPLOAD; once the source has settled the watcher registers it
    -- like any other row. Until then the row -- and the status a caller polls
    -- -- survives, rather than being retired before anything knows this key.
    state            TEXT        NOT NULL DEFAULT 'AWAITING_UPLOAD',
    -- Counts looks, not failures. It also spaces them out: the watcher waits
    -- 0, 1, 2, 4, 8 seconds and then a fixed cap between looks at one row, so
    -- a promise nobody fulfils costs dozens of S3 HeadObjects over its window
    -- rather than one per second. Which is what keeps the count diagnostic --
    -- a high one means many minutes of looking, the shape of an object the
    -- bucket won't serve, not a slow uploader.
    attempts         INT         NOT NULL DEFAULT 0,
    last_error       TEXT,

    -- Past this the bytes can never arrive, so it is the only certain "no" in
    -- the design. Set beyond the URL's own validity, since a transfer that
    -- began just before expiry may still be in flight.
    expires_at       TIMESTAMPTZ NOT NULL,
    -- Which pod owns the row right now. The S3 work happens outside any
    -- transaction, so this lease -- not a row lock -- is what excludes other
    -- pods, and a pod that dies mid-pass is recovered when it expires.
    lease_expires_at TIMESTAMPTZ,
    -- When the watcher registered this row's document. The row lives on until
    -- the document's indexing is queued, and from here the document counts
    -- against the storage quota at its real size, so the row no longer does.
    registered_at    TIMESTAMPTZ,
    -- What state was set to on the transition into IN_PROGRESS, so Claim can
    -- return it instead of IN_PROGRESS itself. It survives a crash: a pod that
    -- dies mid-pass leaves the row IN_PROGRESS with prior_state already set, so
    -- lease recovery still returns AWAITING_DISCOVERY rather than losing it to
    -- the recovering claim's own IN_PROGRESS value.
    prior_state      TEXT,

    PRIMARY KEY (id HASH),
    CONSTRAINT pending_uploads_state_check CHECK (
        state IN ('AWAITING_UPLOAD', 'IN_PROGRESS', 'AWAITING_DISCOVERY', 'ABANDONED', 'OVERSIZED')
    ),
    CONSTRAINT pending_uploads_prior_state_check CHECK (
        prior_state IS NULL OR prior_state IN ('AWAITING_UPLOAD', 'AWAITING_DISCOVERY')
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_uploads_s3_key
    ON meko_system.pending_uploads (s3_key);

-- The watcher's claim query. Partial so its cost tracks rows still live rather
-- than rows ever created.
--
-- ASC, not YugabyteDB's default HASH for a leading index column: the claim
-- takes the oldest rows first so a burst can't starve an early promise, and a
-- hash-sharded index cannot serve that ordering. The write hotspot a range
-- index on a clock column normally brings is not a concern at one row per
-- upload.
CREATE INDEX IF NOT EXISTS idx_pending_uploads_claim
    ON meko_system.pending_uploads (created_at ASC)
    WHERE state IN ('AWAITING_UPLOAD', 'IN_PROGRESS', 'AWAITING_DISCOVERY');

-- The storage-quota check, run on every presign, sums an owner's live rows.
CREATE INDEX IF NOT EXISTS idx_pending_uploads_owner_live
    ON meko_system.pending_uploads (owner_user_id)
    WHERE state IN ('AWAITING_UPLOAD', 'IN_PROGRESS', 'AWAITING_DISCOVERY');

-- Terminal rows are kept only while a caller may still ask why an upload
-- failed, then purged oldest first. ASC for the same ordering reason as the
-- claim index.
CREATE INDEX IF NOT EXISTS idx_pending_uploads_terminal
    ON meko_system.pending_uploads (updated_at ASC)
    WHERE state IN ('ABANDONED', 'OVERSIZED');
