//! Safekeeper control-file storage.
//!
//! yb-amp persists the per-timeline `TimelinePersistentState` as a
//! row in the YB table `amp_storage.branch_state`, one BIGINT/INT
//! column per scalar field.

use std::future::Future;
use std::ops::Deref;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use postgres_versioninfo::PgVersionId;
use utils::id::BranchId;
use utils::lsn::Lsn;
use yb_tokio_postgres::types::ToSql;
use ysql_client::YsqlClient;

use crate::metrics::PERSIST_CONTROL_FILE_SECONDS;
use crate::state::TimelinePersistentState;

/// Storage should keep actual state inside of it. It should implement
/// `Deref` so callers can read fields off the state directly, and
/// provide `persist` for atomic updates.
pub trait Storage: Deref<Target = TimelinePersistentState> {
    /// Persist safekeeper state and update internal state.
    fn persist(&mut self, s: &TimelinePersistentState) -> impl Future<Output = Result<()>> + Send;

    /// Timestamp of last persist.
    fn last_persist_at(&self) -> Instant;
}

/// Control-file storage backed by a YB row in
/// `amp_storage.branch_state`. One instance per timeline; holds a
/// shared `YsqlClient` plus the in-memory `TimelinePersistentState`
/// it last wrote.
pub struct YbControlFileStorage {
    client: Arc<YsqlClient>,
    branch_id: BranchId,
    state: TimelinePersistentState,
    last_persist_at: Instant,
}

impl std::fmt::Debug for YbControlFileStorage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("YbControlFileStorage")
            .field("branch_id", &self.branch_id)
            .finish()
    }
}

const COLUMNS: &str = "timeline_start_lsn, remote_consistent_lsn, \
                       pg_version, system_id, wal_seg_size, creation_ts";

impl YbControlFileStorage {
    /// Load our control file from the branch's `amp_storage.branch_state`
    /// row, or `Ok(None)` if we haven't written it yet.
    ///
    /// The row is provisioned by the controller at branch-create time and the
    /// pageserver owns a separate group of columns on it, so **row presence is
    /// not the signal**: `safekeeper_initialized` is what says our own group is
    /// populated. Absence is a normal outcome, not an error — on a fresh branch
    /// the group is filled by [`Self::write_initial_state`] on the first
    /// walproposer handshake, so startup has to tell "not written yet" apart
    /// from a genuine read failure.
    ///
    /// The flag replaces a `timeline_start_lsn IS NOT NULL` test. That worked
    /// only because our group happens to have a single writer — the pageserver
    /// made the same bet on `consistent_lsn`, which does not, and silently lost
    /// its creation write (#552). An explicit flag doesn't depend on the bet,
    /// and it keeps the non-nullable `try_get`s below honest: `write_row` sets
    /// the flag in the same statement that fills all five columns.
    pub async fn restore_opt(
        client: Arc<YsqlClient>,
        branch_id: &BranchId,
    ) -> Result<Option<YbControlFileStorage>> {
        let sql = format!(
            "SELECT {COLUMNS} FROM amp_storage.branch_state \
             WHERE branch_id = $1 AND safekeeper_initialized"
        );
        let Some(row) = client
            .query_opt(&sql, &[&branch_id.to_string()])
            .await
            .context("SELECT amp_storage.branch_state")?
        else {
            return Ok(None);
        };

        let timeline_start_lsn = Lsn(row.try_get::<_, i64>(0)? as u64);
        let remote_consistent_lsn = Lsn(row.try_get::<_, i64>(1)? as u64);
        let pg_version: PgVersionId =
            PgVersionId::from_full_pg_version(row.try_get::<_, i32>(2)? as u32);
        let system_id = row.try_get::<_, i64>(3)? as u64;
        let wal_seg_size = row.try_get::<_, i32>(4)? as u32;
        let creation_ts_dt: DateTime<Utc> = row.try_get(5)?;
        let creation_ts: std::time::SystemTime = creation_ts_dt.into();

        let state = TimelinePersistentState {
            branch_id: *branch_id,
            server: safekeeper_api::ServerInfo {
                pg_version,
                system_id,
                wal_seg_size,
            },
            timeline_start_lsn,
            remote_consistent_lsn,
            creation_ts,
        };

        Ok(Some(YbControlFileStorage {
            client,
            branch_id: *branch_id,
            state,
            last_persist_at: Instant::now(),
        }))
    }

    /// Populate the safekeeper's column group on the branch's
    /// `amp_storage.branch_state` row.
    ///
    /// The controller provisions the row at branch-create time, before any
    /// safekeeper or pageserver starts, so this only ever UPDATEs — it never
    /// creates the row, and it leaves the pageserver's columns alone.
    pub async fn write_initial_state(
        client: Arc<YsqlClient>,
        branch_id: &BranchId,
        state: TimelinePersistentState,
    ) -> Result<YbControlFileStorage> {
        let store = YbControlFileStorage {
            client,
            branch_id: *branch_id,
            state: state.clone(),
            last_persist_at: Instant::now(),
        };
        store.write_row(&state, /* create= */ true).await?;
        Ok(store)
    }

    /// UPDATE our column group, flipping `safekeeper_initialized` with it —
    /// this is that flag's only writer, so it means exactly "these six columns
    /// are populated". `create` adds `NOT safekeeper_initialized` so the first
    /// write fills the group exactly once; a later `persist` rewrites it
    /// unconditionally. Either way the row must already exist — if the UPDATE
    /// matches nothing on create, we distinguish "already populated" (fine)
    /// from "no row" (the controller never provisioned this branch).
    async fn write_row(&self, s: &TimelinePersistentState, create: bool) -> Result<()> {
        let creation_ts: DateTime<Utc> = s.creation_ts.into();

        let branch_id = self.branch_id.to_string();
        let params: [&(dyn ToSql + Sync); 6] = [
            &branch_id,
            &(s.timeline_start_lsn.0 as i64),
            &(s.remote_consistent_lsn.0 as i64),
            &(s.server.system_id as i64),
            &(s.server.wal_seg_size as i32),
            &creation_ts,
        ];

        // `pg_version` is deliberately not written here. The controller seeds it
        // with the full `PgVersionId` at branch create and is its only writer;
        // we read it back in `restore_opt`, and `handle_greeting` compares
        // *major* versions only, so there is nothing for us to refine.
        let set_clause = "UPDATE amp_storage.branch_state SET \
                 timeline_start_lsn    = $2, \
                 remote_consistent_lsn = $3, \
                 system_id             = $4, \
                 wal_seg_size          = $5, \
                 creation_ts           = $6, \
                 safekeeper_initialized = true \
             WHERE branch_id           = $1";
        let sql = if create {
            format!("{set_clause} AND NOT safekeeper_initialized")
        } else {
            set_clause.to_string()
        };

        let n = self
            .client
            .execute(&sql, &params)
            .await
            .context("UPDATE amp_storage.branch_state")?;
        if create && n == 0 {
            // Either our group is already populated (a concurrent/replayed
            // create — harmless) or the row was never provisioned, which is a
            // wiring bug worth failing on.
            let found = self
                .client
                .query_opt(
                    "SELECT 1 FROM amp_storage.branch_state WHERE branch_id = $1",
                    &[&branch_id],
                )
                .await
                .context("branch_state existence probe")?;
            if found.is_none() {
                anyhow::bail!(
                    "no branch_state row for {} — the controller provisions it at \
                     branch create, so the safekeeper started against an \
                     unprovisioned branch",
                    self.branch_id
                );
            }
        }
        Ok(())
    }
}

/// Client + branch id cloned out of a [`YbControlFileStorage`] so the
/// timeline manager can persist the advisory control-file field
/// without holding the timeline `shared_state`
/// lock. Holding that lock across a YB round trip freezes WAL appends
/// and starves residence-guard issuance (the 2026-07 TPC-C safekeeper
/// wedge, second occurrence: manager stuck in `UpdateControlFile`).
#[derive(Clone)]
pub struct AdvisoryPersist {
    client: Arc<YsqlClient>,
    branch_id: BranchId,
}

impl AdvisoryPersist {
    /// Persist `remote_consistent_lsn` with a narrow, monotonic UPDATE.
    ///
    /// `GREATEST()` makes the statement safe under any interleaving
    /// with the full-row persists issued under the shared_state lock
    /// (`handle_elected`, `map_control_file`): a late-landing advisory
    /// write can never regress the row, and it touches none of the
    /// other columns.
    ///
    /// Scoped to `safekeeper_initialized` so it can only ever refine a group
    /// that exists. Unscoped it was the one write that could put a value into
    /// our group before the creation write ran — the same shape as the
    /// pageserver's `advance_consistent_lsn`, which is what dropped that
    /// daemon's creation write in #552. Here it never falsified the old
    /// `timeline_start_lsn` discriminator (it doesn't touch that column), so it
    /// was latent rather than live; the flag closes it either way.
    pub async fn persist_remote_consistent_lsn(&self, lsn: Lsn) -> Result<()> {
        let _timer = PERSIST_CONTROL_FILE_SECONDS.start_timer();
        self.client
            .execute(
                "UPDATE amp_storage.branch_state \
                 SET remote_consistent_lsn = GREATEST(remote_consistent_lsn, $2) \
                 WHERE branch_id = $1 AND safekeeper_initialized",
                &[&self.branch_id.to_string(), &(lsn.0 as i64)],
            )
            .await
            .context("UPDATE amp_storage.branch_state (advisory)")?;
        Ok(())
    }
}

impl YbControlFileStorage {
    /// Handle for lock-free advisory persists; see [`AdvisoryPersist`].
    pub fn advisory_persist_handle(&self) -> AdvisoryPersist {
        AdvisoryPersist {
            client: self.client.clone(),
            branch_id: self.branch_id,
        }
    }

    /// Fold a completed [`AdvisoryPersist`] write back into the
    /// in-memory mirror of the row. `max()` because a full-row persist
    /// (e.g. on ProposerElected) may have advanced the mirror while the
    /// advisory write was in flight.
    pub fn note_advisory_persisted(&mut self, lsn: Lsn) {
        self.state.remote_consistent_lsn = std::cmp::max(self.state.remote_consistent_lsn, lsn);
        self.last_persist_at = Instant::now();
    }
}

impl Deref for YbControlFileStorage {
    type Target = TimelinePersistentState;
    fn deref(&self) -> &Self::Target {
        &self.state
    }
}

impl Storage for YbControlFileStorage {
    async fn persist(&mut self, s: &TimelinePersistentState) -> Result<()> {
        let _timer = PERSIST_CONTROL_FILE_SECONDS.start_timer();
        self.write_row(s, /* create= */ false).await?;
        self.state = s.clone();
        // Reset the save-interval clock: without this the manager
        // re-persisted every loop iteration (300ms) once the first
        // interval elapsed, hammering YB with row UPDATEs for as long
        // as remote_consistent_lsn kept advancing.
        self.last_persist_at = Instant::now();
        Ok(())
    }

    fn last_persist_at(&self) -> Instant {
        self.last_persist_at
    }
}

/// Control-file storage. Only the YB-backed implementation exists;
/// kept as a type alias so call sites read uniformly with the rest of
/// the code that holds a `Storage`-trait value generically.
pub type ControlFileStorage = YbControlFileStorage;
