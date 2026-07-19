//! Dump `safekeeper.wals` rows (the safekeeper's WAL-in-YB chunks) in a
//! human-readable form.
//!
//! Shares the connection flags (`--host/--port/--user/--db-name/
//! --password-env/--sslmode/--sslrootcert`) with the other yb_storage
//! tools via [`ysql_client::YbConnArgs`], so it works against both a local
//! no-TLS cluster (default) and a TLS-required one (`--sslmode require`).
//!
//! Rows are ordered (segno, chunk_offset ASC). Each chunk's starting-byte
//! LSN is rendered `H/L` hex (= segno*wal_seg_size + chunk_offset). With
//! `--summary` it prints a per-segno health view (min/max LSN, total &
//! gap bytes) — the fastest way to spot a WAL hole during an incident.

use anyhow::{Context, Result, bail};
use clap::Parser;
use utils::id::BranchId;
use utils::lsn::Lsn;
use ysql_client::{YbConnArgs, YsqlClient};

#[derive(Parser)]
#[command(about = "Dump safekeeper.wals (safekeeper WAL-in-YB chunks)")]
struct Args {
    /// Branch id (32 hex chars).
    #[arg(long)]
    branch: BranchId,

    /// Single segment.
    #[arg(long)]
    segno: Option<i64>,
    #[arg(long)]
    segno_from: Option<i64>,
    #[arg(long)]
    segno_to: Option<i64>,
    /// LSN lower bound in `X/Y` hex (mapped to segno/offset).
    #[arg(long, value_name = "X/Y")]
    lsn_from: Option<Lsn>,
    #[arg(long, value_name = "X/Y")]
    lsn_to: Option<Lsn>,
    /// Exact chunk_offset within segno.
    #[arg(long)]
    chunk_offset: Option<i64>,
    #[arg(long)]
    chunk_offset_from: Option<i64>,
    #[arg(long)]
    chunk_offset_to: Option<i64>,
    /// WAL segment size in bytes (default 16 MiB).
    #[arg(long, default_value_t = 16 * 1024 * 1024)]
    wal_seg_size: i64,
    /// Max rows (0 = no limit). Ignored with --summary.
    #[arg(long, default_value_t = 50)]
    limit: i64,
    /// Skip the data sample column.
    #[arg(long)]
    no_data: bool,
    /// Per-segno (min/max/count/holes) summary instead of rows.
    #[arg(long)]
    summary: bool,

    #[command(flatten)]
    conn: YbConnArgs,
}

/// `segno*seg + chunk_offset` rendered as the PS-log `H/XXXXXXXX` LSN form.
fn lsn_expr(seg: i64, off_expr: &str) -> String {
    format!(
        "to_hex((({seg}::bigint * segno + {off_expr}) >> 32)::int8) || '/' || \
         lpad(to_hex((({seg}::bigint * segno + {off_expr}) & x'ffffffff'::bigint)::int8), 8, '0')"
    )
}

/// Split an LSN into the `(segno, offset)` of the byte it addresses, for a given
/// segment size: `segno * seg + offset == lsn` with `0 <= offset < seg`. The
/// inverse of the stored `segno*seg + chunk_offset` LSN encoding (see
/// [`lsn_expr`]); used to translate `--lsn-from` / `--lsn-to` into the stored
/// (segno, chunk_offset) coordinates.
fn lsn_to_seg_off(lsn: Lsn, seg: i64) -> (i64, i64) {
    (lsn.0 as i64 / seg, lsn.0 as i64 % seg)
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    let seg = args.wal_seg_size;
    if seg <= 0 {
        bail!("--wal-seg-size must be positive");
    }
    let client = YsqlClient::from_yb_config(&args.conn.into_config(), "yb-dump-wals", false)
        .await
        .context("connecting to yb_storage")?;

    // WHERE — interpolating only clap-parsed ids (hex) and integers.
    let mut w = format!("WHERE branch_id='{}'", args.branch);
    if let Some(s) = args.segno {
        w += &format!(" AND segno = {s}");
    }
    if let Some(s) = args.segno_from {
        w += &format!(" AND segno >= {s}");
    }
    if let Some(s) = args.segno_to {
        w += &format!(" AND segno <= {s}");
    }
    if let Some(o) = args.chunk_offset {
        w += &format!(" AND chunk_offset = {o}");
    }
    if let Some(o) = args.chunk_offset_from {
        w += &format!(" AND chunk_offset >= {o}");
    }
    if let Some(o) = args.chunk_offset_to {
        w += &format!(" AND chunk_offset <= {o}");
    }
    // LSN bounds map to (segno, offset); a chunk covers [offset, offset+size).
    if let Some(l) = args.lsn_from {
        let (s, o) = lsn_to_seg_off(l, seg);
        w += &format!(" AND (segno > {s} OR (segno = {s} AND chunk_offset + chunk_size > {o}))");
    }
    if let Some(l) = args.lsn_to {
        let (s, o) = lsn_to_seg_off(l, seg);
        w += &format!(" AND (segno < {s} OR (segno = {s} AND chunk_offset <= {o}))");
    }

    let (sql, headers) = if args.summary {
        let min_lsn = lsn_expr(seg, "min(chunk_offset)");
        let max_lsn = lsn_expr(seg, "max(chunk_offset + chunk_size)");
        (
            format!(
                "SELECT segno::text, count(*)::text, min(chunk_offset)::text, \
                        max(chunk_offset + chunk_size)::text, {min_lsn}, {max_lsn}, \
                        sum(chunk_size)::text, \
                        (max(chunk_offset + chunk_size) - min(chunk_offset) - sum(chunk_size))::text \
                 FROM safekeeper.wals {w} GROUP BY segno ORDER BY segno"
            ),
            vec![
                "segno",
                "chunks",
                "min_off",
                "max_end",
                "min_lsn",
                "max_lsn",
                "total_bytes",
                "gap_bytes",
            ],
        )
    } else {
        let start_lsn = lsn_expr(seg, "chunk_offset");
        let data_cols = if args.no_data {
            String::new()
        } else {
            ", octet_length(data)::text, encode(substring(data,1,16),'hex')".to_string()
        };
        let lim = if args.limit > 0 {
            format!("LIMIT {}", args.limit)
        } else {
            String::new()
        };
        let mut h = vec![
            "segno",
            "bucket",
            "chunk_offset",
            "chunk_size",
            "start_lsn",
            "modified_at",
        ];
        if !args.no_data {
            h.push("bytes");
            h.push("data_head");
        }
        (
            format!(
                "SELECT segno::text, bucket_id::text, chunk_offset::text, chunk_size::text, {start_lsn}, \
                        to_char(to_timestamp(last_modified_us/1000000.0),'YYYY-MM-DD HH24:MI:SS'){data_cols} \
                 FROM safekeeper.wals {w} ORDER BY segno ASC, chunk_offset ASC {lim}"
            ),
            h,
        )
    };

    let rows = client
        .query(&sql, &[])
        .await
        .context("query safekeeper.wals")?;
    print_table(&headers, &rows);
    Ok(())
}

/// Print rows as an aligned table. Every selected column is cast to text
/// in SQL, so each cell reads back as `Option<String>`.
fn print_table(headers: &[&str], rows: &[ysql_client::tokio_postgres::Row]) {
    let ncol = headers.len();
    let mut widths: Vec<usize> = headers.iter().map(|h| h.len()).collect();
    let cells: Vec<Vec<String>> = rows
        .iter()
        .map(|r| {
            (0..ncol)
                .map(|i| r.get::<usize, Option<String>>(i).unwrap_or_default())
                .collect()
        })
        .collect();
    for row in &cells {
        for (i, c) in row.iter().enumerate() {
            widths[i] = widths[i].max(c.len());
        }
    }
    let line = |row: &[String]| {
        row.iter()
            .enumerate()
            .map(|(i, c)| format!("{c:<w$}", w = widths[i]))
            .collect::<Vec<_>>()
            .join("  ")
    };
    println!(
        "{}",
        line(&headers.iter().map(|h| h.to_string()).collect::<Vec<_>>())
    );
    for row in &cells {
        println!("{}", line(row));
    }
    println!("({} rows)", rows.len());
}

#[cfg(test)]
mod tests {
    use super::*;

    const SEG: i64 = 16 * 1024 * 1024; // default WAL segment size

    #[test]
    fn lsn_split_round_trips() {
        // For any (segno, offset), encoding to an LSN and splitting it back
        // must recover the same coordinates.
        for (segno, off) in [
            (0i64, 0i64),
            (0, 1),
            (0, SEG - 1),
            (1, 0),
            (5, 123),
            (42, SEG - 1),
        ] {
            let lsn = Lsn((segno * SEG + off) as u64);
            assert_eq!(lsn_to_seg_off(lsn, SEG), (segno, off), "lsn {lsn}");
        }
    }

    #[test]
    fn lsn_split_boundaries() {
        // Exactly on a segment boundary rolls to offset 0 of the next segment.
        assert_eq!(lsn_to_seg_off(Lsn(SEG as u64), SEG), (1, 0));
        // One byte before the boundary is the last offset of segment 0.
        assert_eq!(lsn_to_seg_off(Lsn((SEG - 1) as u64), SEG), (0, SEG - 1));
        // The offset is always within a segment.
        let (_, off) = lsn_to_seg_off(Lsn(u32::MAX as u64), SEG);
        assert!((0..SEG).contains(&off));
    }

    #[test]
    fn lsn_split_honors_segment_size() {
        // A non-default segment size changes where the boundary falls.
        let seg = 1024 * 1024; // 1 MiB
        assert_eq!(lsn_to_seg_off(Lsn(seg as u64 + 7), seg), (1, 7));
        assert_ne!(lsn_to_seg_off(Lsn(seg as u64 + 7), SEG), (1, 7));
    }

    #[test]
    fn lsn_expr_renders_the_hex_lsn_encoding() {
        // The forward encoding: (segno*seg + offset) split into H/XXXXXXXX hex.
        let e = lsn_expr(SEG, "chunk_offset");
        assert!(e.contains("16777216::bigint * segno + chunk_offset"), "{e}");
        assert!(e.contains("to_hex"), "{e}");
        assert!(e.contains("|| '/' ||"), "{e}");
        assert!(e.contains("lpad"), "{e}");
        // The offset expression is substituted, not hard-coded.
        assert!(lsn_expr(SEG, "min(chunk_offset)").contains("min(chunk_offset)"));
    }
}
