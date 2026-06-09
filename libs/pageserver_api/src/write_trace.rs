//! Env-gated per-key trace of the WAL→YB write pipeline.
//!
//! Set `YB_PS_TRACE_REL_BLK=sp/db/rel/fork/blk` to emit a
//! `target: yb_ps_trace, stage = "<...>"` log line every time a row
//! matching the filter crosses a pipeline boundary. `blk` may be `*`
//! to match every block of the rel/fork. The filter is parsed once
//! lazily; when unset every call site is one `Option::is_none()` check.
//!
//! Stages emitted (in pipeline order):
//!
//! - `decode`        — WAL decoder added the row to the per-shard batch.
//! - `put`           — `PageWriter::put` pushed it into `UnifiedPages`.
//! - `snapshot`      — `PageWriter::flush` captured it in the unflushed
//!                     snapshot it is about to send to YB.
//! - `put_pages_in`  — `PageStore::put_pages` is about to INSERT this
//!                     row's chunk.
//! - `put_pages_ok`  — the chunk's INSERT returned success.
//! - `durable`       — `mark_flushed` flipped the cache slot durable.
//!
//! A row that emits `decode` but not `put_pages_ok` was dropped before
//! YB saw it. A row that emits `put_pages_ok` but is missing from YB
//! later was lost by YB itself (or by some path after the INSERT).

use std::sync::OnceLock;

use utils::lsn::Lsn;

use crate::key::Key;

#[derive(Clone, Debug)]
struct TraceFilter {
    spcnode: u32,
    dbnode: u32,
    relnode: u32,
    forknum: u8,
    /// `None` matches every block of the rel/fork.
    blknum: Option<u32>,
}

fn parse_filter(env: &str) -> Option<TraceFilter> {
    // sp/db/rel/fork/blk  with  blk = `*` matching any block.
    let parts: Vec<&str> = env.split('/').collect();
    if parts.len() != 5 {
        return None;
    }
    Some(TraceFilter {
        spcnode: parts[0].parse().ok()?,
        dbnode: parts[1].parse().ok()?,
        relnode: parts[2].parse().ok()?,
        forknum: parts[3].parse().ok()?,
        blknum: if parts[4] == "*" {
            None
        } else {
            Some(parts[4].parse().ok()?)
        },
    })
}

fn filter() -> Option<&'static TraceFilter> {
    static F: OnceLock<Option<TraceFilter>> = OnceLock::new();
    F.get_or_init(|| std::env::var("YB_PS_TRACE_REL_BLK").ok().as_deref().and_then(parse_filter))
        .as_ref()
}

/// `true` iff the trace filter is set AND this key matches it.
/// One atomic load when the filter is unset — safe to call on every row.
#[inline]
pub fn is_traced(key: &Key) -> bool {
    let Some(f) = filter() else {
        return false;
    };
    if !key.is_rel_block_key() {
        return false;
    }
    let Ok((rel, blknum)) = key.to_rel_block() else {
        return false;
    };
    rel.spcnode == f.spcnode
        && rel.dbnode == f.dbnode
        && rel.relnode == f.relnode
        && rel.forknum == f.forknum
        && f.blknum.is_none_or(|b| b == blknum)
}

/// Emit one trace event if `key` matches the filter. `stage` should be
/// a fixed literal — it ends up in the `stage` field on the log line
/// so dashboards / `grep` can pivot on it.
#[inline]
pub fn trace_stage(stage: &'static str, key: &Key, lsn: Lsn) {
    if is_traced(key) {
        tracing::info!(
            target: "yb_ps_trace",
            stage,
            key = %key,
            lsn = %lsn,
            "write_trace"
        );
    }
}

/// Same as [`trace_stage`] but takes the 18-byte BYTEA key encoding
/// used by `PageRow.key`. Skips immediately if the filter is unset.
#[inline]
pub fn trace_stage_bytes(stage: &'static str, key_bytes: &[u8], lsn: Lsn) {
    if filter().is_none() {
        return;
    }
    if key_bytes.len() != crate::key::KEY_SIZE {
        return;
    }
    let key = Key::from_slice(key_bytes);
    trace_stage(stage, &key, lsn);
}

/// Predicate variant of [`is_traced`] that takes the 18-byte BYTEA
/// key encoding `PageRow.key` carries. Returns `false` immediately
/// when the filter is unset.
#[inline]
pub fn is_traced_bytes(key_bytes: &[u8]) -> bool {
    if filter().is_none() {
        return false;
    }
    if key_bytes.len() != crate::key::KEY_SIZE {
        return false;
    }
    is_traced(&Key::from_slice(key_bytes))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_explicit_block() {
        let f = parse_filter("1663/16384/16649/0/39662").unwrap();
        assert_eq!(f.spcnode, 1663);
        assert_eq!(f.dbnode, 16384);
        assert_eq!(f.relnode, 16649);
        assert_eq!(f.forknum, 0);
        assert_eq!(f.blknum, Some(39662));
    }

    #[test]
    fn parse_wildcard_block() {
        let f = parse_filter("1663/16384/16649/0/*").unwrap();
        assert_eq!(f.blknum, None);
    }

    #[test]
    fn parse_rejects_malformed() {
        assert!(parse_filter("1663/16384/16649").is_none());
        assert!(parse_filter("notnum/16384/16649/0/0").is_none());
        assert!(parse_filter("").is_none());
    }
}
