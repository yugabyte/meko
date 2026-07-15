//! Throwaway field-scale probe (not for CI): big pages-shaped partitions
//! flushed to SSTs, chain reads incl. anchorless blocks, under writes.
//! Run: YB_LIVE_TSERVERS=... cargo test -p yb_pgclient --test scale_probe -- --ignored --nocapture
//!
//! TODO: this shells out to an external `yb-admin` binary at a hardcoded
//! dev-server path (`YB_ADMIN` override) to flush tablets to SSTs. Drop
//! that once ybdb is integrated into the amp repo and yb-admin (or an
//! equivalent flush) is available in-tree.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use bytes::Bytes;
use yb_pgclient::{BindValue, CondOp, PgClient, PgClientConfig, RangeRead, UpsertRow};
use ysql_client::{HostPorts, YbConnConfig, YsqlClient};

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| default.to_string())
}

async fn ysql() -> YsqlClient {
    let conf = YbConnConfig {
        host_ports: env_or("YB_LIVE_YSQL", "127.0.0.1:5433")
            .parse::<HostPorts>()
            .unwrap(),
        user: "yugabyte".to_string(),
        dbname: env_or("YB_LIVE_DB", "yb_storage"),
        password_env: None,
        sslmode: None,
        sslrootcert: None,
        disable_transactional_writes: false,
        hot_path: ysql_client::HotPath::Ysql,
        tserver_rpc_addrs: None,
    };
    YsqlClient::from_yb_config(&conf, "yb_pgclient_scale_probe", false)
        .await
        .expect("ysql connect")
}

/// The exact shape of the #217 field incident: a few HOT blocks whose
/// chains are thousands of TINY deltas (pl=24) with NO will_init anchor
/// anywhere, still being written while concurrent chain reads page
/// through the whole history (anchor never found => the paging loop runs
/// to range exhaustion, ~140 continuations per read). Reads run 16-way
/// concurrent on ONE client (one shared session), like the pageserver
/// under sysbench/TPC-C. Pass 1 unflushed, pass 2 after flush_table.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "manual field-scale probe"]
async fn field_exact_hot_anchorless_chains() {
    const TABLE: &str = "pgclient_field_pages";
    const BLOCKS: i64 = 6;
    const CHAIN: i64 = 4500;
    const REL: i64 = 16384;
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id    TEXT     NOT NULL,
                 spcnode      BIGINT   NOT NULL,
                 dbnode       BIGINT   NOT NULL,
                 relnode      BIGINT   NOT NULL,
                 forknum      SMALLINT NOT NULL,
                 blknum       BIGINT   NOT NULL,
                 is_image     BOOLEAN  NOT NULL,
                 lsn          BIGINT   NOT NULL,
                 will_init    BOOLEAN  NOT NULL,
                 payload      BYTEA    NOT NULL,
                 PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                              blknum, lsn DESC, is_image)
             ) SPLIT INTO 9 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    let t0 = std::time::Instant::now();
    for blk in 0..BLOCKS {
        for lo in (0..CHAIN).step_by(1500) {
            let hi = (lo + 1500).min(CHAIN);
            ysql.execute(
                &format!(
                    "INSERT INTO {TABLE}
                     SELECT 'br-field', 1663, 13000, {REL}, 0, {blk},
                            false, {blk} * 100000 + i, false,
                            decode(repeat('5a', 24), 'hex')
                     FROM generate_series({lo}, {hi} - 1) i"
                ),
                &[],
            )
            .await
            .unwrap();
        }
    }
    println!("inserted {} hot chains ({:?})", BLOCKS, t0.elapsed());
    let rows = ysql
        .query(
            &format!(
                "SELECT (SELECT oid FROM pg_database WHERE datname = current_database())::bigint,
                        c.oid::bigint, c.relfilenode::bigint
                 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                 WHERE n.nspname = 'public' AND c.relname = '{TABLE}'"
            ),
            &[],
        )
        .await
        .unwrap();
    let db_oid = rows[0].get::<_, i64>(0) as u32;
    let rel_oid = rows[0].get::<_, i64>(1) as u32;
    let relfilenode = rows[0].get::<_, i64>(2) as u32;
    let table_oid = if relfilenode != 0 {
        relfilenode
    } else {
        rel_oid
    };

    let tservers: Vec<String> = env_or("YB_LIVE_TSERVERS", "127.0.0.1:9100")
        .split(',')
        .map(str::to_string)
        .collect();
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: tservers,
        write_sessions: 2,
        ..Default::default()
    })
    .await
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Writers keep extending the SAME hot chains (fresh higher LSNs)
    // while the readers page through them.
    let stop = Arc::new(AtomicBool::new(false));
    let writer = {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        tokio::spawn(async move {
            let payload = Bytes::from(vec![0x5au8; 24]);
            let mut lsn = 10_000_000i64;
            while !stop.load(Ordering::Relaxed) {
                let rows: Vec<UpsertRow> = (0..16)
                    .map(|i| {
                        UpsertRow::new(
                            &table,
                            vec![
                                BindValue::Text("br-field".to_string()),
                                BindValue::I64(1663),
                                BindValue::I64(13000),
                                BindValue::I64(REL),
                                BindValue::I16(0),
                                BindValue::I64((lsn + i) % BLOCKS),
                                BindValue::I64(lsn + i),
                                BindValue::Bool(false),
                            ],
                            &[
                                ("will_init", BindValue::Bool(false)),
                                ("payload", BindValue::Bytes(payload.clone())),
                            ],
                        )
                        .unwrap()
                    })
                    .collect();
                client.upsert_batch(&table, &rows).await.expect("writer");
                lsn += 16;
            }
        })
    };

    // One full-chain paged read of a hot block, the exact
    // read_chain_pgclient loop. target above everything, incl. the
    // writer's fresh deltas (the field target is current WAL end).
    async fn chain_read(
        client: &PgClient,
        table: &Arc<yb_pgclient::TableHandle>,
        blk: i64,
        pass: &str,
    ) -> Result<(usize, u64), String> {
        let read = RangeRead::new(
            table,
            vec![
                BindValue::Text("br-field".to_string()),
                BindValue::I64(1663),
                BindValue::I64(13000),
                BindValue::I64(REL),
                BindValue::I16(0),
            ],
        )
        .unwrap()
        .cond(table, "blknum", CondOp::Eq, BindValue::I64(blk))
        .unwrap()
        .cond(table, "lsn", CondOp::Le, BindValue::I64(i64::MAX / 2))
        .unwrap()
        .targets(table, &["lsn", "is_image", "will_init", "payload"])
        .unwrap()
        .limit(32);
        let mut got = 0usize;
        let mut pages = 0u64;
        let mut paging = None;
        loop {
            pages += 1;
            match tokio::time::timeout(
                std::time::Duration::from_secs(60),
                client.read_page(table, &read, paging.take()),
            )
            .await
            {
                Err(_) => return Err(format!("{pass} blk {blk}: HANG >60s after {pages} pages")),
                Ok(Err(e)) => {
                    return Err(format!("{pass} blk {blk}: ERROR after {pages} pages: {e}"));
                }
                Ok(Ok(page)) => {
                    let n = page.rows.len();
                    got += n;
                    for row in page.rows {
                        if row[2].as_bool().unwrap() {
                            return Ok((got, pages)); // anchor (never present here)
                        }
                    }
                    if pages > 2000 {
                        return Err(format!(
                            "{pass} blk {blk}: RUNAWAY: {pages} pages, {got} rows"
                        ));
                    }
                    match page.paging_state {
                        Some(next) => paging = Some(next),
                        None => return Ok((got, pages)),
                    }
                }
            }
        }
    }

    for pass in ["unflushed", "flushed"] {
        if pass == "flushed" {
            let yb_admin = env_or(
                "YB_ADMIN",
                "/net/dev-server-hsunder/share/Downloads/yugabyte/yugabyte-2025.2.3.0/bin/yb-admin",
            );
            let masters = env_or("YB_LIVE_MASTERS", "127.0.0.1:7100");
            let flush = std::process::Command::new(&yb_admin)
                .args([
                    "--master_addresses",
                    &masters,
                    "flush_table",
                    &format!("ysql.{}", env_or("YB_LIVE_DB", "yb_storage")),
                    TABLE,
                    "120",
                ])
                .output()
                .expect("run yb-admin flush_table");
            println!(
                "flush_table: {} {}",
                String::from_utf8_lossy(&flush.stdout).trim(),
                String::from_utf8_lossy(&flush.stderr).trim()
            );
            assert!(flush.status.success(), "yb-admin flush_table failed");
            tokio::time::sleep(std::time::Duration::from_secs(2)).await;
        }
        // 16-way concurrent full-chain reads on the one shared session.
        let t = std::time::Instant::now();
        let mut tasks = Vec::new();
        for i in 0..16 {
            let (client, table) = (client.clone(), table.clone());
            let pass = pass.to_string();
            tasks.push(tokio::spawn(async move {
                chain_read(&client, &table, i % BLOCKS, &pass).await
            }));
        }
        let mut failures = Vec::new();
        for task in tasks {
            match task.await.unwrap() {
                Ok((got, pages)) => {
                    assert!(got >= CHAIN as usize, "short chain: {got} rows");
                    let _ = pages;
                }
                Err(e) => failures.push(e),
            }
        }
        println!(
            "{pass}: 16 concurrent full-chain reads in {:?}, {} failures",
            t.elapsed(),
            failures.len()
        );
        for f in &failures {
            println!("  {f}");
        }
        assert!(failures.is_empty(), "{failures:#?}");
    }
    stop.store(true, Ordering::Relaxed);
    writer.await.unwrap();
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "manual field-scale probe"]
async fn field_scale_chain_reads() {
    const TABLE: &str = "pgclient_scale_pages";
    const RELS: i64 = 2;
    const BLOCKS: i64 = 60;
    // 200 x 8KB = 1.6MB per block chain: every chain read needs ~7
    // paging continuations (limit 32), the field shape (materialiser
    // lag => hundreds of deltas per hot block).
    const ROWS_PER_BLOCK: i64 = 200;
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id    TEXT     NOT NULL,
                 spcnode      BIGINT   NOT NULL,
                 dbnode       BIGINT   NOT NULL,
                 relnode      BIGINT   NOT NULL,
                 forknum      SMALLINT NOT NULL,
                 blknum       BIGINT   NOT NULL,
                 is_image     BOOLEAN  NOT NULL,
                 lsn          BIGINT   NOT NULL,
                 will_init    BOOLEAN  NOT NULL,
                 payload      BYTEA    NOT NULL,
                 PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                              blknum, lsn DESC, is_image)
             ) SPLIT INTO 9 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    let t0 = std::time::Instant::now();
    for rel in 0..RELS {
        for lo in (0..BLOCKS).step_by(5) {
            let hi = (lo + 5).min(BLOCKS);
            // blk % 7 == 3 -> anchorless chain (no will_init row at all):
            // the read must scan the block's whole history to range end.
            ysql.execute(
                &format!(
                    "INSERT INTO {TABLE}
                     SELECT 'br-scale', 1663, 13000, 16384 + {rel}, 0, blk,
                            (i = 0 AND blk % 7 <> 3), blk * 1000 + i,
                            (i = 0 AND blk % 7 <> 3),
                            decode(repeat('a5', 8192), 'hex')
                     FROM generate_series({lo}, {hi} - 1) blk,
                          LATERAL generate_series(0, {ROWS_PER_BLOCK} - 1) i"
                ),
                &[],
            )
            .await
            .unwrap();
        }
        println!("inserted rel {rel} ({:?})", t0.elapsed());
    }
    // Flush to SSTs so reads hit disk like a long-lived TPC-C store.
    // (yb-admin flush_table; YB_ADMIN / YB_LIVE_MASTERS override the
    // binary and master addresses.)
    let yb_admin = env_or(
        "YB_ADMIN",
        "/net/dev-server-hsunder/share/Downloads/yugabyte/yugabyte-2025.2.3.0/bin/yb-admin",
    );
    let masters = env_or("YB_LIVE_MASTERS", "127.0.0.1:7100");
    let flush = std::process::Command::new(&yb_admin)
        .args([
            "--master_addresses",
            &masters,
            "flush_table",
            &format!("ysql.{}", env_or("YB_LIVE_DB", "yb_storage")),
            TABLE,
            "120",
        ])
        .output()
        .expect("run yb-admin flush_table");
    println!(
        "flush_table: {} {}",
        String::from_utf8_lossy(&flush.stdout).trim(),
        String::from_utf8_lossy(&flush.stderr).trim()
    );
    assert!(flush.status.success(), "yb-admin flush_table failed");
    tokio::time::sleep(std::time::Duration::from_secs(3)).await;
    let rows = ysql
        .query(
            &format!(
                "SELECT (SELECT oid FROM pg_database WHERE datname = current_database())::bigint,
                        c.oid::bigint, c.relfilenode::bigint
                 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                 WHERE n.nspname = 'public' AND c.relname = '{TABLE}'"
            ),
            &[],
        )
        .await
        .unwrap();
    let db_oid = rows[0].get::<_, i64>(0) as u32;
    let rel_oid = rows[0].get::<_, i64>(1) as u32;
    let relfilenode = rows[0].get::<_, i64>(2) as u32;
    let table_oid = if relfilenode != 0 {
        relfilenode
    } else {
        rel_oid
    };

    let tservers: Vec<String> = env_or("YB_LIVE_TSERVERS", "127.0.0.1:9100")
        .split(',')
        .map(str::to_string)
        .collect();
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: tservers,
        write_sessions: 2,
        ..Default::default()
    })
    .await
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Concurrent writers appending fresh high-LSN deltas to the SAME
    // blocks being read.
    let stop = Arc::new(AtomicBool::new(false));
    let writer = {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        tokio::spawn(async move {
            let payload = Bytes::from(vec![0x77u8; 8192]);
            let mut lsn = 50_000_000i64;
            while !stop.load(Ordering::Relaxed) {
                let rows: Vec<UpsertRow> = (0..8)
                    .map(|i| {
                        UpsertRow::new(
                            &table,
                            vec![
                                BindValue::Text("br-scale".to_string()),
                                BindValue::I64(1663),
                                BindValue::I64(13000),
                                BindValue::I64(16384 + (lsn + i) % RELS),
                                BindValue::I16(0),
                                BindValue::I64((lsn + i) % BLOCKS),
                                BindValue::I64(lsn + i),
                                BindValue::Bool(false),
                            ],
                            &[
                                ("will_init", BindValue::Bool(false)),
                                ("payload", BindValue::Bytes(payload.clone())),
                            ],
                        )
                        .unwrap()
                    })
                    .collect();
                client.upsert_batch(&table, &rows).await.expect("writer");
                lsn += 8;
            }
        })
    };

    // Chain-read every block of every rel, verbatim read_chain_pgclient.
    let target = 40_000_000i64; // excludes the writer's fresh deltas
    let mut failures = Vec::new();
    let mut worst = std::time::Duration::ZERO;
    let mut worst_blk = (0i64, 0i64);
    let mut calls = 0u64;
    let read_t0 = std::time::Instant::now();
    for rel in 0..RELS {
        for blk in 0..BLOCKS {
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text("br-scale".to_string()),
                    BindValue::I64(1663),
                    BindValue::I64(13000),
                    BindValue::I64(16384 + rel),
                    BindValue::I16(0),
                ],
            )
            .unwrap()
            .cond(&table, "blknum", CondOp::Eq, BindValue::I64(blk))
            .unwrap()
            .cond(&table, "lsn", CondOp::Le, BindValue::I64(target))
            .unwrap()
            .targets(&table, &["lsn", "is_image", "will_init", "payload"])
            .unwrap()
            .limit(32);
            let started = std::time::Instant::now();
            let mut got = 0usize;
            let mut paging = None;
            let outcome = loop {
                calls += 1;
                match tokio::time::timeout(
                    std::time::Duration::from_secs(60),
                    client.read_page(&table, &read, paging.take()),
                )
                .await
                {
                    Err(_) => break Err("HANG >60s".to_string()),
                    Ok(Err(e)) => break Err(format!("ERROR {e}")),
                    Ok(Ok(page)) => {
                        let mut done = false;
                        for row in page.rows {
                            got += 1;
                            if row[2].as_bool().unwrap() {
                                done = true;
                                break;
                            }
                        }
                        if done {
                            break Ok(());
                        }
                        match page.paging_state {
                            Some(next) => paging = Some(next),
                            None => break Ok(()),
                        }
                    }
                }
            };
            let el = started.elapsed();
            if el > worst {
                worst = el;
                worst_blk = (rel, blk);
            }
            match outcome {
                Err(e) => failures.push(format!("rel {rel} blk {blk}: {e}")),
                Ok(()) => {
                    if got != ROWS_PER_BLOCK as usize {
                        failures.push(format!("rel {rel} blk {blk}: {got} rows"));
                    }
                }
            }
        }
        println!(
            "rel {rel} read done ({:?} total, worst {worst:?} at {worst_blk:?})",
            read_t0.elapsed()
        );
    }
    // Range-scan pass: the pageserver's other paged shape
    // (`blknum >= lo AND blknum < hi AND lsn <= target`, limit 128),
    // reading through every page.
    for rel in 0..RELS {
        for lo in (0..BLOCKS).step_by(16) {
            let hi = (lo + 16).min(BLOCKS);
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text("br-scale".to_string()),
                    BindValue::I64(1663),
                    BindValue::I64(13000),
                    BindValue::I64(16384 + rel),
                    BindValue::I16(0),
                ],
            )
            .unwrap()
            .cond(&table, "blknum", CondOp::Ge, BindValue::I64(lo))
            .unwrap()
            .cond(&table, "blknum", CondOp::Lt, BindValue::I64(hi))
            .unwrap()
            .cond(&table, "lsn", CondOp::Le, BindValue::I64(target))
            .unwrap()
            .targets(
                &table,
                &["blknum", "lsn", "is_image", "will_init", "payload"],
            )
            .unwrap()
            .limit(128);
            let started = std::time::Instant::now();
            let mut got = 0usize;
            let mut paging = None;
            let outcome = loop {
                calls += 1;
                match tokio::time::timeout(
                    std::time::Duration::from_secs(60),
                    client.read_page(&table, &read, paging.take()),
                )
                .await
                {
                    Err(_) => break Err("HANG >60s".to_string()),
                    Ok(Err(e)) => break Err(format!("ERROR {e}")),
                    Ok(Ok(page)) => {
                        got += page.rows.len();
                        match page.paging_state {
                            Some(next) => paging = Some(next),
                            None => break Ok(()),
                        }
                    }
                }
            };
            let el = started.elapsed();
            if el > worst {
                worst = el;
                worst_blk = (rel, lo);
            }
            let want = ((hi - lo) * ROWS_PER_BLOCK) as usize;
            match outcome {
                Err(e) => failures.push(format!("scan rel {rel} lo {lo}: {e}")),
                Ok(()) => {
                    if got != want {
                        failures.push(format!("scan rel {rel} lo {lo}: {got} rows, want {want}"));
                    }
                }
            }
        }
        println!("rel {rel} range-scan done ({:?} total)", read_t0.elapsed());
    }
    stop.store(true, Ordering::Relaxed);
    writer.await.unwrap();
    println!(
        "scale probe: {} blocks, {calls} calls, worst {worst:?} at {worst_blk:?}, {} failures",
        RELS * BLOCKS,
        failures.len()
    );
    for f in failures.iter().take(20) {
        println!("  {f}");
    }
    assert!(failures.is_empty(), "{failures:#?}");
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}
