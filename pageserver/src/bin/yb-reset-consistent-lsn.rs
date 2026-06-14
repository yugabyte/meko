//! Find — and optionally set — the maximum valid `consistent_lsn` at or
//! below a target LSN, for a single (tenant, timeline) in `yb_storage`.
//!
//! `pageserver.timeline_state.consistent_lsn` must point at a *real WAL
//! record boundary*: the byte just after a complete record (= the start
//! of the next record). The pageserver's WAL reader does not scan
//! forward to the next boundary — it starts decoding at exactly
//! `consistent_lsn` and fails (looping forever) if that byte is mid-record
//! or in a WAL hole:
//!
//!   - `YB WAL hole at segno N offset O` — no `safekeeper.wals` chunk covers it.
//!   - `invalid xlog page header: xlp_rem_len=X, expected Y` — chunk exists
//!     but the LSN is in the middle of a multi-page record.
//!
//! So you cannot just pick a segment- or page-aligned LSN to rewind to.
//! This tool derives a guaranteed-good boundary the only reliable way: it
//! anchors on a nearby WAL page that starts a fresh record, replays the
//! real `WalStreamDecoder` forward across the `safekeeper.wals` bytes, and
//! reports the largest record boundary that is `<= target`. The boundary
//! just before it is reported as the matching `prev_record_lsn`.
//!
//! By default it only prints. `--set` writes the pair back to
//! `pageserver.timeline_state` (stop the pageserver for this timeline
//! first — a running one will overwrite this on its next flush, and may
//! be generation-fenced against this write).

use anyhow::{Context, bail};
use clap::Parser;
use postgres_ffi::wal_record_boundary::{WalSource, decode_forward, find_anchor};
use postgres_ffi::{PgMajorVersion, WAL_SEGMENT_SIZE};
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use ysql_client::tokio_postgres::types::ToSql;
use ysql_client::YsqlClient;

#[derive(Parser)]
#[command(
    about = "Find (and optionally set) the max valid pageserver consistent_lsn <= a target LSN"
)]
struct Args {
    /// Tenant id (32 hex chars).
    #[arg(long)]
    tenant: TenantId,
    /// Timeline id (32 hex chars).
    #[arg(long)]
    timeline: TimelineId,
    /// Target max LSN in `X/Y` hex form. The result is the largest real
    /// WAL record boundary `<= target`.
    #[arg(long, value_name = "X/Y")]
    target_lsn: Lsn,

    /// Write the computed pair back to `pageserver.timeline_state`.
    /// Without this the tool only prints (dry run).
    #[arg(long)]
    set: bool,
    /// When writing, store NULL for `prev_record_lsn` and let the
    /// walreceiver re-derive it from the safekeeper stream, instead of
    /// the boundary this tool computed.
    #[arg(long)]
    null_prev: bool,
    /// When writing, fence the UPDATE on this `current_generation`. The
    /// write affects 0 rows (and warns) if the row's generation differs.
    #[arg(long)]
    expect_generation: Option<i32>,

    #[command(flatten)]
    conn: ysql_client::YbConnArgs,
}

/// The subset of `pageserver.timeline_state` this tool reads.
struct TimelineState {
    consistent_lsn: Lsn,
    prev_record_lsn: Option<Lsn>,
    applied_gc_cutoff_lsn: Lsn,
    pg_version: i32,
    current_generation: i32,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let seg = WAL_SEGMENT_SIZE as u64;

    let cfg = args.conn.into_config();
    let client = YsqlClient::from_yb_config(&cfg, "yb-reset-consistent-lsn", false)
        .await
        .context("connecting to yb_storage")?;

    let tenant = args.tenant.to_string();
    let timeline = args.timeline.to_string();

    let state = read_timeline_state(&client, &tenant, &timeline).await?;
    let pg_version = match state.pg_version {
        17 => PgMajorVersion::PG17,
        v => bail!("timeline_state.pg_version is {v}, not a supported PG major version"),
    };

    println!("tenant   {tenant}");
    println!("timeline {timeline}");
    println!("pg_version            {}", state.pg_version);
    println!("current_generation    {}", state.current_generation);
    println!("applied_gc_cutoff_lsn {}", state.applied_gc_cutoff_lsn);
    println!("current consistent_lsn  {}", state.consistent_lsn);
    println!(
        "current prev_record_lsn {}",
        state
            .prev_record_lsn
            .map(|l| l.to_string())
            .unwrap_or_else(|| "NULL".into())
    );
    println!("target_lsn              {}", args.target_lsn);
    println!();

    if args.target_lsn < state.applied_gc_cutoff_lsn {
        eprintln!(
            "WARNING: target {} is below applied_gc_cutoff_lsn {} — WAL there may be garbage-collected",
            args.target_lsn, state.applied_gc_cutoff_lsn
        );
    }

    let reader = WalReader {
        client: &client,
        tenant: &tenant,
        timeline: &timeline,
        seg,
    };

    // 1. Anchor: walk back from target's page to one that starts a fresh
    //    record (XLP_FIRST_IS_CONTRECORD clear). That page boundary is a
    //    guaranteed record boundary the decoder can be seeded at.
    let anchor = find_anchor(&reader, args.target_lsn).await?;
    println!("anchor (decode start)   {anchor}");

    // 2. Replay the real decoder forward, collecting record boundaries.
    let (boundaries, stop) = decode_forward(&reader, anchor, args.target_lsn, pg_version).await?;

    // 3. Largest boundary <= target is the answer; the one before it is
    //    the matching prev_record_lsn.
    let idx = boundaries
        .iter()
        .rposition(|b| *b <= args.target_lsn)
        .context("no WAL record boundary found at or below target (anchor scan/decoder produced none)")?;
    let consistent = boundaries[idx];
    let prev = idx.checked_sub(1).map(|i| boundaries[i]);

    if let Some(reason) = stop {
        println!("note: forward decode stopped early: {reason}");
    }
    println!();
    println!("==> max valid consistent_lsn <= target: {consistent}");
    match prev {
        Some(p) => println!("    matching prev_record_lsn:           {p}"),
        None => println!(
            "    matching prev_record_lsn:           UNKNOWN (anchor was the first boundary) — will write NULL"
        ),
    }
    if consistent == state.consistent_lsn {
        println!("    (equal to the current consistent_lsn — no rewind needed)");
    } else if let Some(behind) = state.consistent_lsn.checked_sub(consistent) {
        println!("    ({} bytes below current consistent_lsn)", behind.0);
    }

    if !args.set {
        println!();
        println!("dry run: pass --set to write this to pageserver.timeline_state");
        return Ok(());
    }

    let prev_to_write: Option<Lsn> = if args.null_prev { None } else { prev };
    eprintln!();
    eprintln!(
        "WARNING: writing consistent_lsn={consistent} prev_record_lsn={} — stop the pageserver \
         for this timeline first; a running one will overwrite this on its next flush.",
        prev_to_write
            .map(|l| l.to_string())
            .unwrap_or_else(|| "NULL".into())
    );

    let affected = write_timeline_state(
        &client,
        &tenant,
        &timeline,
        consistent,
        prev_to_write,
        args.expect_generation,
    )
    .await?;
    if affected == 0 {
        bail!(
            "UPDATE affected 0 rows{}",
            match args.expect_generation {
                Some(g) => format!(
                    " — current_generation != {g} (row is at {}); re-run without --expect-generation \
                     or with the right value",
                    state.current_generation
                ),
                None => " — row not found (tenant/timeline mismatch?)".into(),
            }
        );
    }
    println!();
    println!("done: updated {affected} row(s) in pageserver.timeline_state");
    Ok(())
}

async fn read_timeline_state(
    client: &YsqlClient,
    tenant: &str,
    timeline: &str,
) -> anyhow::Result<TimelineState> {
    let sql = "SELECT consistent_lsn, prev_record_lsn, applied_gc_cutoff_lsn, pg_version, \
               current_generation \
               FROM pageserver.timeline_state WHERE tenant_id = $1 AND timeline_id = $2";
    let params: [&(dyn ToSql + Sync); 2] = [&tenant, &timeline];
    let row = client
        .query_opt(sql, &params)
        .await
        .context("reading pageserver.timeline_state")?
        .with_context(|| format!("no timeline_state row for tenant={tenant} timeline={timeline}"))?;
    let consistent_lsn = Lsn(row.get::<_, i64>(0) as u64);
    let prev_record_lsn = row.get::<_, Option<i64>>(1).map(|v| Lsn(v as u64));
    let applied_gc_cutoff_lsn = Lsn(row.get::<_, i64>(2) as u64);
    let pg_version = row.get::<_, i32>(3);
    let current_generation = row.get::<_, i32>(4);
    Ok(TimelineState {
        consistent_lsn,
        prev_record_lsn,
        applied_gc_cutoff_lsn,
        pg_version,
        current_generation,
    })
}

async fn write_timeline_state(
    client: &YsqlClient,
    tenant: &str,
    timeline: &str,
    consistent: Lsn,
    prev: Option<Lsn>,
    expect_generation: Option<i32>,
) -> anyhow::Result<u64> {
    let consistent_i = consistent.0 as i64;
    let prev_i = prev.map(|l| l.0 as i64);
    if let Some(generation) = expect_generation {
        let sql = "UPDATE pageserver.timeline_state SET consistent_lsn = $3, prev_record_lsn = $4 \
                   WHERE tenant_id = $1 AND timeline_id = $2 AND current_generation = $5";
        let params: [&(dyn ToSql + Sync); 5] =
            [&tenant, &timeline, &consistent_i, &prev_i, &generation];
        client.execute(sql, &params).await.context("UPDATE timeline_state")
    } else {
        let sql = "UPDATE pageserver.timeline_state SET consistent_lsn = $3, prev_record_lsn = $4 \
                   WHERE tenant_id = $1 AND timeline_id = $2";
        let params: [&(dyn ToSql + Sync); 4] = [&tenant, &timeline, &consistent_i, &prev_i];
        client.execute(sql, &params).await.context("UPDATE timeline_state")
    }
}

/// Reads raw WAL bytes by LSN from `safekeeper.wals`, mapping each LSN to
/// `(segno, chunk_offset)` and resolving the chunk that covers it.
struct WalReader<'a> {
    client: &'a YsqlClient,
    tenant: &'a str,
    timeline: &'a str,
    seg: u64,
}

impl WalSource for WalReader<'_> {
    fn seg(&self) -> u64 {
        self.seg
    }

    async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<(Lsn, Vec<u8>)> {
        let segno = (lsn.0 / self.seg) as i64;
        let off = (lsn.0 % self.seg) as i64;
        let sql = "SELECT chunk_offset, data FROM safekeeper.wals \
                   WHERE tenant_id = $1 AND timeline_id = $2 AND segno = $3 AND chunk_offset <= $4 \
                   ORDER BY chunk_offset DESC LIMIT 1";
        let params: [&(dyn ToSql + Sync); 4] = [&self.tenant, &self.timeline, &segno, &off];
        let row = self
            .client
            .query_opt(sql, &params)
            .await
            .context("reading safekeeper.wals")?
            .with_context(|| {
                format!("WAL hole at {lsn} (segno {segno} offset {off}): no covering chunk in safekeeper.wals")
            })?;
        let chunk_offset = row.get::<_, i64>(0) as u64;
        let data: Vec<u8> = row.get::<_, Vec<u8>>(1);
        let chunk_start = segno as u64 * self.seg + chunk_offset;
        if lsn.0 >= chunk_start + data.len() as u64 {
            bail!(
                "WAL hole at {lsn}: nearest chunk covers [{}, {}) only",
                Lsn(chunk_start),
                Lsn(chunk_start + data.len() as u64)
            );
        }
        Ok((Lsn(chunk_start), data))
    }
}


#[cfg(test)]
mod tests {
    use super::*;
    use postgres_ffi::XLOG_BLCKSZ;
    use postgres_ffi::v17::wal_generator::{LogicalMessageGenerator, WalGenerator};

    /// In-memory WAL: a single contiguous byte buffer covering
    /// `[start, start + buf.len())`. Serves the whole buffer as one
    /// "chunk", and reports a hole for any LSN outside it.
    struct MemWal {
        start: Lsn,
        buf: Vec<u8>,
    }

    impl WalSource for MemWal {
        fn seg(&self) -> u64 {
            WAL_SEGMENT_SIZE as u64
        }
        async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<(Lsn, Vec<u8>)> {
            let end = self.start.0 + self.buf.len() as u64;
            if lsn.0 < self.start.0 || lsn.0 >= end {
                bail!("WAL hole at {lsn} (mem buffer is [{}, {}))", self.start, Lsn(end));
            }
            Ok((self.start, self.buf.clone()))
        }
    }

    /// Generate a realistic WAL image starting at `start_lsn` with `count`
    /// uniform logical-message records of `value_size` payload bytes.
    /// Returns the buffer and the true record-start boundaries (the LSN of
    /// each record's first byte, in order).
    fn gen_wal(start_lsn: Lsn, count: usize, value_size: usize) -> (Vec<u8>, Vec<Lsn>) {
        gen_wal_sizes(start_lsn, &vec![value_size; count])
    }

    /// As [`gen_wal`], but one record per entry in `value_sizes`, so the
    /// caller can mix small and large (multi-page) records.
    fn gen_wal_sizes(start_lsn: Lsn, value_sizes: &[usize]) -> (Vec<u8>, Vec<Lsn>) {
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), start_lsn);
        let mut buf = Vec::new();
        let mut starts = Vec::new();
        for &sz in value_sizes {
            let value = vec![7u8; sz];
            let (lsn, bytes) = g.append_logical_message(c"t", &value);
            starts.push(lsn);
            buf.extend_from_slice(&bytes);
        }
        (buf, starts)
    }

    /// A segment boundary, so the buffer opens with a long page header and
    /// `find_anchor` exercises both header sizes as it scans back.
    const START: Lsn = Lsn(WAL_SEGMENT_SIZE as u64);

    #[tokio::test]
    async fn decodes_exact_boundaries_including_multipage_record() {
        // Small records, plus the run is long enough to cross several 8 KiB
        // page boundaries (so contrecord pages exist in the image).
        let (buf, starts) = gen_wal(START, 4000, 16);
        let src = MemWal { start: START, buf };

        // Decode the whole thing from the very start (itself a boundary).
        let target = *starts.last().unwrap();
        let (boundaries, stop) = decode_forward(&src, START, target, PgMajorVersion::PG17)
            .await
            .unwrap();
        assert!(stop.is_none(), "unexpected early stop: {stop:?}");

        // Every generator record-start must appear as a decoded boundary.
        for s in &starts {
            assert!(
                boundaries.contains(s),
                "decoded boundaries missing generator record-start {s}"
            );
        }
        // Boundaries are strictly increasing.
        assert!(boundaries.windows(2).all(|w| w[0] < w[1]));
    }

    #[tokio::test]
    async fn picks_largest_boundary_at_or_below_target() {
        let (buf, starts) = gen_wal(START, 4000, 64);
        let src = MemWal {
            start: START,
            buf: buf.clone(),
        };

        // Target exactly on a boundary -> that boundary, prev = the one before.
        let k = 2500;
        let target = starts[k];
        let anchor = find_anchor(&src, target).await.unwrap();
        assert!(anchor <= target, "anchor {anchor} must be <= target {target}");
        let (boundaries, _) = decode_forward(&src, anchor, target, PgMajorVersion::PG17)
            .await
            .unwrap();
        let idx = boundaries.iter().rposition(|b| *b <= target).unwrap();
        assert_eq!(boundaries[idx], starts[k], "consistent_lsn on a boundary");
        assert_eq!(
            boundaries[idx - 1],
            starts[k - 1],
            "prev_record_lsn is the preceding boundary"
        );

        // Target a few bytes PAST a boundary (mid-record) -> rounds down to
        // that boundary, never above it.
        let mid = Lsn(starts[k].0 + 8);
        assert!(mid < starts[k + 1], "test setup: 8 bytes stays within record k");
        let anchor = find_anchor(&src, mid).await.unwrap();
        let (boundaries, _) = decode_forward(&src, anchor, mid, PgMajorVersion::PG17)
            .await
            .unwrap();
        let idx = boundaries.iter().rposition(|b| *b <= mid).unwrap();
        assert_eq!(
            boundaries[idx], starts[k],
            "mid-record target must round down to the record's start boundary"
        );
        assert!(
            boundaries[idx] <= mid && (idx + 1 >= boundaries.len() || boundaries[idx + 1] > mid),
            "result must be the largest boundary <= target"
        );
    }

    #[tokio::test]
    async fn rounds_down_through_a_multipage_record() {
        // A record whose payload far exceeds 8 KiB spans several pages,
        // creating contrecord pages that find_anchor must scan back past.
        // Layout: 50 small, one ~40 KiB record (≈5 pages), then 50 small.
        let mut sizes = vec![32usize; 50];
        let big_idx = sizes.len();
        sizes.push(40 * 1024);
        sizes.extend(std::iter::repeat_n(32usize, 50));
        let (buf, starts) = gen_wal_sizes(START, &sizes);
        let big_start = starts[big_idx];
        let after_big = starts[big_idx + 1];
        assert!(
            after_big.0 - big_start.0 > 4 * XLOG_BLCKSZ as u64,
            "test setup: big record should span several pages"
        );
        let src = MemWal { start: START, buf };

        // Target deep inside the multi-page record. The only real boundary
        // at or below it is the record's own start.
        let target = Lsn(big_start.0 + 3 * XLOG_BLCKSZ as u64);
        assert!(target < after_big);
        let anchor = find_anchor(&src, target).await.unwrap();
        assert!(anchor <= target);
        let (boundaries, _) = decode_forward(&src, anchor, target, PgMajorVersion::PG17)
            .await
            .unwrap();
        let idx = boundaries.iter().rposition(|b| *b <= target).unwrap();
        assert_eq!(
            boundaries[idx], big_start,
            "a target inside a multi-page record must round down to that record's start"
        );
        assert_eq!(boundaries[idx - 1], starts[big_idx - 1]);
    }

    #[tokio::test]
    async fn anchor_is_a_real_boundary_far_from_buffer_start() {
        // A target deep in the buffer: find_anchor should land on a real
        // record-start near (and <=) it, not the buffer start.
        let (buf, starts) = gen_wal(START, 4000, 200);
        let src = MemWal { start: START, buf };
        let target = starts[3000];
        let anchor = find_anchor(&src, target).await.unwrap();
        assert!(anchor <= target);
        // The anchor is itself a generator record-start (a true boundary).
        assert!(
            starts.contains(&anchor),
            "anchor {anchor} is not a real record boundary"
        );
    }
}
