//! Defines per timeline data stored persistently (SafeKeeperPersistentState)
//! and its wrapper with in memory layer (SafekeeperState).

use std::ops::Deref;
use std::time::SystemTime;

use anyhow::{Result, bail};
use postgres_ffi::WAL_SEGMENT_SIZE;
use postgres_versioninfo::{PgMajorVersion, PgVersionId};
use safekeeper_api::ServerInfo;
use serde::{Deserialize, Serialize};
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::control_file;
use crate::safekeeper::UNKNOWN_SERVER_VERSION;
use crate::timeline::TimelineError;

/// Persistent information stored on safekeeper node about timeline.
/// Serialised to `amp_storage.branch_state` columns (see
/// `crate::control_file::YbControlFileStorage`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TimelinePersistentState {
    #[serde(with = "hex")]
    pub branch_id: BranchId,
    /// information about server
    pub server: ServerInfo,
    /// Since which LSN this timeline starts: the branch point, latched at
    /// creation or on the first `ProposerElected`. Persisted WAL begins here.
    pub timeline_start_lsn: Lsn,
    pub creation_ts: SystemTime,
}

impl TimelinePersistentState {
    /// yb-amp: `commit_lsn` collapsed into `flush_lsn`, so creation only
    /// needs `start_lsn`.
    pub fn new(
        branch_id: &BranchId,
        server_info: ServerInfo,
        start_lsn: Lsn,
    ) -> anyhow::Result<TimelinePersistentState> {
        if server_info.wal_seg_size == 0 {
            bail!(TimelineError::UninitializedWalSegSize(*branch_id));
        }

        if server_info.pg_version == UNKNOWN_SERVER_VERSION {
            bail!(TimelineError::UninitialinzedPgVersion(*branch_id));
        }

        Ok(TimelinePersistentState {
            branch_id: *branch_id,
            server: server_info,
            timeline_start_lsn: start_lsn,
            creation_ts: SystemTime::now(),
        })
    }

    pub fn empty() -> Self {
        TimelinePersistentState::new(
            &BranchId::from_array([0u8; 16]),
            ServerInfo {
                pg_version: PgVersionId::from(PgMajorVersion::PG18),
                system_id: 0, /* Postgres system identifier */
                wal_seg_size: WAL_SEGMENT_SIZE as u32,
            },
            Lsn::INVALID,
        )
        .unwrap()
    }
}

/// Safekeeper state: the persisted control-file row plus the one value the
/// safekeeper tracks in memory.
///
/// Implements Deref into the *persistent* part.
pub struct TimelineState<CTRL: control_file::Storage> {
    /// The WAL-retention floor: the pageserver's durable-ingest frontier, and
    /// the bound on WAL GC.
    ///
    /// Deliberately not persisted here. The pageserver already records it
    /// authoritatively in `amp_storage.branch_state.consistent_lsn`, which it
    /// writes — generation-fenced — *before* echoing the same value back as
    /// `PageserverFeedback.disk_consistent_lsn`. So every value this holds is
    /// one the pageserver has already committed: seeded from that column at
    /// startup, advanced by feedback thereafter, and a crash can only resume at
    /// or above where it left off.
    pub consistent_lsn: Lsn,
    pub pers: CTRL,
}

impl<CTRL> TimelineState<CTRL>
where
    CTRL: control_file::Storage,
{
    /// `retention_floor` seeds [`Self::consistent_lsn`] from the pageserver's
    /// `branch_state` column, floored at `timeline_start_lsn` — which is where
    /// it lands when that daemon has not written it yet, so all WAL is retained
    /// until the first feedback.
    pub fn new(state: CTRL, retention_floor: Lsn) -> Self {
        TimelineState {
            consistent_lsn: retention_floor,
            pers: state,
        }
    }

    /// Snapshot the persisted state for editing; pair with `finish_change`.
    pub fn start_change(&self) -> TimelinePersistentState {
        self.pers.clone()
    }

    /// Write `s` back, skipping the round trip when nothing changed.
    pub async fn finish_change(&mut self, s: &TimelinePersistentState) -> Result<()> {
        if !s.eq(&*self.pers) {
            self.pers.persist(s).await?;
        }
        Ok(())
    }
}

impl<CTRL> Deref for TimelineState<CTRL>
where
    CTRL: control_file::Storage,
{
    type Target = TimelinePersistentState;

    fn deref(&self) -> &Self::Target {
        &self.pers
    }
}
