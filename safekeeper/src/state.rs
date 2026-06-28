//! Defines per timeline data stored persistently (SafeKeeperPersistentState)
//! and its wrapper with in memory layer (SafekeeperState).

use std::ops::Deref;
use std::time::SystemTime;

use anyhow::{Result, bail};
use postgres_ffi::WAL_SEGMENT_SIZE;
use postgres_versioninfo::{PgMajorVersion, PgVersionId};
use safekeeper_api::membership::Configuration;
use safekeeper_api::{INITIAL_TERM, ServerInfo};
use serde::{Deserialize, Serialize};
use tracing::info;
use utils::id::BranchId;
use utils::lsn::Lsn;

use crate::control_file;
use crate::safekeeper::{AcceptorState, PgUuid, TermHistory, TermLsn, UNKNOWN_SERVER_VERSION};
use crate::timeline::TimelineError;

/// Persistent information stored on safekeeper node about timeline.
/// Serialised to `safekeeper.timeline_state` columns (see
/// `crate::control_file::YbControlFileStorage`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct TimelinePersistentState {
    #[serde(with = "hex")]
    pub branch_id: BranchId,
    /// Membership configuration.
    pub mconf: Configuration,
    /// persistent acceptor state
    pub acceptor_state: AcceptorState,
    /// information about server
    pub server: ServerInfo,
    /// Unique id of the last *elected* proposer we dealt with. Not needed
    /// for correctness, exists for monitoring purposes.
    #[serde(with = "hex")]
    pub proposer_uuid: PgUuid,
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
    /// needs `start_lsn`. Term history is seeded when `start_lsn` is known.
    pub fn new(
        branch_id: &BranchId,
        mconf: Configuration,
        server_info: ServerInfo,
        start_lsn: Lsn,
    ) -> anyhow::Result<TimelinePersistentState> {
        if server_info.wal_seg_size == 0 {
            bail!(TimelineError::UninitializedWalSegSize(*branch_id));
        }

        if server_info.pg_version == UNKNOWN_SERVER_VERSION {
            bail!(TimelineError::UninitialinzedPgVersion(*branch_id));
        }

        // If we are given with init LSN, initialize term history with it. It
        // ensures that walproposer always must be able to find a common point
        // in histories; if it can't something is corrupted. Not having LSN here
        // is so far left for legacy case where timeline is created by compute
        // and LSN during creation is not known yet.
        let term_history = if start_lsn != Lsn::INVALID {
            TermHistory(vec![TermLsn {
                term: INITIAL_TERM,
                lsn: start_lsn,
            }])
        } else {
            TermHistory::empty()
        };

        Ok(TimelinePersistentState {
            branch_id: *branch_id,
            mconf,
            acceptor_state: AcceptorState {
                term: INITIAL_TERM,
                term_history,
            },
            server: server_info,
            proposer_uuid: [0; 16],
            timeline_start_lsn: start_lsn,
            local_start_lsn: start_lsn,
            remote_consistent_lsn: Lsn(0),
            creation_ts: SystemTime::now(),
        })
    }

    pub fn empty() -> Self {
        TimelinePersistentState::new(
            &BranchId::from_array([0u8; 16]),
            Configuration::empty(),
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
    #[serde(with = "hex")]
    pub proposer_uuid: PgUuid,
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
                proposer_uuid: state.proposer_uuid,
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
        s.proposer_uuid = self.inmem.proposer_uuid;
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
        self.inmem.proposer_uuid = s.proposer_uuid;
        Ok(())
    }

    /// Flush in memory values.
    pub async fn flush(&mut self) -> Result<()> {
        let s = self.start_change();
        self.finish_change(&s).await
    }

    /// Switch into membership configuration `to` if it is higher than the
    /// current one. Called from `handle_elected` in the walproposer protocol;
    /// in single-SK yb-light `to.generation` is always equal to the current
    /// generation so this is effectively a no-op.
    pub async fn membership_switch(&mut self, to: Configuration) -> Result<()> {
        if to.generation <= self.mconf.generation {
            info!(
                "ignoring request to switch membership conf to {}, current conf {}",
                to, self.mconf
            );
            return Ok(());
        }
        let before = self.mconf.clone();
        let mut state = self.start_change();
        state.mconf = to.clone();
        self.finish_change(&state).await?;
        info!("switched membership conf to {} from {}", to, before);
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
