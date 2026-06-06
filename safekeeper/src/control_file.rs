//! Safekeeper control-file storage.
//!
//! yb-light persists the per-timeline `TimelinePersistentState` as a
//! row in the YB table `safekeeper.timeline_state`. Hot-update LSN
//! scalars get their own BIGINT columns; slow-update structures
//! (`mconf`, `term_history`, `partial_backup`, `eviction_state`) go
//! in JSONB so their internal Rust-side shape can evolve without an
//! SQL migration.
//!
//! Prior history: upstream Neon used a local-FS blob, then yb-light's
//! Phase 2 reduction stored the same bincode blob in
//! `GenericRemoteStorage` (the `YbStore` backend) at the key
//! `safekeeper-control/<tenant>/<timeline>`. Both representations are
//! gone — see commit history and `tasks/19_*.md` for the migration.

use std::future::Future;
use std::ops::Deref;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{Context, Result, bail};
use chrono::{DateTime, Utc};
use postgres_versioninfo::{PgMajorVersion, PgVersionId};
use safekeeper_api::Term;
use safekeeper_api::membership::Configuration;
use serde::{Deserialize, Serialize};
use utils::id::TenantTimelineId;
use utils::lsn::Lsn;
use ysql_client::YsqlClient;
use yb_tokio_postgres::types::ToSql;

use crate::metrics::PERSIST_CONTROL_FILE_SECONDS;
use crate::safekeeper::{AcceptorState, PgUuid, TermHistory};
use crate::state::{EvictionState, TimelinePersistentState};
use crate::wal_backup_partial;

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
    ttid: TenantTimelineId,
    state: TimelinePersistentState,
    last_persist_at: Instant,
}

impl std::fmt::Debug for YbControlFileStorage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("YbControlFileStorage")
            .field("ttid", &self.ttid)
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

const COLUMNS: &str = "timeline_start_lsn, local_start_lsn, backup_lsn, remote_consistent_lsn, \
                       pg_version, system_id, wal_seg_size, term, term_history, proposer_uuid, \
                       mconf, partial_backup, eviction_state, creation_ts";

impl YbControlFileStorage {
    /// Load the row for `ttid`. Errors if no row exists — callers
    /// distinguish "create new" from "load existing" via the
    /// `create_new` constructor below.
    pub async fn restore_new(
        client: Arc<YsqlClient>,
        ttid: &TenantTimelineId,
    ) -> Result<YbControlFileStorage> {
        let sql = format!(
            "SELECT {COLUMNS} FROM safekeeper.timeline_state \
             WHERE tenant_id = $1 AND timeline_id = $2"
        );
        let row = client
            .query_opt(
                &sql,
                &[&ttid.tenant_id.to_string(), &ttid.timeline_id.to_string()],
            )
            .await
            .context("SELECT safekeeper.timeline_state")?
            .with_context(|| format!("control file not found in YB for {ttid}"))?;

        let timeline_start_lsn = Lsn(row.try_get::<_, i64>(0)? as u64);
        let local_start_lsn = Lsn(row.try_get::<_, i64>(1)? as u64);
        let backup_lsn = Lsn(row.try_get::<_, i64>(2)? as u64);
        let remote_consistent_lsn = Lsn(row.try_get::<_, i64>(3)? as u64);
        let pg_version: PgVersionId =
            PgVersionId::from_full_pg_version(row.try_get::<_, i32>(4)? as u32);
        let system_id = row.try_get::<_, i64>(5)? as u64;
        let wal_seg_size = row.try_get::<_, i32>(6)? as u32;
        let term: Term = row.try_get::<_, i64>(7)? as Term;
        let term_history = term_history_from_json(row.try_get::<_, serde_json::Value>(8)?)?;
        let proposer_uuid_bytes: Vec<u8> = row.try_get(9)?;
        let proposer_uuid: PgUuid = proposer_uuid_bytes
            .as_slice()
            .try_into()
            .context("proposer_uuid is not 16 bytes")?;
        let mconf: Configuration = serde_json::from_value(row.try_get::<_, serde_json::Value>(10)?)
            .context("decode mconf JSONB")?;
        let partial_backup: wal_backup_partial::State =
            serde_json::from_value(row.try_get::<_, serde_json::Value>(11)?)
                .context("decode partial_backup JSONB")?;
        let eviction_state: EvictionState =
            serde_json::from_value(row.try_get::<_, serde_json::Value>(12)?)
                .context("decode eviction_state JSONB")?;
        let creation_ts_dt: DateTime<Utc> = row.try_get(13)?;
        let creation_ts: std::time::SystemTime = creation_ts_dt.into();

        let state = TimelinePersistentState {
            tenant_id: ttid.tenant_id,
            timeline_id: ttid.timeline_id,
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
            backup_lsn,
            remote_consistent_lsn,
            partial_backup,
            eviction_state,
            creation_ts,
        };

        Ok(YbControlFileStorage {
            client,
            ttid: *ttid,
            state,
            last_persist_at: Instant::now(),
        })
    }

    /// Create + insert a fresh row.
    pub async fn create_new(
        client: Arc<YsqlClient>,
        ttid: &TenantTimelineId,
        state: TimelinePersistentState,
    ) -> Result<YbControlFileStorage> {
        assert!(matches!(state.eviction_state, EvictionState::Present));
        let mut store = YbControlFileStorage {
            client,
            ttid: *ttid,
            state: state.clone(),
            last_persist_at: Instant::now(),
        };
        store.write_row(&state, /* insert= */ true).await?;
        Ok(store)
    }

    /// Delete the row for `ttid`. Used by the timeline-delete flow.
    pub async fn delete(client: &YsqlClient, ttid: &TenantTimelineId) -> Result<()> {
        let sql =
            "DELETE FROM safekeeper.timeline_state WHERE tenant_id = $1 AND timeline_id = $2";
        client
            .execute(
                sql,
                &[&ttid.tenant_id.to_string(), &ttid.timeline_id.to_string()],
            )
            .await
            .context("DELETE safekeeper.timeline_state")?;
        Ok(())
    }

    async fn write_row(&self, s: &TimelinePersistentState, insert: bool) -> Result<()> {
        let pg_version_i32: i32 = s.server.pg_version.as_u32() as i32;
        let proposer_uuid: &[u8] = &s.proposer_uuid[..];
        let creation_ts: DateTime<Utc> = s.creation_ts.into();
        let mconf_json = serde_json::to_value(&s.mconf).context("encode mconf JSONB")?;
        let partial_backup_json =
            serde_json::to_value(&s.partial_backup).context("encode partial_backup JSONB")?;
        let eviction_state_json =
            serde_json::to_value(&s.eviction_state).context("encode eviction_state JSONB")?;
        let term_history_json = term_history_to_json(&s.acceptor_state.term_history);

        let tenant_id = self.ttid.tenant_id.to_string();
        let timeline_id = self.ttid.timeline_id.to_string();
        let params: [&(dyn ToSql + Sync); 16] = [
            &tenant_id,
            &timeline_id,
            &(s.timeline_start_lsn.0 as i64),
            &(s.local_start_lsn.0 as i64),
            &(s.backup_lsn.0 as i64),
            &(s.remote_consistent_lsn.0 as i64),
            &pg_version_i32,
            &(s.server.system_id as i64),
            &(s.server.wal_seg_size as i32),
            &(s.acceptor_state.term as i64),
            &term_history_json,
            &proposer_uuid,
            &mconf_json,
            &partial_backup_json,
            &eviction_state_json,
            &creation_ts,
        ];

        let sql = if insert {
            "INSERT INTO safekeeper.timeline_state (\
                 tenant_id, timeline_id, timeline_start_lsn, local_start_lsn, \
                 backup_lsn, remote_consistent_lsn, pg_version, system_id, \
                 wal_seg_size, term, term_history, proposer_uuid, mconf, \
                 partial_backup, eviction_state, creation_ts) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, \
                     $14, $15, $16)"
                .to_string()
        } else {
            "UPDATE safekeeper.timeline_state SET \
                 timeline_start_lsn    = $3, \
                 local_start_lsn       = $4, \
                 backup_lsn            = $5, \
                 remote_consistent_lsn = $6, \
                 pg_version            = $7, \
                 system_id             = $8, \
                 wal_seg_size          = $9, \
                 term                  = $10, \
                 term_history          = $11, \
                 proposer_uuid         = $12, \
                 mconf                 = $13, \
                 partial_backup        = $14, \
                 eviction_state        = $15, \
                 creation_ts           = $16 \
             WHERE tenant_id           = $1 \
               AND timeline_id         = $2"
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

