//! Safekeeper control-file storage.
//!
//! yb-light persists the per-timeline `TimelinePersistentState` as a
//! row in the YB table `safekeeper.timeline_state`. Hot-update LSN
//! scalars get their own BIGINT columns; slow-update structures
//! (`mconf`, `term_history`) go in JSONB so their internal Rust-side
//! shape can evolve without an SQL migration.
//!
//! Prior history: upstream Neon used a local-FS blob, then yb-light's
//! Phase 2 reduction stored the same bincode blob in
//! `GenericRemoteStorage` (the `YbStore` backend) at the key
//! `safekeeper-control/<branch_id>`. Both representations are
//! gone — see commit history and #10 for the migration.

use std::future::Future;
use std::ops::Deref;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use postgres_versioninfo::PgVersionId;
use safekeeper_api::Term;
use safekeeper_api::membership::Configuration;
use serde::{Deserialize, Serialize};
use utils::id::BranchId;
use utils::lsn::Lsn;
use yb_tokio_postgres::types::ToSql;
use ysql_client::YsqlClient;

use crate::metrics::PERSIST_CONTROL_FILE_SECONDS;
use crate::safekeeper::{AcceptorState, PgUuid, TermHistory};
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
/// `safekeeper.timeline_state`. One instance per timeline; holds a
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

/// Wire shape for the `term_history` JSONB column.
#[derive(Serialize, Deserialize)]
struct TermSwitchJson {
    term: Term,
    lsn: u64,
}

fn term_history_to_json(th: &TermHistory) -> serde_json::Value {
    serde_json::to_value(
        th.0.iter()
            .map(|e| TermSwitchJson {
                term: e.term,
                lsn: e.lsn.0,
            })
            .collect::<Vec<_>>(),
    )
    .expect("TermSwitchJson serializes")
}

fn term_history_from_json(v: serde_json::Value) -> Result<TermHistory> {
    let entries: Vec<TermSwitchJson> =
        serde_json::from_value(v).context("decode term_history JSONB")?;
    Ok(TermHistory(
        entries
            .into_iter()
            .map(|e| crate::safekeeper::TermLsn {
                term: e.term,
                lsn: Lsn(e.lsn),
            })
            .collect(),
    ))
}

const COLUMNS: &str = "timeline_start_lsn, local_start_lsn, remote_consistent_lsn, \
                       pg_version, system_id, wal_seg_size, term, term_history, proposer_uuid, \
                       mconf, creation_ts";

impl YbControlFileStorage {
    /// Load the row for `branch_id`. Errors if no row exists — callers
    /// distinguish "create new" from "load existing" via the
    /// `create_new` constructor below.
    pub async fn restore_new(
        client: Arc<YsqlClient>,
        branch_id: &BranchId,
    ) -> Result<YbControlFileStorage> {
        let sql = format!(
            "SELECT {COLUMNS} FROM safekeeper.timeline_state \
             WHERE branch_id = $1"
        );
        let row = client
            .query_opt(&sql, &[&branch_id.to_string()])
            .await
            .context("SELECT safekeeper.timeline_state")?
            .with_context(|| format!("control file not found in YB for {branch_id}"))?;

        let timeline_start_lsn = Lsn(row.try_get::<_, i64>(0)? as u64);
        let local_start_lsn = Lsn(row.try_get::<_, i64>(1)? as u64);
        let remote_consistent_lsn = Lsn(row.try_get::<_, i64>(2)? as u64);
        let pg_version: PgVersionId =
            PgVersionId::from_full_pg_version(row.try_get::<_, i32>(3)? as u32);
        let system_id = row.try_get::<_, i64>(4)? as u64;
        let wal_seg_size = row.try_get::<_, i32>(5)? as u32;
        let term: Term = row.try_get::<_, i64>(6)? as Term;
        let term_history = term_history_from_json(row.try_get::<_, serde_json::Value>(7)?)?;
        let proposer_uuid_bytes: Vec<u8> = row.try_get(8)?;
        let proposer_uuid: PgUuid = proposer_uuid_bytes
            .as_slice()
            .try_into()
            .context("proposer_uuid is not 16 bytes")?;
        let mconf: Configuration = serde_json::from_value(row.try_get::<_, serde_json::Value>(9)?)
            .context("decode mconf JSONB")?;
        let creation_ts_dt: DateTime<Utc> = row.try_get(10)?;
        let creation_ts: std::time::SystemTime = creation_ts_dt.into();

        let state = TimelinePersistentState {
            branch_id: *branch_id,
            mconf,
            acceptor_state: AcceptorState { term, term_history },
            server: safekeeper_api::ServerInfo {
                pg_version,
                system_id,
                wal_seg_size,
            },
            proposer_uuid,
            timeline_start_lsn,
            local_start_lsn,
            remote_consistent_lsn,
            creation_ts,
        };

        Ok(YbControlFileStorage {
            client,
            branch_id: *branch_id,
            state,
            last_persist_at: Instant::now(),
        })
    }

    /// Create + insert a fresh row.
    pub async fn create_new(
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
        store.write_row(&state, /* insert= */ true).await?;
        Ok(store)
    }

    /// Enumerate every `branch_id` persisted in
    /// `safekeeper.timeline_state`. Used by SK startup in YB-WAL mode
    /// to re-discover branches after a restart — there's no on-disk
    /// directory to scan and the storage_controller doesn't re-issue
    /// `POST /v1/branch` for existing branches.
    pub async fn list_all_branch_ids(client: &YsqlClient) -> Result<Vec<BranchId>> {
        let rows = client
            .query("SELECT branch_id FROM safekeeper.timeline_state", &[])
            .await
            .context("SELECT safekeeper.timeline_state list")?;
        let mut branch_ids = Vec::with_capacity(rows.len());
        for row in rows {
            let branch_id: String = row.try_get(0).context("read branch_id")?;
            branch_ids.push(
                branch_id
                    .parse()
                    .with_context(|| format!("parse branch_id {branch_id}"))?,
            );
        }
        Ok(branch_ids)
    }

    /// Delete the row for `branch_id`. Used by the timeline-delete flow.
    pub async fn delete(client: &YsqlClient, branch_id: &BranchId) -> Result<()> {
        let sql = "DELETE FROM safekeeper.timeline_state WHERE branch_id = $1";
        client
            .execute(sql, &[&branch_id.to_string()])
            .await
            .context("DELETE safekeeper.timeline_state")?;
        Ok(())
    }

    async fn write_row(&self, s: &TimelinePersistentState, insert: bool) -> Result<()> {
        let pg_version_i32: i32 = s.server.pg_version.as_u32() as i32;
        let proposer_uuid: &[u8] = &s.proposer_uuid[..];
        let creation_ts: DateTime<Utc> = s.creation_ts.into();
        let mconf_json = serde_json::to_value(&s.mconf).context("encode mconf JSONB")?;
        let term_history_json = term_history_to_json(&s.acceptor_state.term_history);

        let branch_id = self.branch_id.to_string();
        let params: [&(dyn ToSql + Sync); 12] = [
            &branch_id,
            &(s.timeline_start_lsn.0 as i64),
            &(s.local_start_lsn.0 as i64),
            &(s.remote_consistent_lsn.0 as i64),
            &pg_version_i32,
            &(s.server.system_id as i64),
            &(s.server.wal_seg_size as i32),
            &(s.acceptor_state.term as i64),
            &term_history_json,
            &proposer_uuid,
            &mconf_json,
            &creation_ts,
        ];

        let sql = if insert {
            "INSERT INTO safekeeper.timeline_state (\
                 branch_id, timeline_start_lsn, local_start_lsn, \
                 remote_consistent_lsn, pg_version, system_id, \
                 wal_seg_size, term, term_history, proposer_uuid, mconf, \
                 creation_ts) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)"
                .to_string()
        } else {
            "UPDATE safekeeper.timeline_state SET \
                 timeline_start_lsn    = $2, \
                 local_start_lsn       = $3, \
                 remote_consistent_lsn = $4, \
                 pg_version            = $5, \
                 system_id             = $6, \
                 wal_seg_size          = $7, \
                 term                  = $8, \
                 term_history          = $9, \
                 proposer_uuid         = $10, \
                 mconf                 = $11, \
                 creation_ts           = $12 \
             WHERE branch_id           = $1"
                .to_string()
        };

        self.client
            .execute(&sql, &params)
            .await
            .context(if insert {
                "INSERT safekeeper.timeline_state"
            } else {
                "UPDATE safekeeper.timeline_state"
            })?;
        Ok(())
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
        self.write_row(s, /* insert= */ false).await?;
        self.state = s.clone();
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
