use utils::lsn::Lsn;

use crate::timeline_manager::StateSnapshot;

/// Oldest LSN we still need to keep.
///
/// yb-light: WAL is durably mirrored into `yb_storage.wals` on every
/// `flush_wal()`, so the only horizon constraints are:
///   1. `remote_consistent_lsn` — pageserver hasn't ingested past this yet.
///   2. `flush_lsn` — local end-of-WAL; we can't trim past durability.
///   3. Optional caller-supplied `extra_horizon_lsn` (e.g. a stalled
///      walsender's position).
///
/// The pre-yb-light "wait for S3 offload" branch (`backup_lsn`) is gone
/// along with the offloader itself — including it would pin horizon at
/// boot and the `wals` table would grow forever.
pub(crate) fn calc_horizon_lsn(state: &StateSnapshot, extra_horizon_lsn: Option<Lsn>) -> Lsn {
    use std::cmp::min;

    let mut horizon_lsn = state.cfile_remote_consistent_lsn;
    // Min by flush_lsn so a stale `remote_consistent_lsn` never
    // licenses trimming past local end-of-WAL.
    horizon_lsn = min(horizon_lsn, state.flush_lsn);
    if let Some(extra_horizon_lsn) = extra_horizon_lsn {
        horizon_lsn = min(horizon_lsn, extra_horizon_lsn);
    }

    horizon_lsn
}
