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
/// Serialised to `safekeeper.timeline_state` columns (see
/// `crate::control_file::YbControlFileStorage`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TimelinePersistentState {
    #[serde(with = "hex")]
    pub branch_id: BranchId,
    /// information about server
    pub server: ServerInfo,
    /// Since which LSN this timeline generally starts. Safekeeper might have
    /// joined later.
    pub timeline_start_lsn: Lsn,
    /// Since which LSN safekeeper has (had) WAL for this timeline.
    /// All WAL segments next to one containing local_start_lsn are
    /// filled with data from the beginning.
    pub local_start_lsn: Lsn,
    /// LSN of the oldest known checkpoint made by pageserver. We don't
    /// remove WAL beyond it. The pageserver advertises this via
    /// `PageserverFeedback.remote_consistent_lsn`.
    pub remote_consistent_lsn: Lsn,
    pub creation_ts: SystemTime,
}

impl TimelinePersistentState {
    /// yb-light: `commit_lsn` collapsed into `flush_lsn`, so creation only
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
            local_start_lsn: start_lsn,
            remote_consistent_lsn: Lsn(0),
            creation_ts: SystemTime::now(),
        })
    }

    pub fn empty() -> Self {
        TimelinePersistentState::new(
            &BranchId::from_array([0u8; 16]),
            ServerInfo {
                pg_version: PgVersionId::from(PgMajorVersion::PG17),
                system_id: 0, /* Postgres system identifier */
                wal_seg_size: WAL_SEGMENT_SIZE as u32,
            },
            Lsn::INVALID,
        )
        .unwrap()
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
// In memory safekeeper state. Fields mirror ones in `SafeKeeperPersistentState`; values
// are not flushed yet.
pub struct TimelineMemState {
    pub remote_consistent_lsn: Lsn,
}

/// Safekeeper persistent state plus in memory layer.
///
/// Allows us to avoid frequent fsyncs when we update fields like commit_lsn
/// which don't need immediate persistence. Provides transactional like API
/// to atomically update the state.
///
/// Implements Deref into *persistent* part.
pub struct TimelineState<CTRL: control_file::Storage> {
    pub inmem: TimelineMemState,
    pub pers: CTRL, // persistent
}

impl<CTRL> TimelineState<CTRL>
where
    CTRL: control_file::Storage,
{
    pub fn new(state: CTRL) -> Self {
        TimelineState {
            inmem: TimelineMemState {
                remote_consistent_lsn: state.remote_consistent_lsn,
            },
            pers: state,
        }
    }

    /// Start atomic change. Returns SafeKeeperPersistentState with in memory
    /// values applied; the protocol is to 1) change returned struct as desired
    /// 2) atomically persist it with finish_change.
    pub fn start_change(&self) -> TimelinePersistentState {
        let mut s = self.pers.clone();
        s.remote_consistent_lsn = self.inmem.remote_consistent_lsn;
        s
    }

    /// Persist given state. c.f. start_change.
    pub async fn finish_change(&mut self, s: &TimelinePersistentState) -> Result<()> {
        if s.eq(&*self.pers) {
            // nothing to do if state didn't change
        } else {
            self.pers.persist(s).await?;
        }

        // keep in memory values up to date
        self.inmem.remote_consistent_lsn = s.remote_consistent_lsn;
        Ok(())
    }

    /// Flush in memory values.
    pub async fn flush(&mut self) -> Result<()> {
        let s = self.start_change();
        self.finish_change(&s).await
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
