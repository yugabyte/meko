//! Live integration tests against a running YugabyteDB cluster.
//!
//! Ignored by default; run with a cluster up:
//!
//! ```sh
//! cargo test -p yb_pgclient --test live -- --ignored
//! ```
//!
//! Env overrides: `YB_LIVE_TSERVERS` (default `127.0.0.1:9100`,
//! comma-separated), `YB_LIVE_YSQL` (default `127.0.0.1:5433`),
//! `YB_LIVE_DB` (default `yb_storage`).
//!
//! Covers the session/transport surface end to end: call multiplexing,
//! session-expiry recovery, endpoint failover, shared-snapshot multi-op
//! Performs, and read-time pinning under concurrent writes.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use bytes::Bytes;
use yb_pgclient::{BindValue, CondOp, PgClient, PgClientConfig, RangeRead, TableHandle, UpsertRow};
use ysql_client::{HostPorts, YbConnConfig, YsqlClient};

fn env_or(name: &str, default: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| default.to_string())
}

fn tserver_addrs() -> Vec<String> {
    env_or("YB_LIVE_TSERVERS", "127.0.0.1:9100")
        .split(',')
        .map(str::to_string)
        .collect()
}

async fn ysql() -> YsqlClient {
    // A TLS-required cluster (the RF3 benchmark cluster) needs a password
    // and sslmode; a local dev YB needs neither. Both come from env so the
    // same test binary works against either: `YB_LIVE_YSQL_USER`,
    // `YB_LIVE_YSQL_PASSWORD_ENV` (name of an env var holding the
    // password), and `YB_LIVE_YSQL_SSLMODE`.
    let password_env = std::env::var("YB_LIVE_YSQL_PASSWORD_ENV").ok();
    let sslmode = std::env::var("YB_LIVE_YSQL_SSLMODE").ok();
    let conf = YbConnConfig {
        host_ports: env_or("YB_LIVE_YSQL", "127.0.0.1:5433")
            .parse::<HostPorts>()
            .unwrap(),
        user: env_or("YB_LIVE_YSQL_USER", "yugabyte"),
        dbname: env_or("YB_LIVE_DB", "yb_storage"),
        password_env,
        sslmode,
        sslrootcert: None,
        disable_transactional_writes: false,
        hot_path: ysql_client::HotPath::Ysql,
        tserver_rpc_addrs: None,
    };
    YsqlClient::from_yb_config(&conf, "yb_pgclient_live_test", false)
        .await
        .expect("ysql connect")
}

/// Drops + creates `table` with the standard live-test shape and returns
/// its (database_oid, relfilenode).
async fn create_table(ysql: &YsqlClient, table: &str) -> (u32, u32) {
    ysql.execute(&format!("DROP TABLE IF EXISTS {table}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {table} (
                 branch_id TEXT NOT NULL,
                 lsn BIGINT NOT NULL,
                 seq SMALLINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    table_oids(ysql, table).await
}

async fn table_oids(ysql: &YsqlClient, table: &str) -> (u32, u32) {
    let rows = ysql
        .query(
            &format!(
                "SELECT (SELECT oid FROM pg_database WHERE datname = current_database())::bigint,
                        c.oid::bigint, c.relfilenode::bigint
                 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                 WHERE n.nspname = 'public' AND c.relname = '{table}'"
            ),
            &[],
        )
        .await
        .unwrap();
    let db_oid = rows[0].get::<_, i64>(0) as u32;
    let rel_oid = rows[0].get::<_, i64>(1) as u32;
    let relfilenode = rows[0].get::<_, i64>(2) as u32;
    (
        db_oid,
        if relfilenode != 0 {
            relfilenode
        } else {
            rel_oid
        },
    )
}

async fn connect() -> Arc<PgClient> {
    PgClient::connect(PgClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .await
    .expect("pgclient connect")
}

fn upsert_row(table: &TableHandle, branch: &str, lsn: i64, seq: i16, payload: &[u8]) -> UpsertRow {
    UpsertRow::new(
        table,
        vec![BindValue::Text(branch.to_string()), BindValue::I64(lsn)],
        &[
            ("seq", BindValue::I16(seq)),
            ("payload", BindValue::Bytes(Bytes::copy_from_slice(payload))),
        ],
    )
    .unwrap()
}

/// Reads every (lsn, payload) of a branch in one Perform page. Panics if
/// the result doesn't fit one page (callers size their data to fit).
async fn read_branch_one_page(
    client: &PgClient,
    table: &Arc<TableHandle>,
    branch: &str,
) -> Vec<(i64, Bytes)> {
    let read = RangeRead::new(table, vec![BindValue::Text(branch.to_string())])
        .unwrap()
        .targets(table, &["lsn", "payload"])
        .unwrap()
        .limit(100_000);
    let page = client.read_page(table, &read, None).await.expect("read");
    assert!(
        page.paging_state.is_none(),
        "branch {branch} unexpectedly paged; shrink the test data"
    );
    rows_to_lsn_payload(&page.rows)
}

fn rows_to_lsn_payload(rows: &[yb_pgclient::Row]) -> Vec<(i64, Bytes)> {
    rows.iter()
        .map(|r| (r[0].as_i64().unwrap(), r[1].as_bytes().unwrap().clone()))
        .collect()
}

// ---------------------------------------------------------------------
// 1. Bulk writes + descending paged reads, cross-checked against YSQL in
//    both directions (1k rows).
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn bulk_upsert_and_paged_descending_read_cross_check() {
    const TABLE: &str = "pgclient_live_bulk";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");
    let names: Vec<&str> = table.columns.iter().map(|c| c.name.as_str()).collect();
    assert_eq!(names, ["branch_id", "lsn", "seq", "payload"]);

    // 1000 rows over two branches, deliberately awkward payloads.
    let mut rows = Vec::new();
    for branch in ["a", "b"] {
        for lsn in 1..=500i64 {
            let payload = match lsn % 5 {
                0 => Vec::new(),
                1 => vec![lsn as u8, 0x00, 0xff, 0x10],
                _ => format!("{branch}-{lsn}-\u{00e9}\u{4e16}").into_bytes(),
            };
            rows.push(upsert_row(
                &table,
                branch,
                lsn,
                (lsn % 100) as i16,
                &payload,
            ));
        }
    }
    // Batches of 128 keep frames modest; each batch is one Perform.
    for chunk in rows.chunks(128) {
        client.upsert_batch(&table, chunk).await.expect("upsert");
    }

    // One row written via YSQL must be visible over RPC (other direction).
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} (branch_id, lsn, seq, payload)
             VALUES ('a', 501, 1, '\\x00ff10'::bytea)"
        ),
        &[],
    )
    .await
    .unwrap();

    // Forward scan follows the (lsn DESC) clustering; small pages force
    // a long paging chain.
    let read = RangeRead::new(&table, vec![BindValue::Text("a".to_string())])
        .unwrap()
        .cond(&table, "lsn", CondOp::Le, BindValue::I64(501))
        .unwrap()
        .targets(&table, &["lsn", "seq", "payload"])
        .unwrap()
        .limit(37);
    let got = client.read_all(&table, &read).await.expect("read_all");
    assert_eq!(got.len(), 501);

    let expected = ysql
        .query(
            &format!(
                "SELECT lsn, seq, payload FROM {TABLE}
                 WHERE branch_id = 'a' ORDER BY lsn DESC"
            ),
            &[],
        )
        .await
        .unwrap();
    assert_eq!(got.len(), expected.len());
    for (direct, via_ysql) in got.iter().zip(&expected) {
        assert_eq!(direct[0].as_i64().unwrap(), via_ysql.get::<_, i64>(0));
        assert_eq!(direct[1].as_i16().unwrap(), via_ysql.get::<_, i16>(1));
        assert_eq!(
            direct[2].as_bytes().unwrap().as_ref(),
            via_ysql.get::<_, &[u8]>(2)
        );
    }

    // Reverse scan = ascending LSN; cross-check the other direction too.
    let read_asc = read.clone().descending();
    let got_asc = client
        .read_all(&table, &read_asc)
        .await
        .expect("read_all asc");
    let expected_asc = ysql
        .query(
            &format!(
                "SELECT lsn, seq, payload FROM {TABLE}
                 WHERE branch_id = 'a' ORDER BY lsn ASC"
            ),
            &[],
        )
        .await
        .unwrap();
    assert_eq!(got_asc.len(), expected_asc.len());
    for (direct, via_ysql) in got_asc.iter().zip(&expected_asc) {
        assert_eq!(direct[0].as_i64().unwrap(), via_ysql.get::<_, i64>(0));
        assert_eq!(
            direct[2].as_bytes().unwrap().as_ref(),
            via_ysql.get::<_, &[u8]>(2)
        );
    }

    // RPC-written source values survive the YSQL round trip byte-for-byte
    // (upsert semantics also mean re-writing a key replaces the row).
    client
        .upsert_batch(&table, &[upsert_row(&table, "b", 7, 99, b"v2")])
        .await
        .unwrap();
    let via_ysql = ysql
        .query(
            &format!("SELECT seq, payload FROM {TABLE} WHERE branch_id = 'b' AND lsn = 7"),
            &[],
        )
        .await
        .unwrap();
    assert_eq!(via_ysql.len(), 1);
    assert_eq!(via_ysql[0].get::<_, i16>(0), 99);
    assert_eq!(via_ysql[0].get::<_, &[u8]>(1), b"v2");

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 2. Call multiplexing: 10 concurrent tasks over ONE client/connection,
//    interleaving writes and reads.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn multiplexed_concurrent_performs() {
    const TABLE: &str = "pgclient_live_mux";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");
    let session_before = client.session_id();

    let mut tasks = Vec::new();
    for t in 0..10 {
        let client = client.clone();
        let table = table.clone();
        tasks.push(tokio::spawn(async move {
            let branch = format!("task-{t}");
            for round in 0..5i64 {
                let rows: Vec<UpsertRow> = (0..20)
                    .map(|i| {
                        let lsn = round * 20 + i;
                        upsert_row(
                            &table,
                            &branch,
                            lsn,
                            t as i16,
                            format!("{branch}:{lsn}").as_bytes(),
                        )
                    })
                    .collect();
                client.upsert_batch(&table, &rows).await.expect("upsert");
                // Interleave reads so responses genuinely arrive out of
                // order across tasks.
                let got = read_branch_one_page(&client, &table, &branch).await;
                assert_eq!(got.len(), ((round + 1) * 20) as usize);
            }
            let got = read_branch_one_page(&client, &table, &branch).await;
            assert_eq!(got.len(), 100);
            for (lsn, payload) in got {
                assert_eq!(payload.as_ref(), format!("{branch}:{lsn}").as_bytes());
            }
        }));
    }
    for task in tasks {
        task.await.expect("task panicked");
    }

    // A healthy multiplexed run never needed a session recovery.
    assert_eq!(client.session_id(), session_before);
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 2b. Write-session pool: with `write_sessions > 0`, concurrent upsert
//     batches fan over dedicated sessions while reads stay on the
//     shared one; everything lands, reads see it all, and a poisoned
//     pool (every slot) recovers as one unit.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn pooled_write_sessions_fan_out_and_recover() {
    const TABLE: &str = "pgclient_live_pool";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: tserver_addrs(),
        write_sessions: 4,
        ..Default::default()
    })
    .await
    .expect("pgclient connect (pooled)");
    let table = client.open_table(db_oid, table_oid).await.expect("open");
    let session_before = client.session_id();

    let mut tasks = Vec::new();
    for t in 0..8 {
        let client = client.clone();
        let table = table.clone();
        tasks.push(tokio::spawn(async move {
            let branch = format!("pool-{t}");
            for round in 0..5i64 {
                let rows: Vec<UpsertRow> = (0..20)
                    .map(|i| {
                        let lsn = round * 20 + i;
                        upsert_row(
                            &table,
                            &branch,
                            lsn,
                            t as i16,
                            format!("{branch}:{lsn}").as_bytes(),
                        )
                    })
                    .collect();
                client.upsert_batch(&table, &rows).await.expect("upsert");
                let got = read_branch_one_page(&client, &table, &branch).await;
                assert_eq!(got.len(), ((round + 1) * 20) as usize);
            }
        }));
    }
    for task in tasks {
        task.await.expect("task panicked");
    }
    assert_eq!(client.session_id(), session_before, "no recovery needed");

    // Poison every slot: the next op (whichever session it picks) must
    // recover the whole pool and still succeed.
    client.poison_session_for_test();
    client
        .upsert_batch(&table, &[upsert_row(&table, "pool-r", 1, 1, b"post")])
        .await
        .expect("upsert across pooled recovery");
    assert_ne!(client.session_id(), session_before, "pool was replaced");
    let got = read_branch_one_page(&client, &table, "pool-r").await;
    assert_eq!(got.len(), 1);

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 2c. Multi-tserver spread: with THREE distinct tserver endpoints, the
//     pool must open a read session on every endpoint and write sessions
//     across them, so single-page reads and writes fan over all three
//     nodes (the TPC-C parity bug: the old pool pinned everything to one
//     tserver). Meanwhile a MULTI-page scan's continuations must all stay
//     on the ONE session that pinned page 1 (server read-point history is
//     per session) — proven by the absence of any "bound to session /
//     no progress" continuation error over a long paging chain.
//
//     Needs a real 3-endpoint target: set `YB_LIVE_TSERVERS` to three
//     distinct `host:port`s (e.g. the RF3 cluster tservers). Skips
//     (passes trivially) if fewer than three endpoints are configured.
// ---------------------------------------------------------------------
#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires a live 3-endpoint YugabyteDB cluster"]
async fn reads_and_writes_spread_across_tservers() {
    const TABLE: &str = "pgclient_live_spread";
    let endpoints = tserver_addrs();
    let distinct: std::collections::BTreeSet<&String> = endpoints.iter().collect();
    if distinct.len() < 3 {
        eprintln!(
            "reads_and_writes_spread_across_tservers: only {} distinct endpoint(s) \
             ({endpoints:?}); set YB_LIVE_TSERVERS to 3 tservers to exercise spread. Skipping.",
            distinct.len()
        );
        return;
    }

    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // Multi-tablet so the deep chain's continuations page across tablet
    // boundaries (whose leaders sit on different tservers) while still
    // pinning to one client session.
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT   NOT NULL,
                 lsn       BIGINT NOT NULL,
                 seq       SMALLINT NOT NULL,
                 payload   BYTEA  NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC)
             ) SPLIT INTO 9 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    // 4 write sessions over 3 endpoints: read sessions cover all 3, write
    // sessions round-robin (endpoints 0,1,2,0).
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: endpoints.clone(),
        write_sessions: 4,
        ..Default::default()
    })
    .await
    .expect("pgclient connect (spread)");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // The pool physically spans all three tservers.
    let peers = client.connected_peers();
    let distinct_peers: std::collections::BTreeSet<&String> = peers.iter().collect();
    println!(
        "spread: pool peers {peers:?} ({} distinct)",
        distinct_peers.len()
    );
    let (read_ep, write_ep) = client.endpoint_spread();
    assert_eq!(read_ep, 3, "one read session per endpoint");
    assert_eq!(write_ep, 3, "write sessions cover all three endpoints");
    assert_eq!(
        distinct_peers.len(),
        3,
        "pool must hold connections to all three tservers, got {peers:?}"
    );

    // Seed several branches, then hammer single-page reads + upserts. The
    // per-endpoint session round-robin means these calls land on all
    // three tservers; the server forwards any op whose tablet leader is
    // elsewhere, so every call must still succeed.
    let branches: Vec<String> = (0..12).map(|i| format!("spread-{i}")).collect();
    for branch in &branches {
        let rows: Vec<UpsertRow> = (0..20i64)
            .map(|lsn| upsert_row(&table, branch, lsn, 0, format!("{branch}:{lsn}").as_bytes()))
            .collect();
        client
            .upsert_batch(&table, &rows)
            .await
            .expect("seed upsert");
    }
    // ~600 single-page reads + interleaved writes. Reads target the
    // seeded branches (stable count 20); the interleaved writes go to a
    // SEPARATE churn branch so they don't perturb those counts — the
    // point is to drive read AND write traffic through the round-robin,
    // not to re-verify counts.
    for round in 0..50i64 {
        for branch in &branches {
            let got = read_branch_one_page(&client, &table, branch).await;
            assert_eq!(got.len(), 20, "branch {branch} round {round}");
        }
        client
            .upsert_batch(&table, &[upsert_row(&table, "churn", round, 1, b"x")])
            .await
            .expect("interleaved upsert");
    }
    // The pool never needed a recovery (spread reads didn't destabilize
    // it), so it still spans all three endpoints.
    assert_eq!(client.endpoint_spread(), (3, 3), "pool stayed spread");

    // MULTI-page scan: a deep single-branch chain with a tiny page limit
    // forces a long continuation chain. Every continuation re-pins to the
    // session that started the scan; if the round-robin leaked a
    // continuation onto another session, the session-bound guard (or the
    // no-progress guard) would fail it. Reaching the exact row count with
    // no error is the pin assertion.
    const DEEP: i64 = 2_000;
    let deep_rows: Vec<UpsertRow> = (0..DEEP)
        .map(|lsn| upsert_row(&table, "deep", lsn, 0, b"d"))
        .collect();
    for chunk in deep_rows.chunks(128) {
        client.upsert_batch(&table, chunk).await.expect("deep seed");
    }
    let read = RangeRead::new(&table, vec![BindValue::Text("deep".to_string())])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(17); // small pages => ~118 continuations, all pinned
    let mut n = 0usize;
    let mut pages = 0u64;
    let mut paging = None;
    loop {
        let page = client
            .read_page(&table, &read, paging.take())
            .await
            .expect("deep continuation must stay pinned to its session");
        n += page.rows.len();
        pages += 1;
        match page.paging_state {
            Some(next) => paging = Some(next),
            None => break,
        }
    }
    assert_eq!(n, DEEP as usize, "deep pinned scan read every row");
    assert!(
        pages > 50,
        "expected a long paging chain, got {pages} pages"
    );
    println!("spread: deep scan {n} rows over {pages} pinned continuations");

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 3. Session-expiry recovery: poison the session id so the server
//    rejects it ("Unknown session"), and verify the client re-opens a
//    session + tables and the op succeeds transparently.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn session_expiry_recovery() {
    const TABLE: &str = "pgclient_live_sess";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    client
        .upsert_batch(&table, &[upsert_row(&table, "s", 1, 1, b"before")])
        .await
        .expect("first upsert");
    let healthy_session = client.session_id();

    // Fake an expiry: the server has never heard of this id, so the next
    // Perform fails exactly the way a 60s-idle expiry does.
    client.poison_session_for_test();
    client
        .upsert_batch(&table, &[upsert_row(&table, "s", 2, 2, b"after")])
        .await
        .expect("upsert across session recovery");

    let recovered_session = client.session_id();
    assert_ne!(
        recovered_session,
        u64::MAX - 1,
        "session was re-established"
    );
    assert_ne!(
        recovered_session, healthy_session,
        "recovery must open a fresh session"
    );

    // Both writes visible; reads work on the recovered session (and the
    // re-opened table handle).
    let got = read_branch_one_page(&client, &table, "s").await;
    assert_eq!(got.len(), 2);

    // The recovered session survives its own keep-alives / further ops.
    client
        .upsert_batch(&table, &[upsert_row(&table, "s", 3, 3, b"again")])
        .await
        .expect("post-recovery upsert");

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 4. Endpoint failover: a dead endpoint first in the list (and even
//    preferred) must not prevent connecting; the client walks on.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn endpoint_failover_skips_dead_tserver() {
    const TABLE: &str = "pgclient_live_failover";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    // Port 1 refuses connections immediately.
    let dead = "127.0.0.1:1".to_string();
    let mut addrs = vec![dead.clone()];
    addrs.extend(tserver_addrs());
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: addrs,
        preferred_addr: Some(dead.clone()),
        ..Default::default()
    })
    .await
    .expect("connect must fail over past the dead endpoint");
    assert_ne!(client.connected_addr(), dead);

    // And the failed-over connection is fully functional.
    let table = client.open_table(db_oid, table_oid).await.expect("open");
    client
        .upsert_batch(&table, &[upsert_row(&table, "f", 1, 1, b"x")])
        .await
        .expect("upsert");
    let got = read_branch_one_page(&client, &table, "f").await;
    assert_eq!(got.len(), 1);

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 5. Two read ops in ONE Perform observe one snapshot, while the same
//    two reads as separate Performs (with a concurrent writer) do not.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn one_perform_two_ops_share_a_snapshot() {
    const TABLE: &str = "pgclient_live_snap";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Writer: append row i to branch "wa" and then to "wb", in that
    // order, forever. Any single snapshot therefore satisfies
    // count(wa) >= count(wb).
    let stop = Arc::new(AtomicBool::new(false));
    let writer = {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        tokio::spawn(async move {
            let mut i = 1i64;
            while !stop.load(Ordering::Relaxed) && i <= 2_000 {
                for branch in ["wa", "wb"] {
                    client
                        .upsert_batch(&table, &[upsert_row(&table, branch, i, 0, b"w")])
                        .await
                        .expect("writer upsert");
                }
                i += 1;
            }
        })
    };

    let count_read = |branch: &str| {
        RangeRead::new(&table, vec![BindValue::Text(branch.to_string())])
            .unwrap()
            .targets(&table, &["lsn"])
            .unwrap()
            .limit(100_000)
    };

    // One Perform, two ops: the invariant must hold every time.
    for _ in 0..40 {
        let pages = client
            .read_pages(&table, &[count_read("wa"), count_read("wb")])
            .await
            .expect("two-op perform");
        assert!(pages.iter().all(|p| p.paging_state.is_none()));
        let (wa, wb) = (pages[0].rows.len(), pages[1].rows.len());
        assert!(
            wa >= wb,
            "two ops of one Perform saw torn counts: wa={wa} < wb={wb}"
        );
    }

    // Contrast arm (best-effort): the same reads as two separate
    // Performs read "wa" first; by the time "wb" is read the writer has
    // moved on, so wb > wa violations are expected to show up.
    let mut separate_violations = 0;
    for _ in 0..40 {
        let wa = client
            .read_page(&table, &count_read("wa"), None)
            .await
            .expect("read wa")
            .rows
            .len();
        let wb = client
            .read_page(&table, &count_read("wb"), None)
            .await
            .expect("read wb")
            .rows
            .len();
        if wb > wa {
            separate_violations += 1;
        }
    }
    println!(
        "separate-Perform contrast arm: {separate_violations}/40 iterations \
         saw wb > wa (one-Perform arm saw 0)"
    );

    stop.store(true, Ordering::Relaxed);
    writer.await.expect("writer");
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 6. Paged reads under concurrent writes: continuations stay on the
//    first page's snapshot (server-side read serial pinning) — no torn
//    reads; two scans sharing one Perform see identical sets; explicit
//    hybrid-time pinning excludes later writes.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn paged_read_pinning_under_concurrent_writes() {
    const TABLE: &str = "pgclient_live_pin";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Seed 300 rows, then start a writer appending rows with ascending
    // LSNs, one batch (= one commit) at a time.
    let seed: Vec<UpsertRow> = (1..=300)
        .map(|lsn| upsert_row(&table, "p", lsn, 0, format!("seed-{lsn}").as_bytes()))
        .collect();
    for chunk in seed.chunks(128) {
        client.upsert_batch(&table, chunk).await.expect("seed");
    }
    let stop = Arc::new(AtomicBool::new(false));
    let writer = {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        tokio::spawn(async move {
            let mut lsn = 1_000i64;
            while !stop.load(Ordering::Relaxed) && lsn < 5_000 {
                client
                    .upsert_batch(&table, &[upsert_row(&table, "p", lsn, 1, b"late")])
                    .await
                    .expect("writer upsert");
                lsn += 1;
            }
        })
    };

    let read = RangeRead::new(&table, vec![BindValue::Text("p".to_string())])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(23);

    // Full paged read: small pages force a long chain while the writer
    // keeps committing. No torn read: the writer's rows in the snapshot
    // must form a contiguous LSN prefix 1000..=k (each batch is one
    // commit in ascending-LSN order) and every seed row must be present.
    let follow = |first: yb_pgclient::ReadPage| {
        let client = &client;
        let table = &table;
        let read = &read;
        async move {
            let mut rows = rows_to_lsn_payload(&first.rows);
            let mut paging = first.paging_state;
            while let Some(ps) = paging {
                let page = client.read_page(table, read, Some(ps)).await.expect("page");
                rows.extend(rows_to_lsn_payload(&page.rows));
                paging = page.paging_state;
            }
            rows
        }
    };
    let assert_untorn = |rows: &[(i64, Bytes)]| {
        let seed_rows = rows.iter().filter(|(lsn, _)| *lsn <= 300).count();
        assert_eq!(seed_rows, 300, "all seed rows visible");
        let mut writer_lsns: Vec<i64> = rows
            .iter()
            .map(|(lsn, _)| *lsn)
            .filter(|lsn| *lsn >= 1_000)
            .collect();
        writer_lsns.sort_unstable();
        for (i, lsn) in writer_lsns.iter().enumerate() {
            assert_eq!(
                *lsn,
                1_000 + i as i64,
                "writer rows in the snapshot must form a contiguous prefix"
            );
        }
    };
    let first_page = client.read_page(&table, &read, None).await.expect("page 1");
    let paged_rows = follow(first_page).await;
    assert_untorn(&paged_rows);

    // Two identical scans issued as ONE Perform share a snapshot; each
    // then pages independently (their continuations carry the shared
    // read serial). Under a live writer the two full sets must match
    // exactly — this is the replacement for the old capture-ht-then-pin
    // two-step, which this RPC path cannot support (the server withholds
    // its read time; see DESIGN.md).
    let pages = client
        .read_pages(&table, &[read.clone(), read.clone()])
        .await
        .expect("shared-snapshot perform");
    let [pa, pb]: [yb_pgclient::ReadPage; 2] = pages.try_into().ok().expect("two pages");
    let mut a = follow(pa).await;
    let mut b = follow(pb).await;
    assert_untorn(&a);
    a.sort();
    b.sort();
    assert_eq!(a, b, "two scans of one Perform must see one snapshot");

    // Explicit pinning: pin to the server's current hybrid time (clock
    // via YSQL on the same node; HybridTime = micros << 12), write a
    // burst afterwards, and verify the pinned read excludes the burst.
    stop.store(true, Ordering::Relaxed);
    writer.await.expect("writer");
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    let server_micros: i64 = ysql
        .query("SELECT (EXTRACT(EPOCH FROM now()) * 1000000)::bigint", &[])
        .await
        .unwrap()[0]
        .get(0);
    let pin_ht = (server_micros as u64) << 12;
    let before_burst = rows_to_lsn_payload(&client.read_all(&table, &read).await.unwrap());
    for lsn in 10_000..10_050i64 {
        client
            .upsert_batch(&table, &[upsert_row(&table, "p", lsn, 2, b"burst")])
            .await
            .unwrap();
    }
    let pinned = rows_to_lsn_payload(
        &client
            .read_all(&table, &read.clone().read_time_ht(pin_ht))
            .await
            .expect("pinned read_all"),
    );
    let (mut x, mut y) = (before_burst.clone(), pinned);
    x.sort();
    y.sort();
    assert_eq!(x, y, "explicitly pinned read must exclude the later burst");
    let unpinned = client.read_all(&table, &read).await.unwrap();
    assert_eq!(unpinned.len(), before_burst.len() + 50);

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 6b. Multi-node routing: with the client pinned to ONE tserver
//     endpoint, reads (and writes) whose tablet leader lives on a
//     DIFFERENT tserver must still complete — the server-side proxy
//     (pg_client_session -> yb::client) forwards them. A 9-tablet table
//     on an RF3 x 3-node cluster guarantees ~2/3 of the keys have a
//     remote leader; on a single-node cluster this degenerates to the
//     local case and still passes.
//
//     This reproduces the field incident where basebackup reads via
//     PgClient::read_page never completed on a 3-node cluster while
//     everything passed on single-node dev.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn single_endpoint_reads_cover_remote_leader_tablets() {
    const TABLE: &str = "pgclient_live_multinode";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // Same key shape as pageserver.pages: multi-column hash key
    // (TEXT + BIGINTs + SMALLINT), clustered range columns, 9 tablets.
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id   TEXT     NOT NULL,
                 spcnode     BIGINT   NOT NULL,
                 dbnode      BIGINT   NOT NULL,
                 relnode     BIGINT   NOT NULL,
                 forknum     SMALLINT NOT NULL,
                 blknum      BIGINT   NOT NULL,
                 lsn         BIGINT   NOT NULL,
                 payload     BYTEA    NOT NULL,
                 PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                              blknum, lsn DESC)
             ) SPLIT INTO 9 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    // 40 hash keys x 6 blocks, inserted via YSQL so the read side is
    // exercised independently of this client's write path.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE}
             SELECT 'branch-' || b, 1663, 13000, 16384 + r, 0, blk, 100 + blk,
                    ('\\x00ff10')::bytea
             FROM generate_series(0, 9) b, generate_series(0, 3) r,
                  generate_series(0, 5) blk"
        ),
        &[],
    )
    .await
    .unwrap();
    // Per-key 16-bit hash (what routing is keyed on), for diagnostics.
    let keys = ysql
        .query(
            &format!(
                "SELECT DISTINCT branch_id, spcnode, dbnode, relnode, forknum,
                        yb_hash_code(branch_id, spcnode, dbnode, relnode, forknum)::bigint
                 FROM {TABLE} ORDER BY 6"
            ),
            &[],
        )
        .await
        .unwrap();
    assert_eq!(keys.len(), 40);
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    // Pin the client to the FIRST endpoint only: no failover can hide a
    // routing failure. Short rpc_timeout so a hang classifies quickly.
    let one_endpoint = vec![tserver_addrs().remove(0)];
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: one_endpoint.clone(),
        rpc_timeout: std::time::Duration::from_secs(3),
        ..Default::default()
    })
    .await
    .expect("pgclient connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    let mut tasks = Vec::new();
    for row in &keys {
        let branch: String = row.get(0);
        let (spc, db, rel): (i64, i64, i64) = (row.get(1), row.get(2), row.get(3));
        let fork: i16 = row.get(4);
        let hash: i64 = row.get(5);
        let (client, table) = (client.clone(), table.clone());
        tasks.push(tokio::spawn(async move {
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text(branch.clone()),
                    BindValue::I64(spc),
                    BindValue::I64(db),
                    BindValue::I64(rel),
                    BindValue::I16(fork),
                ],
            )
            .unwrap()
            .targets(&table, &["blknum", "lsn", "payload"])
            .unwrap()
            .limit(1000);
            let outcome = tokio::time::timeout(
                std::time::Duration::from_secs(8),
                client.read_page(&table, &read, None),
            )
            .await;
            let label = format!("key {branch}/{rel} hash {hash:#06x}");
            match outcome {
                Err(_) => Err(format!("{label}: HANG (no completion in 8s)")),
                Ok(Err(e)) => Err(format!("{label}: ERROR {e}")),
                Ok(Ok(page)) => {
                    if page.rows.len() == 6 {
                        Ok(label)
                    } else {
                        Err(format!("{label}: got {} rows, want 6", page.rows.len()))
                    }
                }
            }
        }));
    }
    let mut failures = Vec::new();
    let mut ok = 0;
    for task in tasks {
        match task.await.expect("task panicked") {
            Ok(_) => ok += 1,
            Err(f) => failures.push(f),
        }
    }
    println!(
        "single-endpoint ({}) read fan-out: {ok}/40 keys ok, {} failed",
        one_endpoint[0],
        failures.len()
    );
    for f in &failures {
        println!("  {f}");
    }
    assert!(
        failures.is_empty(),
        "reads to tablets with a remote leader must complete: {failures:#?}"
    );

    // Writes through the same single-endpoint client must land on
    // remote-leader tablets too.
    let rows: Vec<UpsertRow> = keys
        .iter()
        .map(|row| {
            let branch: String = row.get(0);
            let (spc, db, rel): (i64, i64, i64) = (row.get(1), row.get(2), row.get(3));
            let fork: i16 = row.get(4);
            UpsertRow::new(
                &table,
                vec![
                    BindValue::Text(branch),
                    BindValue::I64(spc),
                    BindValue::I64(db),
                    BindValue::I64(rel),
                    BindValue::I16(fork),
                    BindValue::I64(999),
                    BindValue::I64(999),
                ],
                &[("payload", BindValue::Bytes(Bytes::from_static(b"w")))],
            )
            .unwrap()
        })
        .collect();
    client
        .upsert_batch(&table, &rows)
        .await
        .expect("upsert to remote-leader tablets");
    let count: i64 = ysql
        .query(
            &format!("SELECT count(*) FROM {TABLE} WHERE blknum = 999"),
            &[],
        )
        .await
        .unwrap()[0]
        .get(0);
    assert_eq!(count, 40);

    // ---- Paged scans (the basebackup shape): hash-eq + range conds +
    // small page limit forces long paging chains through remote-leader
    // tablets. Deepen each key to 120 rows first.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE}
             SELECT 'branch-' || b, 1663, 13000, 16384 + r, 0, blk, 100 + blk,
                    ('\\x00ff10')::bytea
             FROM generate_series(0, 9) b, generate_series(0, 3) r,
                  generate_series(100, 213) blk"
        ),
        &[],
    )
    .await
    .unwrap();
    let mut tasks = Vec::new();
    for row in &keys {
        let branch: String = row.get(0);
        let (spc, db, rel): (i64, i64, i64) = (row.get(1), row.get(2), row.get(3));
        let fork: i16 = row.get(4);
        let hash: i64 = row.get(5);
        let (client, table) = (client.clone(), table.clone());
        tasks.push(tokio::spawn(async move {
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text(branch.clone()),
                    BindValue::I64(spc),
                    BindValue::I64(db),
                    BindValue::I64(rel),
                    BindValue::I16(fork),
                ],
            )
            .unwrap()
            .cond(&table, "blknum", CondOp::Ge, BindValue::I64(0))
            .unwrap()
            .cond(&table, "blknum", CondOp::Lt, BindValue::I64(500))
            .unwrap()
            .cond(&table, "lsn", CondOp::Le, BindValue::I64(100_000))
            .unwrap()
            .targets(&table, &["blknum", "lsn", "payload"])
            .unwrap()
            .limit(17);
            let label = format!("key {branch}/{rel} hash {hash:#06x}");
            let scan = async {
                let mut n = 0usize;
                let mut pages = 0usize;
                let mut paging = None;
                loop {
                    let page = client.read_page(&table, &read, paging.take()).await?;
                    n += page.rows.len();
                    pages += 1;
                    match page.paging_state {
                        Some(next) => paging = Some(next),
                        None => return Ok::<_, yb_pgclient::YbError>((n, pages)),
                    }
                }
            };
            match tokio::time::timeout(std::time::Duration::from_secs(30), scan).await {
                Err(_) => Err(format!("{label}: PAGED SCAN HANG (>30s)")),
                Ok(Err(e)) => Err(format!("{label}: PAGED SCAN ERROR {e}")),
                // 6 + 114 rows blk<500 (blk=999 excluded by the bound).
                Ok(Ok((n, pages))) if n == 120 && pages >= 7 => Ok(()),
                Ok(Ok((n, pages))) => Err(format!(
                    "{label}: got {n} rows in {pages} pages, want 120 in >=7"
                )),
            }
        }));
    }
    let mut failures = Vec::new();
    for task in tasks {
        if let Err(f) = task.await.expect("task panicked") {
            failures.push(f);
        }
    }
    println!("single-endpoint paged scans: {}/40 ok", 40 - failures.len());
    for f in &failures {
        println!("  {f}");
    }
    assert!(
        failures.is_empty(),
        "paged scans must complete: {failures:#?}"
    );

    // ---- Multi-op Perform (read_pages): all 40 scans in ONE Perform —
    // the ops fan out to tablets whose leaders live on all three
    // tservers — then each continuation followed to completion.
    let reads: Vec<RangeRead> = keys
        .iter()
        .map(|row| {
            let branch: String = row.get(0);
            let (spc, db, rel): (i64, i64, i64) = (row.get(1), row.get(2), row.get(3));
            let fork: i16 = row.get(4);
            RangeRead::new(
                &table,
                vec![
                    BindValue::Text(branch),
                    BindValue::I64(spc),
                    BindValue::I64(db),
                    BindValue::I64(rel),
                    BindValue::I16(fork),
                ],
            )
            .unwrap()
            .cond(&table, "blknum", CondOp::Lt, BindValue::I64(500))
            .unwrap()
            .targets(&table, &["blknum", "lsn"])
            .unwrap()
            .limit(29)
        })
        .collect();
    let first_pages = tokio::time::timeout(
        std::time::Duration::from_secs(30),
        client.read_pages(&table, &reads),
    )
    .await
    .expect("multi-op Perform hung >30s")
    .expect("multi-op Perform failed");
    assert_eq!(first_pages.len(), 40);
    for (page, read) in first_pages.into_iter().zip(&reads) {
        let mut n = page.rows.len();
        let mut paging = page.paging_state;
        while let Some(ps) = paging {
            let next = tokio::time::timeout(
                std::time::Duration::from_secs(10),
                client.read_page(&table, read, Some(ps)),
            )
            .await
            .expect("continuation hung >10s")
            .expect("continuation failed");
            n += next.rows.len();
            paging = next.paging_state;
        }
        assert_eq!(n, 120);
    }

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 6c. Reads under sustained writes on a multi-node cluster: the read
//     path (proxy-picked read time via ENSURE_READ_TIME_IS_SET) must
//     make progress while writers hammer the same keys. On multi-node,
//     the write hybrid times are assigned on OTHER tservers' clocks, so
//     reads risk `PgsqlStatusRestartRequiredError` ping-pong that a
//     single-node cluster (one clock, local safe time) never shows.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn reads_progress_under_sustained_writes() {
    const TABLE: &str = "pgclient_live_rw";
    let _ = tracing_subscriber::fmt()
        .with_env_filter("yb_pgclient=debug")
        .try_init();
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id   TEXT     NOT NULL,
                 relnode     BIGINT   NOT NULL,
                 blknum      BIGINT   NOT NULL,
                 lsn         BIGINT   NOT NULL,
                 payload     BYTEA    NOT NULL,
                 PRIMARY KEY ((branch_id, relnode) HASH, blknum, lsn DESC)
             ) SPLIT INTO 9 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    let one_endpoint = vec![tserver_addrs().remove(0)];
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: one_endpoint.clone(),
        write_sessions: 4,
        ..Default::default()
    })
    .await
    .expect("pgclient connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // 12 keys spanning the tablets (hash spreads (branch, relnode)).
    let keys: Vec<(String, i64)> = (0..12).map(|i| (format!("rw-{i}"), 16384 + i)).collect();

    // Writers: hammer every key with ascending-LSN single-row batches.
    let stop = Arc::new(AtomicBool::new(false));
    let mut writers = Vec::new();
    for (branch, rel) in keys.clone() {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        writers.push(tokio::spawn(async move {
            let mut lsn = 1i64;
            while !stop.load(Ordering::Relaxed) {
                let row = UpsertRow::new(
                    &table,
                    vec![
                        BindValue::Text(branch.clone()),
                        BindValue::I64(rel),
                        BindValue::I64(lsn % 50),
                        BindValue::I64(lsn),
                    ],
                    &[("payload", BindValue::Bytes(Bytes::from_static(b"w")))],
                )
                .unwrap();
                client
                    .upsert_batch(&table, &[row])
                    .await
                    .expect("writer upsert");
                lsn += 1;
            }
            lsn
        }));
    }

    // Readers: 20 seconds of tight read loops over all keys via the
    // single endpoint — alternating single-op `read_page` and two-op
    // `read_pages` Performs. The two-op shape is the regression for the
    // stale-read-serial retry bug: under sustained writes, read-restarts
    // (`TryAgainCode restart_read_time`) are routine on multi-node, and
    // a retry that reused the failed attempt's read serial re-read at
    // the failed read time forever, turning one restart into a
    // persistent call failure. Track failures and worst-case latency.
    let mut readers = Vec::new();
    for (branch, rel) in keys.clone() {
        let (client, table) = (client.clone(), table.clone());
        readers.push(tokio::spawn(async move {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(20);
            let mut reads = 0u64;
            let mut failures: Vec<String> = Vec::new();
            let mut max_latency = std::time::Duration::ZERO;
            while std::time::Instant::now() < deadline {
                let read = || {
                    RangeRead::new(
                        &table,
                        vec![BindValue::Text(branch.clone()), BindValue::I64(rel)],
                    )
                    .unwrap()
                    .cond(&table, "lsn", CondOp::Le, BindValue::I64(i64::MAX / 2))
                    .unwrap()
                    .targets(&table, &["blknum", "lsn", "payload"])
                    .unwrap()
                    .limit(500)
                };
                let started = std::time::Instant::now();
                let outcome = if reads % 2 == 0 {
                    tokio::time::timeout(
                        std::time::Duration::from_secs(30),
                        client.read_page(&table, &read(), None),
                    )
                    .await
                    .map(|r| r.map(drop))
                } else {
                    tokio::time::timeout(
                        std::time::Duration::from_secs(30),
                        client.read_pages(&table, &[read(), read()]),
                    )
                    .await
                    .map(|r| r.map(drop))
                };
                match outcome {
                    Err(_) => failures.push(format!("{branch}: HANG >30s")),
                    Ok(Err(e)) => failures.push(format!("{branch}: {e}")),
                    Ok(Ok(())) => {}
                }
                max_latency = max_latency.max(started.elapsed());
                reads += 1;
            }
            (reads, failures, max_latency)
        }));
    }

    let mut total_reads = 0u64;
    let mut all_failures = Vec::new();
    let mut worst = std::time::Duration::ZERO;
    for r in readers {
        let (reads, failures, max_latency) = r.await.expect("reader panicked");
        total_reads += reads;
        all_failures.extend(failures);
        worst = worst.max(max_latency);
    }
    stop.store(true, Ordering::Relaxed);
    let mut total_writes = 0i64;
    for w in writers {
        total_writes += w.await.expect("writer panicked");
    }
    println!(
        "sustained r/w: {total_reads} reads ({} failed, worst latency {worst:?}) \
         against {total_writes} writes via {}",
        all_failures.len(),
        one_endpoint[0]
    );
    for f in all_failures.iter().take(20) {
        println!("  {f}");
    }
    assert!(
        all_failures.is_empty(),
        "reads must make progress under sustained writes: {} failures",
        all_failures.len()
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 6d. LARGE payloads on the exact `pageserver.pages` shape: chains of
//     10–30 rows of 8KB bytea per block. A single fetched page here is
//     a multi-hundred-KB response — the server truncates such pages by
//     SIZE (not row limit), a behavior small-row tests never trigger.
//     Mirrors `pageserver/src/page_store.rs::read_chain_pgclient`'s
//     RangeRead verbatim (hash-eq on the 5 partition cols, blknum eq,
//     lsn <= target, targets incl. payload, limit 32) and its
//     anchor-stop paging loop, over several hundred blocks.
//
//     This reproduces the field incident where `PgClient::read_page`
//     on TPC-C-sized `pageserver.pages` chains never completed while
//     `read_meta` / `scan_slru_range` (small rows) on the SAME session
//     were fine.
// ---------------------------------------------------------------------
#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires a live YugabyteDB cluster"]
async fn large_payload_page_chains_complete() {
    const TABLE: &str = "pgclient_live_pages8k";
    /// Hash keys (relnodes), spread over the 9 tablets.
    const RELS: i64 = 12;
    /// Blocks per relation.
    const BLOCKS: i64 = 40;
    const PAGE_BYTES: usize = 8192;
    /// `DIRECT_CHAIN_PAGE_ROWS` in page_store.rs.
    const CHAIN_PAGE_ROWS: u64 = 32;
    // 10..=100 rows per block: chains longer than one 32-row fetch page
    // force paging continuations whose frames carry ~256KB of payload —
    // the field shape (image-materialiser lag on TPC-C data).
    let chain_len = |rel: i64, blk: i64| 10 + ((rel * BLOCKS + blk) % 91);

    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // The real pageserver.pages schema (architecture/yugabyte.md).
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
    // Chains: per block, row i=0 is the image anchor (is_image,
    // will_init) and i=1.. are deltas, LSN-ascending; 8192-byte payloads.
    for rel in 0..RELS {
        for lo in (0..BLOCKS).step_by(20) {
            let hi = (lo + 20).min(BLOCKS);
            ysql.execute(
                &format!(
                    "INSERT INTO {TABLE}
                     SELECT 'br-large', 1663, 13000, 16384 + {rel}, 0, blk,
                            i = 0, blk * 1000 + i, i = 0,
                            decode(repeat('a5', {PAGE_BYTES}), 'hex')
                     FROM generate_series({lo}, {hi} - 1) blk,
                          LATERAL generate_series(0, 10 + ({rel} * {BLOCKS} + blk) % 91 - 1) i"
                ),
                &[],
            )
            .await
            .unwrap();
        }
    }
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Concurrent write load on the same table (fresh blocks well above
    // the read range): the field cluster was ingesting TPC-C WAL while
    // the chain reads ran.
    let stop = Arc::new(AtomicBool::new(false));
    let writer = {
        let (client, table, stop) = (client.clone(), table.clone(), stop.clone());
        tokio::spawn(async move {
            let mut lsn = 10_000_000i64;
            let payload = Bytes::from(vec![0x5au8; PAGE_BYTES]);
            while !stop.load(Ordering::Relaxed) {
                let rows: Vec<UpsertRow> = (0..8)
                    .map(|i| {
                        UpsertRow::new(
                            &table,
                            vec![
                                BindValue::Text("br-large".to_string()),
                                BindValue::I64(1663),
                                BindValue::I64(13000),
                                BindValue::I64(16384 + (lsn + i) % RELS),
                                BindValue::I16(0),
                                BindValue::I64(100_000 + (lsn + i) % 64),
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
                client
                    .upsert_batch(&table, &rows)
                    .await
                    .expect("writer upsert");
                lsn += 8;
            }
        })
    };

    // Per-block chain read, verbatim `read_chain_pgclient` + its
    // `fetch_chain_rows_pgclient` anchor-stop paging loop.
    let target = i64::MAX / 2;
    let mut latencies = Vec::with_capacity((RELS * BLOCKS) as usize);
    let mut calls = 0u64;
    let mut multi_page_chains = 0u64;
    let mut failures: Vec<String> = Vec::new();
    for rel in 0..RELS {
        for blk in 0..BLOCKS {
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text("br-large".to_string()),
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
            .limit(CHAIN_PAGE_ROWS);

            let started = std::time::Instant::now();
            let mut rows: Vec<yb_pgclient::Row> = Vec::new();
            let mut paging = None;
            let mut pages = 0u64;
            let outcome = loop {
                calls += 1;
                pages += 1;
                match tokio::time::timeout(
                    std::time::Duration::from_secs(30),
                    client.read_page(&table, &read, paging.take()),
                )
                .await
                {
                    Err(_) => {
                        break Err("HANG: read_page gave no completion in 30s".to_string());
                    }
                    Ok(Err(e)) => break Err(format!("ERROR: {e}")),
                    Ok(Ok(page)) => {
                        // Anchor stop (ChainFetchState): rows arrive
                        // LSN-descending; stop at the first will_init row.
                        let mut done = false;
                        for row in page.rows {
                            let will_init = row[2].as_bool().unwrap();
                            rows.push(row);
                            if will_init {
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
            let elapsed = started.elapsed();
            latencies.push(elapsed);
            if pages > 1 {
                multi_page_chains += 1;
            }
            match outcome {
                Err(e) => failures.push(format!("rel {rel} blk {blk}: {e} after {elapsed:?}")),
                Ok(()) => {
                    let want = chain_len(rel, blk) as usize;
                    if rows.len() != want {
                        failures.push(format!(
                            "rel {rel} blk {blk}: got {} rows, want {want}",
                            rows.len()
                        ));
                    } else {
                        // Chain integrity: LSN-descending, 8KB payloads,
                        // anchor at the bottom.
                        for (i, row) in rows.iter().enumerate() {
                            let lsn = row[0].as_i64().unwrap();
                            assert_eq!(
                                lsn,
                                blk * 1000 + (want - 1 - i) as i64,
                                "rel {rel} blk {blk} row {i}"
                            );
                            assert_eq!(
                                row[3].as_bytes().unwrap().len(),
                                PAGE_BYTES,
                                "rel {rel} blk {blk} row {i} payload"
                            );
                        }
                        assert!(
                            rows.last().unwrap()[2].as_bool().unwrap(),
                            "rel {rel} blk {blk} anchor"
                        );
                    }
                }
            }
        }
    }
    stop.store(true, Ordering::Relaxed);
    writer.await.expect("writer");

    latencies.sort();
    let pct = |p: usize| latencies[(latencies.len() - 1) * p / 100];
    println!(
        "large-payload chains: {} blocks ({multi_page_chains} paged), {calls} read_page \
         calls, latency p50 {:?} p99 {:?} max {:?}, {} failures",
        RELS * BLOCKS,
        pct(50),
        pct(99),
        pct(100),
        failures.len()
    );
    for f in failures.iter().take(20) {
        println!("  {f}");
    }
    assert!(
        failures.is_empty(),
        "large-payload chain reads must complete: {} failures (first: {})",
        failures.len(),
        failures.first().map(String::as_str).unwrap_or("")
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 6e. Deep paged chain reads on HOT blocks — blocks being written WHILE
//     the paged scan runs, with the lsn bound ABOVE the writer's rows
//     (the getpage shape: target = current WAL end). The field failure
//     mode this regresses: an unclamped proxy-picked read point keeps
//     its clock-skew uncertainty window (500ms) open for the whole
//     scan, so the writer's commits into the scanned range fail a late
//     continuation with `TryAgainCode restart_read_time`; the
//     continuation is snapshot-pinned so no in-place retry can heal it,
//     and rescanning from scratch under sustained writes livelocks the
//     same way — `read_chain` on TPC-C-hot pages never completed.
//     `clamp_uncertainty_window` on unpinned reads removes the window;
//     this test fails within seconds without it.
// ---------------------------------------------------------------------
#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires a live YugabyteDB cluster"]
async fn hot_block_chain_reads_complete_under_writes() {
    const TABLE: &str = "pgclient_live_hot_chains";
    const BLOCKS: i64 = 4;
    /// Pre-existing chain depth: ~94 paged continuations at limit 32,
    /// all `will_init=false` (the #217 shape: no anchor, the read pages
    /// to range exhaustion).
    const CHAIN: i64 = 3000;
    const REL: i64 = 16384;
    const READERS: usize = 8;

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
             ) SPLIT INTO 3 TABLETS"
        ),
        &[],
    )
    .await
    .unwrap();
    for blk in 0..BLOCKS {
        ysql.execute(
            &format!(
                "INSERT INTO {TABLE}
                 SELECT 'br-hot', 1663, 13000, {REL}, 0, {blk},
                        false, {blk} * 100000 + i, false,
                        decode(repeat('5a', 24), 'hex')
                 FROM generate_series(0, {CHAIN} - 1) i"
            ),
            &[],
        )
        .await
        .unwrap();
    }
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // The writer keeps appending fresh higher-LSN deltas to the SAME
    // hot blocks the readers are paging through.
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
                                BindValue::Text("br-hot".to_string()),
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
                client
                    .upsert_batch(&table, &rows)
                    .await
                    .expect("writer upsert");
                lsn += 16;
            }
        })
    };

    // Concurrent full-chain reads, verbatim `read_chain_pgclient` +
    // anchor-stop loop; lsn bound ABOVE everything (incl. the writer),
    // like a getpage at the current WAL end.
    let mut tasks = Vec::new();
    for i in 0..READERS {
        let (client, table) = (client.clone(), table.clone());
        tasks.push(tokio::spawn(async move {
            let blk = i as i64 % BLOCKS;
            let read = RangeRead::new(
                &table,
                vec![
                    BindValue::Text("br-hot".to_string()),
                    BindValue::I64(1663),
                    BindValue::I64(13000),
                    BindValue::I64(REL),
                    BindValue::I16(0),
                ],
            )
            .unwrap()
            .cond(&table, "blknum", CondOp::Eq, BindValue::I64(blk))
            .unwrap()
            .cond(&table, "lsn", CondOp::Le, BindValue::I64(i64::MAX / 2))
            .unwrap()
            .targets(&table, &["lsn", "is_image", "will_init", "payload"])
            .unwrap()
            .limit(32);
            let mut got = 0usize;
            let mut pages = 0u64;
            let mut paging = None;
            loop {
                pages += 1;
                let page = match tokio::time::timeout(
                    std::time::Duration::from_secs(30),
                    client.read_page(&table, &read, paging.take()),
                )
                .await
                {
                    Err(_) => return Err(format!("blk {blk}: HANG after {pages} pages")),
                    Ok(Err(e)) => return Err(format!("blk {blk}: after {pages} pages: {e}")),
                    Ok(Ok(page)) => page,
                };
                got += page.rows.len();
                if page.rows.iter().any(|r| r[2].as_bool().unwrap()) {
                    return Err(format!("blk {blk}: unexpected anchor"));
                }
                if pages > 1000 {
                    return Err(format!("blk {blk}: runaway paging ({got} rows)"));
                }
                match page.paging_state {
                    Some(next) => paging = Some(next),
                    None => break,
                }
            }
            // The scan's snapshot covers at least the pre-existing chain
            // (writer rows may or may not be visible depending on when
            // the read point landed).
            if got < CHAIN as usize {
                return Err(format!("blk {blk}: short chain: {got} rows"));
            }
            Ok(())
        }));
    }
    let mut failures = Vec::new();
    for task in tasks {
        if let Err(e) = task.await.expect("reader task") {
            failures.push(e);
        }
    }
    stop.store(true, Ordering::Relaxed);
    writer.await.expect("writer");
    assert!(
        failures.is_empty(),
        "hot-block chain reads must complete under sustained writes: {failures:#?}"
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// 7. Single-tablet batch atomicity: rows of one upsert_batch that share
//    a partition key commit at one hybrid time, so a concurrent snapshot
//    reader sees complete batches only.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn single_tablet_batch_atomicity() {
    const TABLE: &str = "pgclient_live_atomic";
    const BATCH: usize = 10;
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    let writer = {
        let (client, table) = (client.clone(), table.clone());
        tokio::spawn(async move {
            for batch in 0..60i64 {
                let rows: Vec<UpsertRow> = (0..BATCH as i64)
                    .map(|i| upsert_row(&table, "atom", batch * BATCH as i64 + i, 0, b"a"))
                    .collect();
                client.upsert_batch(&table, &rows).await.expect("batch");
            }
        })
    };

    // Every observed count must be a whole number of batches.
    let mut observed = Vec::new();
    while !writer.is_finished() {
        let n = read_branch_one_page(&client, &table, "atom").await.len();
        assert_eq!(
            n % BATCH,
            0,
            "reader saw a partial batch ({n} rows): single-tablet batches must be atomic"
        );
        observed.push(n);
    }
    writer.await.expect("writer");
    let final_n = read_branch_one_page(&client, &table, "atom").await.len();
    assert_eq!(final_n, 60 * BATCH);
    println!(
        "atomicity: {} concurrent reads, all multiples of {BATCH} (max seen {})",
        observed.len(),
        observed.iter().max().unwrap_or(&0)
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// Idle-close + reopen against a real cluster: a session that sits idle
// past a short idle_timeout is swept Cold (its connection dropped), and
// the next read transparently reopens with a FRESH server session id and
// succeeds — without re-running OpenTable (the cached handle stays
// valid). Also confirms a paged-scan continuation whose session was
// idle-closed fails loudly rather than silently switching snapshots.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn idle_close_then_reopen_without_opentable() {
    const TABLE: &str = "pgclient_live_idle";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    // Short idle timeout so the sweep closes the session immediately.
    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: tserver_addrs(),
        idle_timeout: Some(std::time::Duration::from_millis(1)),
        ..Default::default()
    })
    .await
    .expect("pgclient connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Warm the pool with a write + read.
    client
        .upsert_batch(&table, &[upsert_row(&table, "idle", 1, 1, b"v1")])
        .await
        .expect("first upsert");
    let got = read_branch_one_page(&client, &table, "idle").await;
    assert_eq!(got.len(), 1);
    let warm_id = client.session_id();
    assert_eq!(
        client.warm_session_count_for_test().await,
        client.connected_peers().len(),
        "every pool session warm after use"
    );

    // Go idle, then sweep: all sessions close (Cold).
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    let warm = client.sweep_idle_for_test().await;
    assert_eq!(warm, 0, "idle sweep must close every session");

    // Next read reopens transparently and returns the same data.
    let got = read_branch_one_page(&client, &table, "idle").await;
    assert_eq!(got.len(), 1, "reopened read must still see the row");
    let reopened_id = client.session_id();
    assert_ne!(reopened_id, warm_id, "reopen mints a fresh server session");

    // The reopened session serves writes too, proving the cached table
    // handle stayed valid across the reopen (no OpenTable needed).
    client
        .upsert_batch(&table, &[upsert_row(&table, "idle", 2, 2, b"v2")])
        .await
        .expect("upsert on reopened session");
    assert_eq!(read_branch_one_page(&client, &table, "idle").await.len(), 2);

    // Scan-pin interaction: an unpinned paged continuation whose session
    // was idle-closed (new session id) must fail loudly — the old
    // read-point history is gone — rather than serve a wrong snapshot.
    for i in 0..500i64 {
        client
            .upsert_batch(&table, &[upsert_row(&table, "scan", i, 0, &[0u8; 256])])
            .await
            .expect("scan seed");
    }
    let read = RangeRead::new(&table, vec![BindValue::Text("scan".into())])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(50);
    let page1 = client.read_page(&table, &read, None).await.expect("page 1");
    let cont = page1
        .paging_state
        .expect("a 500-row/50-limit scan must page");
    // Force the pinning session Cold, then continue: must fail loudly.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    client.sweep_idle_for_test().await;
    let err = match client.read_page(&table, &read, Some(cont)).await {
        Ok(_) => panic!("continuation on an idle-closed session must fail loudly"),
        Err(e) => e,
    };
    let msg = err.to_string();
    assert!(
        msg.contains("session") || msg.contains("recovered") || msg.contains("restart"),
        "unexpected continuation error: {err}"
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// idle_timeout = None keeps the previous always-warm behavior: an idle
// sweep never closes a session.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn idle_timeout_none_never_closes() {
    const TABLE: &str = "pgclient_live_neverclose";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = PgClient::connect(PgClientConfig {
        tserver_addrs: tserver_addrs(),
        idle_timeout: None,
        ..Default::default()
    })
    .await
    .expect("pgclient connect");
    let table = client.open_table(db_oid, table_oid).await.expect("open");
    client
        .upsert_batch(&table, &[upsert_row(&table, "warm", 1, 1, b"v")])
        .await
        .expect("upsert");

    let total = client.connected_peers().len();
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    let warm = client.sweep_idle_for_test().await;
    assert_eq!(
        warm, total,
        "idle_timeout=None must keep every session warm"
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// Reading a branch that was never written returns zero rows (not an
// error), and a write to another branch doesn't leak into it. The
// empty-scan case (cf. YB client-test.cc TestScanEmptyTable).
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn empty_branch_read_returns_no_rows() {
    const TABLE: &str = "pgclient_live_empty";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Never-written branch: clean, empty, no error.
    assert!(
        read_branch_one_page(&client, &table, "ghost")
            .await
            .is_empty()
    );

    // A write to a different branch must not make "ghost" non-empty.
    client
        .upsert_batch(&table, &[upsert_row(&table, "real", 1, 1, b"v")])
        .await
        .expect("upsert");
    assert!(
        read_branch_one_page(&client, &table, "ghost")
            .await
            .is_empty()
    );
    assert_eq!(read_branch_one_page(&client, &table, "real").await.len(), 1);

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// An empty upsert batch is a no-op that succeeds and writes nothing
// (cf. TestEmptyBatch).
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn empty_upsert_batch_is_noop() {
    const TABLE: &str = "pgclient_live_emptybatch";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    client
        .upsert_batch(&table, &[])
        .await
        .expect("empty batch is ok");
    assert!(
        read_branch_one_page(&client, &table, "any")
            .await
            .is_empty()
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// A batch that repeats an already-present key alongside a new key
// flushes cleanly: the repeat is overwritten (upsert) and the new key
// still lands — one row's collision doesn't abort the batch
// (cf. TestBatchWithDuplicates).
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn batch_with_duplicate_key_overwrites_and_keeps_others() {
    const TABLE: &str = "pgclient_live_dup";
    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;

    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    // Seed lsn=1.
    client
        .upsert_batch(&table, &[upsert_row(&table, "dup", 1, 1, b"original")])
        .await
        .expect("seed");

    // One batch: rewrite lsn=1 and add lsn=2. Must not error.
    client
        .upsert_batch(
            &table,
            &[
                upsert_row(&table, "dup", 1, 1, b"overwritten"),
                upsert_row(&table, "dup", 2, 2, b"fresh"),
            ],
        )
        .await
        .expect("a batch with a repeated key must still flush");

    let mut rows = read_branch_one_page(&client, &table, "dup").await;
    rows.sort_by_key(|(lsn, _)| *lsn);
    assert_eq!(rows.len(), 2);
    assert_eq!(rows[0], (1, Bytes::from_static(b"overwritten")));
    assert_eq!(rows[1], (2, Bytes::from_static(b"fresh")));

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

// ---------------------------------------------------------------------
// Randomized upsert stream cross-checked against an in-memory model.
// Over a small key space (so overwrites recur), a seeded sequence of
// upserts must leave YB exactly matching the model — the write / read /
// decode round trip under churn (cf. TestRandomWriteOperation, upsert-
// only since the hot path never deletes via the client). Set
// YB_LIVE_SEED to reproduce a failure.
// ---------------------------------------------------------------------
#[tokio::test]
#[ignore = "requires a live YugabyteDB cluster"]
async fn randomized_upsert_cross_check() {
    use std::collections::HashMap;
    const TABLE: &str = "pgclient_live_random";
    const BRANCH: &str = "rnd";
    const KEYSPACE: u64 = 64;
    const ITERS: usize = 2000;

    let ysql = ysql().await;
    let (db_oid, table_oid) = create_table(&ysql, TABLE).await;
    let client = connect().await;
    let table = client.open_table(db_oid, table_oid).await.expect("open");

    let seed: u64 = env_or("YB_LIVE_SEED", "88172645463325252")
        .parse()
        .expect("YB_LIVE_SEED must be a u64");
    // xorshift64* — deterministic, no external dependency.
    let mut state = seed | 1;
    let mut next = || {
        state ^= state >> 12;
        state ^= state << 25;
        state ^= state >> 27;
        state.wrapping_mul(0x2545_F491_4F6C_DD1D)
    };

    let mut model: HashMap<i64, Bytes> = HashMap::new();
    for i in 0..ITERS {
        let lsn = (next() % KEYSPACE) as i64 + 1; // 1..=KEYSPACE
        let seq = (next() % 100) as i16;
        let payload = Bytes::from(format!("v{i}-{}", next() % 1000).into_bytes());
        client
            .upsert_batch(&table, &[upsert_row(&table, BRANCH, lsn, seq, &payload)])
            .await
            .expect("upsert");
        model.insert(lsn, payload);

        if i % 200 == 199 {
            let got: HashMap<i64, Bytes> = read_branch_one_page(&client, &table, BRANCH)
                .await
                .into_iter()
                .collect();
            assert_eq!(got, model, "model mismatch at iter {i} (seed {seed})");
        }
    }
    let got: HashMap<i64, Bytes> = read_branch_one_page(&client, &table, BRANCH)
        .await
        .into_iter()
        .collect();
    assert_eq!(got, model, "final model mismatch (seed {seed})");

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}
