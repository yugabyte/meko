//! Dump or decode `pageserver.pages` rows.
//!
//! Shares the connection flags with the other yb_storage tools via
//! [`ysql_client::YbConnArgs`] (works against local no-TLS and `--sslmode
//! require` TLS clusters alike).
//!
//! LIST (default): one line per row — lsn / fork / is_image,will_init /
//! payload sample. DECODE (`--decode`): for a SINGLE block, walk the chain
//! in the order walredo applies it (LSN-ascending) and decode each row —
//! image → line-pointer max offset + flag counts; delta → WAL resource
//! manager, op, and target offnum — flagging a GAP when a delta's offnum
//! exceeds running_max+1 (the chain-can't-fold signature; see issue #22).

use std::collections::BTreeMap;

use anyhow::{Context, Result};
use clap::Parser;
use utils::id::{TenantId, TimelineId};
use utils::lsn::Lsn;
use ysql_client::{YbConnArgs, YsqlClient};

#[derive(Parser)]
#[command(about = "Dump or decode pageserver.pages rows for one (rel, fork) tuple")]
struct Args {
    #[arg(long)]
    tenant: TenantId,
    #[arg(long)]
    timeline: TimelineId,
    #[arg(long)]
    spc: i64,
    #[arg(long)]
    db: i64,
    #[arg(long)]
    rel: i64,
    #[arg(long)]
    fork: i16,
    /// Single block. Required for --decode (a chain is per-block).
    #[arg(long)]
    blknum: Option<i64>,
    #[arg(long)]
    blknum_from: Option<i64>,
    #[arg(long)]
    blknum_to: Option<i64>,
    #[arg(long, value_name = "X/Y")]
    lsn_from: Option<Lsn>,
    #[arg(long, value_name = "X/Y")]
    lsn_to: Option<Lsn>,
    /// Max rows (0 = no limit). LIST only.
    #[arg(long, default_value_t = 50)]
    limit: i64,
    /// Skip the payload sample column (LIST only).
    #[arg(long)]
    no_payload: bool,
    /// Decode a single block's chain (image max_off, delta op/offnum, gaps).
    #[arg(long)]
    decode: bool,

    #[command(flatten)]
    conn: YbConnArgs,
}

fn key_where(a: &Args) -> String {
    let mut w = format!(
        "tenant_id='{}' AND timeline_id='{}' AND spcnode={} AND dbnode={} \
         AND relnode={} AND forknum={}",
        a.tenant, a.timeline, a.spc, a.db, a.rel, a.fork
    );
    if let Some(l) = a.lsn_from {
        w += &format!(" AND lsn >= {}", l.0 as i64);
    }
    if let Some(l) = a.lsn_to {
        w += &format!(" AND lsn <= {}", l.0 as i64);
    }
    w
}

fn fmt_lsn(v: u64) -> String {
    format!("{:X}/{:08X}", v >> 32, v & 0xFFFFFFFF)
}

fn unhex(s: &str) -> Vec<u8> {
    (0..s.len() / 2)
        .map(|i| u8::from_str_radix(&s[i * 2..i * 2 + 2], 16).unwrap_or(0))
        .collect()
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    let client = YsqlClient::from_yb_config(&args.conn.into_config(), "yb-dump-pages", false)
        .await
        .context("connecting to yb_storage")?;
    if args.decode {
        decode(&args, &client).await
    } else {
        list(&args, &client).await
    }
}

async fn list(a: &Args, client: &YsqlClient) -> Result<()> {
    let mut w = key_where(a);
    if let Some(b) = a.blknum {
        w += &format!(" AND blknum = {b}");
    }
    if let Some(b) = a.blknum_from {
        w += &format!(" AND blknum >= {b}");
    }
    if let Some(b) = a.blknum_to {
        w += &format!(" AND blknum <= {b}");
    }
    let lim = if a.limit > 0 {
        format!("LIMIT {}", a.limit)
    } else {
        String::new()
    };
    let lsn = "to_hex((lsn>>32)::int8) || '/' || lpad(to_hex((lsn & x'ffffffff'::bigint)::int8),8,'0')";
    let payload = if a.no_payload {
        String::new()
    } else {
        ", octet_length(payload)::text, encode(substring(payload,1,16),'hex')".to_string()
    };
    let fork = "CASE forknum WHEN 0 THEN 'main' WHEN 1 THEN 'fsm' WHEN 2 THEN 'vm' \
                WHEN 3 THEN 'init' ELSE forknum::text END";
    let sql = format!(
        "SELECT blknum::text, {lsn}, {fork}, is_image::text, will_init::text{payload} \
         FROM pageserver.pages WHERE {w} \
         ORDER BY blknum ASC, lsn DESC, is_image ASC {lim}"
    );
    let rows = client.query(&sql, &[]).await.context("query pageserver.pages")?;
    let mut headers = vec!["blknum", "lsn", "fork", "is_image", "will_init"];
    if !a.no_payload {
        headers.push("bytes");
        headers.push("payload_head");
    }
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
    let fmt = |row: &[String]| {
        row.iter()
            .enumerate()
            .map(|(i, c)| format!("{c:<w$}", w = widths[i]))
            .collect::<Vec<_>>()
            .join("  ")
    };
    println!("{}", fmt(&headers.iter().map(|h| h.to_string()).collect::<Vec<_>>()));
    for row in &cells {
        println!("{}", fmt(row));
    }
    println!("({} rows)", rows.len());
    Ok(())
}

/// (max_off, {flag: count}) from a raw 8 KB heap page image.
fn lp_state(page: &[u8]) -> (usize, BTreeMap<&'static str, usize>) {
    if page.len() < 24 {
        return (0, BTreeMap::new());
    }
    let pd_lower = u16::from_le_bytes([page[12], page[13]]) as usize;
    let nlp = pd_lower.saturating_sub(24) / 4;
    let mut counts = BTreeMap::new();
    for i in 0..nlp {
        let o = 24 + i * 4;
        if o + 4 > page.len() {
            break;
        }
        let dw = u32::from_le_bytes([page[o], page[o + 1], page[o + 2], page[o + 3]]);
        let flag = ["UNUSED", "NORMAL", "REDIRECT", "DEAD"][((dw >> 15) & 3) as usize];
        *counts.entry(flag).or_insert(0) += 1;
    }
    (nlp, counts)
}

/// Raw XLogRecord bytes -> (rmid, info, maindata).
fn decode_record(rec: &[u8]) -> Option<(u8, u8, Vec<u8>)> {
    if rec.len() < 24 {
        return None;
    }
    let tot = u32::from_le_bytes([rec[0], rec[1], rec[2], rec[3]]) as usize;
    if tot > rec.len() {
        return None;
    }
    let (info, rmid) = (rec[16], rec[17]);
    let mut pos = 24usize;
    let mut maindata = Vec::new();
    while pos < tot {
        let blkid = rec[pos];
        if blkid == 255 {
            let ln = rec[pos + 1] as usize;
            maindata = rec[tot - ln..tot].to_vec();
            break;
        } else if blkid == 254 {
            let ln =
                u32::from_le_bytes([rec[pos + 1], rec[pos + 2], rec[pos + 3], rec[pos + 4]]) as usize;
            maindata = rec[tot - ln..tot].to_vec();
            break;
        } else if blkid == 253 {
            pos += 1 + 2;
        } else if blkid == 252 {
            pos += 1 + 4;
        } else if blkid <= 32 {
            let fork_flags = rec[pos + 1];
            pos += 4; // blkid + fork_flags + data_len(u16)
            if fork_flags & 0x10 != 0 {
                let bimg_info = rec[pos + 4];
                pos += 5;
                if bimg_info & 0x04 != 0 {
                    pos += 2;
                }
            }
            if fork_flags & 0x80 == 0 {
                pos += 12; // rel (spc,db,rel)
            }
            pos += 4; // blkno
        } else {
            return None;
        }
    }
    Some((rmid, info, maindata))
}

/// (op-name, target offnum) for a Neon-RM record.
fn neon_offnum(info: u8, md: &[u8]) -> (String, Option<u16>) {
    let u16le = |b: &[u8]| u16::from_le_bytes([b[0], b[1]]);
    let (op, off): (&str, Option<u16>) = match info & 0x70 {
        0x00 => ("INSERT", md.get(0..2).map(u16le)),
        0x10 => ("DELETE", md.get(4..6).map(u16le)),
        0x20 => ("UPDATE", md.get(16..18).map(u16le)),
        0x30 => ("HOT_UPDATE", md.get(16..18).map(u16le)),
        0x40 => ("LOCK", None),
        0x50 => ("MULTI_INSERT", None),
        x => return (format!("op{x:#x}"), None),
    };
    let init = if info & 0x80 != 0 { "+INIT" } else { "" };
    (format!("{op}{init}"), off)
}

fn rmgr(rmid: u8) -> String {
    match rmid {
        0 => "XLOG".into(),
        1 => "Xact".into(),
        2 => "Storage".into(),
        3 => "CLOG".into(),
        8 => "Standby".into(),
        9 => "Heap2".into(),
        10 => "Heap".into(),
        11 => "Btree".into(),
        134 => "neon".into(),
        x => format!("rm{x}"),
    }
}

async fn decode(a: &Args, client: &YsqlClient) -> Result<()> {
    let blk = a
        .blknum
        .context("--decode needs a single --blknum (a chain is per-block)")?;
    let w = format!("{} AND blknum = {blk}", key_where(a));
    // LSN-ascending, image-first on ties (an image supersedes a same-LSN delta).
    let sql = format!(
        "SELECT to_hex(lsn), is_image::int::text, will_init::int::text, encode(payload,'hex') \
         FROM pageserver.pages WHERE {w} ORDER BY lsn, is_image DESC"
    );
    let rows = client.query(&sql, &[]).await.context("query pageserver.pages")?;
    let lo = a.lsn_from.map(|l| format!(" from {}", fmt_lsn(l.0))).unwrap_or_default();
    let hi = a.lsn_to.map(|l| format!(" to {}", fmt_lsn(l.0))).unwrap_or_default();
    println!("{} rows for rel={} blk={blk}{lo}{hi}\n", rows.len(), a.rel);

    let mut running_max: usize = 0;
    let mut gaps = 0usize;
    for r in &rows {
        let lsn = u64::from_str_radix(&r.get::<usize, String>(0), 16).unwrap_or(0);
        let is_image = r.get::<usize, String>(1) == "1";
        let will_init = r.get::<usize, String>(2) == "1";
        let payload = unhex(&r.get::<usize, String>(3));
        if is_image {
            let (max_off, counts) = lp_state(&payload);
            running_max = running_max.max(max_off);
            println!("IMAGE  {}  will_init={will_init}  max_off={max_off}  {counts:?}", fmt_lsn(lsn));
            continue;
        }
        // Delta: 13-byte BE-bincode header (variant u32 + will_init u8 +
        // len u64) then the raw XLogRecord.
        if payload.len() < 13 {
            println!("DELTA  {}  !! short payload ({}B)", fmt_lsn(lsn), payload.len());
            continue;
        }
        let declared = u64::from_be_bytes(payload[5..13].try_into().unwrap()) as usize;
        let rec = &payload[13..];
        if rec.len() != declared {
            println!("DELTA  {}  !! framing: len={} declared={declared}", fmt_lsn(lsn), rec.len());
            continue;
        }
        let (rmname, op, off) = match decode_record(rec) {
            Some((rmid, info, md)) if rmid == 134 => {
                let (op, off) = neon_offnum(info, &md);
                (rmgr(rmid), op, off)
            }
            Some((rmid, info, _)) => (rmgr(rmid), format!("info={info:#x}"), None),
            None => ("?".into(), "undecodable".into(), None),
        };
        let mut flag = String::new();
        if let Some(o) = off {
            let o = o as usize;
            if op.starts_with("INSERT") || op.starts_with("UPDATE") || op.starts_with("HOT_UPDATE") {
                if o > running_max + 1 {
                    flag = format!(
                        "   <-- GAP: offnum {o} > running_max+1 ({}); chain/base inconsistent",
                        running_max + 1
                    );
                    gaps += 1;
                }
                running_max = running_max.max(o);
            }
        }
        let offstr = off.map(|o| format!(" off={o}")).unwrap_or_default();
        println!("DELTA  {}  {rmname}/{op}{offstr}{flag}", fmt_lsn(lsn));
    }

    println!();
    if gaps > 0 {
        println!("** {gaps} GAP(s) — this chain does not fold cleanly (walredo would PANIC). See #22.");
    } else {
        println!("no offnum gaps — chain line-pointers are contiguous");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unhex_and_fmt_lsn() {
        assert_eq!(unhex("00ff10"), vec![0x00, 0xff, 0x10]);
        assert_eq!(unhex(""), Vec::<u8>::new());
        assert_eq!(fmt_lsn(0x22_9B61F090), "22/9B61F090");
        assert_eq!(fmt_lsn(0xD_E4B94BA8), "D/E4B94BA8");
    }

    #[test]
    fn lp_state_counts_line_pointers() {
        // 8 KB page; pd_lower (u16 LE @ off 12) = 24 + 4*N for N line
        // pointers. Three NORMAL (flags=1 -> bit15 set) line pointers.
        let mut page = vec![0u8; 8192];
        let nlp = 3usize;
        let pd_lower = (24 + 4 * nlp) as u16;
        page[12..14].copy_from_slice(&pd_lower.to_le_bytes());
        for i in 0..nlp {
            // lp_flags == 1 (LP_NORMAL) lives in bits 15..17 -> dword 0x8000.
            page[24 + i * 4..24 + i * 4 + 4].copy_from_slice(&0x8000u32.to_le_bytes());
        }
        let (max_off, counts) = lp_state(&page);
        assert_eq!(max_off, 3);
        assert_eq!(counts.get("NORMAL"), Some(&3));
        assert!(!counts.contains_key("UNUSED"));
    }

    /// Build a minimal Neon-RM XLogRecord: header + one same-rel block
    /// reference (no image) + a DATA_SHORT main-data block carrying the
    /// heap op's main data at the tail.
    fn neon_record(info: u8, main: &[u8]) -> Vec<u8> {
        let main_len = main.len();
        let tot = 24 + 4 /*blkref*/ + 4 /*blkno*/ + 2 /*DATA_SHORT marker*/ + main_len;
        let mut rec = vec![0u8; tot];
        rec[0..4].copy_from_slice(&(tot as u32).to_le_bytes());
        rec[16] = info;
        rec[17] = 134; // RM_NEON_ID
        // pos 24: block reference, fork_flags=0x80 (BKPBLOCK_SAME_REL, no image)
        rec[24] = 0; // blkid (<=32)
        rec[25] = 0x80; // fork_flags
        // data_len u16 @ 26..28 = 0; blkno @ 28..32 = 0.
        // pos 32: DATA_SHORT marker.
        rec[32] = 255;
        rec[33] = main_len as u8;
        // main data at the tail.
        rec[tot - main_len..tot].copy_from_slice(main);
        rec
    }

    #[test]
    fn decode_neon_insert_offnum() {
        // INSERT main data: offnum(u16 LE) + flags(u8).
        let main = [7u8, 0, 0]; // offnum = 7
        let rec = neon_record(0x00, &main);
        let (rmid, info, md) = decode_record(&rec).expect("decode");
        assert_eq!(rmid, 134);
        let (op, off) = neon_offnum(info, &md);
        assert_eq!(op, "INSERT");
        assert_eq!(off, Some(7));
    }

    #[test]
    fn decode_neon_insert_init_and_update() {
        // INSERT+INIT (info bit 0x80).
        let (op, off) = neon_offnum(0x80, &[1u8, 0, 0]);
        assert_eq!(op, "INSERT+INIT");
        assert_eq!(off, Some(1));
        // HOT_UPDATE new_offnum at byte 16 of main data.
        let mut md = vec![0u8; 18];
        md[16..18].copy_from_slice(&58u16.to_le_bytes());
        let (op, off) = neon_offnum(0x30, &md);
        assert_eq!(op, "HOT_UPDATE");
        assert_eq!(off, Some(58));
    }

    #[test]
    fn rmgr_names() {
        assert_eq!(rmgr(134), "neon");
        assert_eq!(rmgr(10), "Heap");
        assert_eq!(rmgr(200), "rm200");
    }
}
