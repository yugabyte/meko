---
status: done
severity: medium
area: safekeeper
opened: 2026-06-05
closed: 2026-06-05
---

## DONE in commit `840e56aae`

Schema landed at the planned shape; `YbControlFileStorage`
rewritten to read/write the row via `YsqlClient`; the blob path
(magic + bincode + crc32c + `safekeeper-control/...` RemotePath)
is gone, along with the entire `control_file_upgrade.rs` module
(778 LOC of legacy V1..V9 upgrade paths that only ran against
the deleted blob). e2e green. Follow-on cleanup in the same
commit: dropped `--remote-storage` SK CLI arg + matching field
on `SafeKeeperConf`, dropped `wal_backup::delete_timeline` and
friends.

# Phase 19 — Move SK control file from remote-storage blob to YB columns

Replace `YbControlFileStorage` (a 388-byte bincode blob stored at
`safekeeper-control/<tenant>/<timeline>` in `GenericRemoteStorage`)
with a structured row in a new `safekeeper.timeline_state` YB table.

User confirmed **no migration** from the old blob is required — fresh
tables only.

## Proposed schema

```sql
CREATE TABLE safekeeper.timeline_state (
    tenant_id              TEXT     NOT NULL,
    timeline_id            TEXT     NOT NULL,
    timeline_start_lsn     BIGINT   NOT NULL,
    local_start_lsn        BIGINT   NOT NULL,
    commit_lsn             BIGINT   NOT NULL,
    backup_lsn             BIGINT   NOT NULL,
    peer_horizon_lsn       BIGINT   NOT NULL,
    remote_consistent_lsn  BIGINT   NOT NULL,
    pg_version             INT      NOT NULL,
    system_id              BIGINT   NOT NULL,
    wal_seg_size           INT      NOT NULL,
    term                   BIGINT   NOT NULL,
    term_history           JSONB    NOT NULL,        -- Vec<{term, lsn}>
    proposer_uuid          BYTEA    NOT NULL,        -- 16 bytes
    mconf                  JSONB    NOT NULL,        -- membership Configuration
    partial_backup         JSONB    NOT NULL,
    eviction_state         JSONB    NOT NULL,        -- {"Present"} | {"Offloaded": lsn}
    creation_ts            TIMESTAMPTZ NOT NULL,
    PRIMARY KEY ((tenant_id, timeline_id) HASH)
);
```

## Rationale

- Hot-update scalars (LSN fields) get their own columns so a control-file
  save is a single column-scoped UPSERT, not a 400-byte blob rewrite.
- Slow-update structures (`mconf`, `term_history`, server info) go in
  JSONB so we don't have to evolve SQL columns when their internal
  shape changes upstream.
- Hash on `(tenant_id, timeline_id)` keeps the row on one tablet (same
  pattern as `pageserver.timeline_state`).

## Work items

- Add the table to
  `storage_controller/migrations/00000000000001_initial_schema/up.sql`.
- Replace `YbControlFileStorage` (in `safekeeper/src/control_file.rs`)
  with a struct that READs by SELECTing this row and serializes /
  deserializes by mapping `TimelinePersistentState` ↔ columns (JSONB
  for nested structs).
- `persist()` becomes an UPSERT (UPDATE with full row payload) instead
  of a blob upload.
- Drop the `safekeeper-control/...` path code, the
  `RemotePath::from_string("safekeeper-control/...")`, the
  checksum + magic + version framing (DB-side schema versioning handles
  it now).
- Verify e2e: cargo test -p safekeeper --lib, then
  `test/e2e/restart-wal-replay-yb.sh`.

When dropping fields whose semantics are dead in yb-mode
(`backup_lsn` / `partial_backup`), do that as a follow-up — keep them
in this commit for parity with the in-memory state struct.

When peer_horizon_lsn / commit_lsn cleanup lands (see follow-up
commits this session), drop those columns from the schema and the
mapper.

## Notes

- `EvictionState::Offloaded(Lsn)` is effectively dead in yb-mode
  (eviction never trims local WAL because WAL lives in YB rows). Keep
  the column for now; revisit when eviction state itself is removed.
