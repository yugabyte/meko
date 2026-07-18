//! Field-repro probe: replicate the pageserver's `read_chain_tsclient`
//! getpage against a real cluster, with wire tracing, to see whether/
//! where `read_page` stalls. NOT shipped — a diagnostic.
//!
//! Run (RUST_LOG shows the transport steps):
//!   RUST_LOG=yb_tsclient=trace,yb_tsclient::wire=trace \
//!   cargo run -p yb_tsclient --example readpage_probe -- \
//!     <tserver_addr> <db_oid> <table_oid> <branch_id> \
//!     <spc> <db> <rel> <fork> <blk> <target_lsn>

use std::sync::Arc;
use std::time::{Duration, Instant};

use bytes::Bytes;
use yb_tsclient::{BindValue, CondOp, RangeRead, TsClient, TsClientConfig};

#[tokio::main(flavor = "multi_thread")]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .with_target(true)
        .init();

    let a: Vec<String> = std::env::args().skip(1).collect();
    if a.len() < 10 {
        eprintln!(
            "usage: readpage_probe <tserver> <db_oid> <table_oid> <branch_id> \
             <spc> <db> <rel> <fork> <blk> <target_lsn> [chain_page_rows=32]"
        );
        std::process::exit(2);
    }
    let tserver = a[0].clone();
    let db_oid: u32 = a[1].parse()?;
    let table_oid: u32 = a[2].parse()?;
    let branch = a[3].clone();
    let (spc, db, rel): (i64, i64, i64) = (a[4].parse()?, a[5].parse()?, a[6].parse()?);
    let fork: i64 = a[7].parse()?;
    let blk: i64 = a[8].parse()?;
    let target: i64 = a[9].parse()?;
    let limit: u64 = a.get(10).and_then(|s| s.parse().ok()).unwrap_or(32);

    eprintln!("connecting to {tserver} (rpc_timeout stays default 10s)…");
    let client = TsClient::connect(TsClientConfig {
        tserver_addrs: vec![tserver.clone()],
        ..Default::default()
    })
    .await?;
    eprintln!("connected, session {}", client.session_id());

    let pages = client.open_table(db_oid, table_oid).await?;
    eprintln!(
        "opened table {} cols={:?}",
        pages.table_id,
        pages.columns.iter().map(|c| &c.name).collect::<Vec<_>>()
    );

    // `SCAN=<blk_hi>` switches to the scan_one_fork shape: blknum in
    // [blk, blk_hi), read-time pinned after page 1 (the basebackup
    // relation-scan path — the op with no completed metric series).
    let scan_hi: Option<i64> = std::env::var("SCAN").ok().and_then(|s| s.parse().ok());
    let mut read = RangeRead::new(
        &pages,
        vec![
            BindValue::Text(branch),
            int_bind(&pages, "spcnode", spc)?,
            int_bind(&pages, "dbnode", db)?,
            int_bind(&pages, "relnode", rel)?,
            int_bind(&pages, "forknum", fork)?,
        ],
    )?;
    read = if let Some(hi) = scan_hi {
        read.cond(&pages, "blknum", CondOp::Ge, BindValue::I64(blk))?
            .cond(&pages, "blknum", CondOp::Lt, BindValue::I64(hi))?
            .cond(&pages, "lsn", CondOp::Le, BindValue::I64(target))?
            .targets(
                &pages,
                &["blknum", "lsn", "is_image", "will_init", "payload"],
            )?
            .limit(limit)
    } else {
        read.cond(&pages, "blknum", CondOp::Eq, BindValue::I64(blk))?
            .cond(&pages, "lsn", CondOp::Le, BindValue::I64(target))?
            .targets(&pages, &["lsn", "is_image", "will_init", "payload"])?
            .limit(limit)
    };

    eprintln!(
        "issuing {} (blk_lo={blk} scan_hi={scan_hi:?} target_lsn={target} limit={limit})…",
        if scan_hi.is_some() {
            "SCAN"
        } else {
            "chain read"
        }
    );
    let mut pinned = false;
    let overall = Instant::now();
    let mut paging: Option<Bytes> = None;
    let mut page_no = 0;
    let mut rows_total = 0;
    loop {
        page_no += 1;
        let t0 = Instant::now();
        // Bound each page ourselves so a genuine hang is visible as a
        // probe-level timeout rather than an infinite wait.
        let res = tokio::time::timeout(
            Duration::from_secs(45),
            client.read_page(&pages, &read, paging.take()),
        )
        .await;
        let page = match res {
            Err(_) => {
                eprintln!(
                    "!!! PAGE {page_no} DID NOT RETURN in 45s (elapsed {:?}) — read_page hung; \
                     the client's own {:?} rpc_timeout did NOT fire",
                    overall.elapsed(),
                    Duration::from_secs(10),
                );
                anyhow::bail!("read_page hang reproduced");
            }
            Ok(Err(e)) => {
                eprintln!("page {page_no} errored after {:?}: {e}", t0.elapsed());
                return Err(e.into());
            }
            Ok(Ok(p)) => p,
        };
        rows_total += page.rows.len();
        eprintln!(
            "page {page_no}: {} rows in {:?} (total {rows_total}), used_read_ht={} more={}",
            page.rows.len(),
            t0.elapsed(),
            page.used_read_time_ht,
            page.paging_state.is_some()
        );
        // Mirror scan_one_fork: pin the read time from page 1 for the rest.
        if scan_hi.is_some() && !pinned && page.used_read_time_ht > 0 {
            read = read.read_time_ht(page.used_read_time_ht);
            pinned = true;
        }
        match page.paging_state {
            Some(p) => paging = Some(p),
            None => break,
        }
    }
    eprintln!(
        "DONE: {rows_total} rows over {page_no} page(s) in {:?}",
        overall.elapsed()
    );
    Ok(())
}

fn int_bind(t: &Arc<yb_tsclient::TableHandle>, col: &str, v: i64) -> anyhow::Result<BindValue> {
    use yb_tsclient::ValueType;
    Ok(match t.col(col)?.value_type {
        ValueType::I16 => BindValue::I16(v as i16),
        ValueType::I32 => BindValue::I32(v as i32),
        _ => BindValue::I64(v),
    })
}
