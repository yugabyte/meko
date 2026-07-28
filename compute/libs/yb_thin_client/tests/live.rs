//! Live smoke test for `yb_thin_client` against a running YugabyteDB.
//!
//! Requires the REAL shim (not the stub) and a reachable tserver:
//!
//! ```sh
//! LD_LIBRARY_PATH=<dir with libyb_thin_client.so> \
//!   cargo test -p yb_thin_client --test live -- --ignored --nocapture
//! ```
//!
//! Env overrides: `YB_LIVE_TSERVERS` (default `127.0.0.1:9100`),
//! `YB_LIVE_YSQL` (default `127.0.0.1:5433`), `YB_LIVE_DB` (default
//! `yugabyte`). Exercises the full ABI round-trip through the shim:
//! connect (Heartbeat/session), open_table (schema), upsert (write
//! Perform), read_page (read Perform). A signature mismatch at the FFI
//! boundary would surface here as a crash/failure, so this doubles as an
//! ABI sanity check for the prebuilt shim.

use bytes::Bytes;
use yb_thin_client::{BindValue, RangeRead, ReadOp, TsClient, TsClientConfig, UpsertRow};
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
    let conf = YbConnConfig {
        host_ports: env_or("YB_LIVE_YSQL", "127.0.0.1:5433")
            .parse::<HostPorts>()
            .unwrap(),
        user: env_or("YB_LIVE_YSQL_USER", "yugabyte"),
        dbname: env_or("YB_LIVE_DB", "yugabyte"),
        password_env: std::env::var("YB_LIVE_YSQL_PASSWORD_ENV").ok(),
        sslmode: std::env::var("YB_LIVE_YSQL_SSLMODE").ok(),
        sslrootcert: None,
        disable_transactional_writes: false,
        hot_path: ysql_client::HotPath::Ysql,
        tserver_rpc_addrs: None,
        tsclient_read_sessions: None,
        tsclient_write_sessions: None,
        tsclient_sessions_per_conn: None,
    };
    YsqlClient::from_yb_config(&conf, "yb_thin_client_live", false)
        .await
        .expect("ysql connect")
}

/// `(db_oid, table_oid)` for a `public` table — table_oid is the
/// relfilenode when set (YB's DocDB table id), else the pg_class oid.
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

#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn smoke_connect_open_upsert_read() {
    const TABLE: &str = "thin_smoke";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
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
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    // connect — loads the shim, does a Heartbeat, opens a server session.
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("thin-client connect");

    // open_table — fetches the schema; the four columns must come back.
    let table = client.open_table(db_oid, table_oid).expect("open_table");
    for want in ["branch_id", "lsn", "seq", "payload"] {
        assert!(
            table.columns.iter().any(|c| c.name == want),
            "open_table missing column {want}; got {:?}",
            table.columns.iter().map(|c| &c.name).collect::<Vec<_>>()
        );
    }

    // upsert — two rows (branches "s" and "t") in ONE write Perform,
    // exercising a cross-key write batch. UpsertRow::new resolves the value
    // column names to ids against the schema.
    let mk_row = |branch: &str, lsn: i64, seq: i16, payload: &'static [u8]| {
        UpsertRow::new(
            &table,
            vec![BindValue::Text(branch.into()), BindValue::I64(lsn)],
            &[
                ("seq", BindValue::I16(seq)),
                ("payload", BindValue::Bytes(Bytes::from_static(payload))),
            ],
        )
        .expect("build upsert row")
    };
    client
        .upsert(&[mk_row("s", 1, 1, b"hello-s"), mk_row("t", 2, 2, b"hello-t")])
        .await
        .expect("upsert");

    // read — the two branches as a 2-op batch: ONE Perform, one snapshot,
    // one RPC. results[i] corresponds to ops[i]. The shim decodes each page
    // into typed cells; we assert the exact rows we wrote come back with
    // targets (lsn, payload) in order.
    let read_s = RangeRead::new(&table, vec![BindValue::Text("s".into())])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(100);
    let read_t = RangeRead::new(&table, vec![BindValue::Text("t".into())])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(100);
    let batch = client
        .read(
            &[
                ReadOp {
                    table: &table,
                    read: &read_s,
                    paging_state: None,
                },
                ReadOp {
                    table: &table,
                    read: &read_t,
                    paging_state: None,
                },
            ],
            0,
        )
        .await
        .expect("read batch");
    assert_eq!(batch.ops.len(), 2, "expected 2 op results");
    for (i, (branch, lsn, payload)) in [
        ("s", 1i64, b"hello-s".as_ref()),
        ("t", 2, b"hello-t".as_ref()),
    ]
    .into_iter()
    .enumerate()
    {
        let rows = &batch.ops[i].rows;
        assert_eq!(rows.len(), 1, "branch {branch}: expected one row");
        assert_eq!(rows[0].len(), 2, "branch {branch}: expected (lsn, payload)");
        assert_eq!(rows[0][0].as_i64().unwrap(), lsn, "branch {branch}: lsn");
        assert_eq!(
            rows[0][1].as_bytes().unwrap().as_ref(),
            payload,
            "branch {branch}: payload"
        );
    }
    println!(
        "smoke OK — 2-op batch, used_read_time_ht={}",
        batch.used_read_time_ht
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

/// Repro for the `branch_ancestor` wedge: a forward `limit(1)` read on a table
/// whose PK clusters `lsn DESC` must return the row with the HIGHEST lsn (the
/// newest), because storage order == PK order == lsn-descending. This is
/// exactly what `PageStore::read_meta` relies on to fetch the latest checkpoint
/// (`nextXid`) — a stale (oldest) result makes a branch start with a too-low
/// `nextXid`, so committed ancestor tuples look "in the future" and every user
/// table reads as "does not exist". If the shim's forward scan ignores the
/// column's DESC modifier and returns the LOWEST lsn, this fails.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_desc_pk_limit1_newest() {
    use yb_thin_client::RangeRead;
    const TABLE: &str = "descpk_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // Mirrors amp_storage.meta: hash key + a single `lsn DESC` clustering col.
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, lsn BIGINT NOT NULL, payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Three versions of the same key at ascending lsns (like checkpoint updates).
    ysql.execute(
        &format!("INSERT INTO {TABLE} VALUES ('m',10,'\\x0a'),('m',20,'\\x14'),('m',30,'\\x1e')"),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    // Forward scan, lsn <= 100, limit 1 — EXACTLY read_meta's shape. Must be lsn=30.
    let read = RangeRead::new(&table, vec![BindValue::Text("m".into())])
        .unwrap()
        .cond(
            &table,
            "lsn",
            yb_thin_client::CondOp::Le,
            BindValue::I64(100),
        )
        .unwrap()
        .targets(&table, &["lsn"])
        .unwrap()
        .limit(1);
    let page = client.read_page(&table, &read, None).await.expect("read");
    let seen = page.rows.first().map(|r| r[0].as_i64().unwrap());
    println!(
        "repro desc-pk limit1: forward read of ((k) HASH, lsn DESC) limit 1 -> lsn={seen:?} (want Some(30), the NEWEST)"
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        seen,
        Some(30),
        "forward limit(1) on a `lsn DESC` PK returned the WRONG row (want newest=30) — \
         read_meta would fetch a stale checkpoint/nextXid, breaking branch visibility"
    );
}

/// Repro for cross-key contamination under CONCURRENCY — the branch's
/// pageserver fires many getpages at once over 4 read sessions. If the shim
/// mis-routes responses across concurrent reads (wrong key's row returned),
/// `get_checkpoint` can get another meta key's (older) row. Each of N keys has
/// a unique payload; concurrent reads must each return their OWN key's row.
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_concurrent_multikey_reads() {
    use std::sync::Arc;
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "ckey_repro";
    const N: i64 = 64;
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL, tag BIGINT NOT NULL,
                 PRIMARY KEY ((branch_id, key) HASH, lsn DESC))"
        ),
        &[],
    ).await.unwrap();
    // Key i: two versions (lsn 100 tag=i, lsn 200 tag=i+1000). Reading key i
    // capped at lsn<=150 must return tag=i (its own lsn-100 row).
    ysql.execute(
        &format!("INSERT INTO {TABLE} SELECT 'm', set_byte(set_byte('\\x0000'::bytea,0,3),1,g), 100, g FROM generate_series(0,{}) g", N-1),
        &[],
    ).await.unwrap();
    ysql.execute(
        &format!("INSERT INTO {TABLE} SELECT 'm', set_byte(set_byte('\\x0000'::bytea,0,3),1,g), 200, g+1000 FROM generate_series(0,{}) g", N-1),
        &[],
    ).await.unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        read_sessions: 4,
        write_sessions: 4,
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");
    // Fire all N key-reads concurrently, many rounds, capped so the "own row"
    // is lsn=100 (tag=i) — NOT the lsn=200 row and NOT another key's row.
    let mut wrong = 0usize;
    for _round in 0..20 {
        let mut handles = Vec::new();
        for i in 0..N {
            let (c, t) = (Arc::clone(&client), Arc::clone(&table));
            handles.push(tokio::spawn(async move {
                let key = vec![0x03u8, i as u8];
                let read = RangeRead::new(
                    &t,
                    vec![
                        BindValue::Text("m".into()),
                        BindValue::Bytes(Bytes::from(key)),
                    ],
                )
                .unwrap()
                .cond(&t, "lsn", CondOp::Le, BindValue::I64(150))
                .unwrap()
                .targets(&t, &["tag"])
                .unwrap()
                .limit(1);
                let got = c
                    .read_page(&t, &read, None)
                    .await
                    .expect("read")
                    .rows
                    .first()
                    .map(|r| r[0].as_i64().unwrap());
                (i, got)
            }));
        }
        for h in handles {
            let (i, got) = h.await.unwrap();
            if got != Some(i) {
                wrong += 1;
                if wrong <= 5 {
                    println!("  MISMATCH key {i}: got {got:?} want Some({i})");
                }
            }
        }
    }
    println!(
        "repro concurrent multikey: {wrong} wrong results across {} reads",
        N * 20
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        wrong, 0,
        "concurrent reads returned WRONG key's row {wrong} times — shim mis-routes concurrent read responses"
    );
}

/// Reproduce the EXACT failing read: `read_meta` on the real `amp_storage.meta`
/// shape — 18-byte CHECKPOINT_KEY (`03 00..00 01`), real-magnitude lsns
/// spanning the cap — capped at the branch point. Must return the newest row
/// AT OR BELOW the cap. In production this returned the OLDEST.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_real_ckpt_read() {
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "realck_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id, key) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Real CHECKPOINT_KEY (18 bytes) + OTHER meta keys (controlfile, dbdir, a
    // reldir), ALL with a row at 22469680 and 22889880 — mirroring how main's
    // meta table holds many keys that each got their initial row at the same
    // early lsn. Reading CHECKPOINT_KEY capped must return CHECKPOINT's own
    // newest<=cap, not another key's early row.
    let ck_hex = format!("03{}01", "00".repeat(16)); // CHECKPOINT_KEY
    let other_keys = [
        format!("04{}01", "00".repeat(16)), // CONTROLFILE-ish
        format!("05{}01", "00".repeat(16)),
        format!("00{}0affffffff", "00".repeat(11)), // reldir-ish
        format!("06{}01", "00".repeat(16)),
    ];
    let mut vals = vec![
        format!("('m','\\x{ck_hex}',22469680,'\\x00')"),
        format!("('m','\\x{ck_hex}',22889880,'\\x01')"),
        format!("('m','\\x{ck_hex}',22891336,'\\x02')"),
        format!("('m','\\x{ck_hex}',22973632,'\\x03')"),
    ];
    for k in &other_keys {
        vals.push(format!("('m','\\x{k}',22469680,'\\xee')"));
        vals.push(format!("('m','\\x{k}',22889880,'\\xee')"));
    }
    ysql.execute(
        &format!("INSERT INTO {TABLE} VALUES {}", vals.join(",")),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");
    let mut ck = vec![0u8; 18];
    ck[0] = 0x03;
    ck[17] = 0x01;
    let read = RangeRead::new(
        &table,
        vec![
            BindValue::Text("m".into()),
            BindValue::Bytes(Bytes::from(ck)),
        ],
    )
    .unwrap()
    .cond(&table, "lsn", CondOp::Le, BindValue::I64(22891280))
    .unwrap()
    .targets(&table, &["lsn"])
    .unwrap()
    .limit(1);
    let seen = client
        .read_page(&table, &read, None)
        .await
        .expect("read")
        .rows
        .first()
        .map(|r| r[0].as_i64().unwrap());
    println!(
        "repro real ckpt: lsn<=22891280 limit1 -> {seen:?} (want Some(22889880); bug=Some(22469680))"
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        seen,
        Some(22889880),
        "real-shape capped read returned wrong row (want newest<=cap=22889880)"
    );
}

/// THE `branch_ancestor` root cause: a forward `limit(1)` read on a `lsn DESC`
/// PK with an UPPER-BOUND `lsn <= X` BELOW the max stored lsn must return the
/// newest row AT OR BELOW X — not the oldest. This is `read_meta` reading an
/// ancestor's checkpoint capped at the branch point (`lsn <= ancestor_lsn`),
/// where the ancestor has newer rows above the cap. Earlier repros used
/// `lsn <= huge` (cap above all rows), masking it. If the shim returns the
/// OLDEST matching row when the cap excludes the top, a branch reads the
/// ancestor's initial checkpoint (nextXid=0) → ancestor tuples invisible.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_desc_pk_capped_limit1() {
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "cappk_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, lsn BIGINT NOT NULL, payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT 'm', g*100, '\\x00'::bytea FROM generate_series(1,16) g"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");
    // lsns 100..1600; cap at 850 → newest <=850 is 800.
    let read = RangeRead::new(&table, vec![BindValue::Text("m".into())])
        .unwrap()
        .cond(&table, "lsn", CondOp::Le, BindValue::I64(850))
        .unwrap()
        .targets(&table, &["lsn"])
        .unwrap()
        .limit(1);
    let seen = client
        .read_page(&table, &read, None)
        .await
        .expect("read")
        .rows
        .first()
        .map(|r| r[0].as_i64().unwrap());
    println!(
        "repro capped desc-pk limit1: lsn<=850 limit 1 -> {seen:?} (want Some(800) newest<=cap; bug=Some(100) oldest)"
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        seen,
        Some(800),
        "capped forward limit(1) on lsn DESC PK returned wrong row (want newest<=cap=800) — \
         read_meta reads a stale ancestor checkpoint when the cap excludes newer rows"
    );
}

/// Repro for CROSS-CLIENT read-after-write visibility — the actual
/// `branch_ancestor` shape. Client A (main's pageserver) writes newer
/// versions of a key via THIN upserts; client B (the branch's fresh
/// pageserver) then reads via a fresh connection. `read_after_write_visibility`
/// covered ONE client; this covers two. If B's fresh read doesn't see A's
/// committed writes, a branch reads a stale ancestor checkpoint (`nextXid`) and
/// every ancestor tuple reads "in the future" → "does not exist".
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_cross_client_visibility() {
    use yb_thin_client::{CondOp, RangeRead, UpsertRow};
    const TABLE: &str = "xclient_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id, key) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    // Client A = the "ancestor's" writer (like main's pageserver).
    let a = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect A");
    let ta = a.open_table(db_oid, table_oid).expect("open A");
    let kbytes = Bytes::from_static(&[0x03, 0xab, 0xcd, 0x01]);
    // A writes 16 ascending versions (like the checkpoint's nextXid rewrites).
    for i in 1..=16i64 {
        let row = UpsertRow::new(
            &ta,
            vec![
                BindValue::Text("m".into()),
                BindValue::Bytes(kbytes.clone()),
                BindValue::I64(i * 100),
            ],
            &[("payload", BindValue::Bytes(Bytes::from(vec![i as u8; 4])))],
        )
        .unwrap();
        a.upsert(&[row]).await.expect("A upsert");
    }

    // Client B = a FRESH client (like the branch's newly-started pageserver),
    // reads the newest version — read_meta's exact pattern.
    let b = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect B");
    let tb = b.open_table(db_oid, table_oid).expect("open B");
    let read = RangeRead::new(
        &tb,
        vec![
            BindValue::Text("m".into()),
            BindValue::Bytes(kbytes.clone()),
        ],
    )
    .unwrap()
    .cond(&tb, "lsn", CondOp::Le, BindValue::I64(1_000_000))
    .unwrap()
    .targets(&tb, &["lsn"])
    .unwrap()
    .limit(1);
    let seen = b
        .read_page(&tb, &read, None)
        .await
        .expect("B read")
        .rows
        .first()
        .map(|r| r[0].as_i64().unwrap());
    println!(
        "repro cross-client: A wrote up to lsn=1600; fresh client B reads lsn={seen:?} (want 1600)"
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        seen,
        Some(1600),
        "fresh client B did NOT see client A's latest committed write (want 1600) — \
         cross-client staleness: a branch reads a stale ancestor checkpoint/nextXid"
    );
}

/// Repro for `PageStore::read_meta` fetching a STALE checkpoint (the
/// `branch_ancestor` root cause). `read_meta` mirrors the `amp_storage.meta`
/// table: PK `((branch_id, key) HASH, lsn DESC)`, a BYTEA key, and MANY lsn
/// versions of one key (the checkpoint's `nextXid` is rewritten on every
/// xid-advancing WAL record). It relies on a forward `limit(1)` returning the
/// row with the HIGHEST lsn (newest). If the shim's forward `limit(1)` on this
/// two-column-hash + BYTEA-key + many-version shape returns an OLDER row, a
/// branch reads a stale checkpoint → stale `nextXid` → ancestor tuples appear
/// "in the future" → every user table reads "does not exist".
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_meta_bytea_limit1_newest() {
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "meta_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // Exactly amp_storage.meta's shape.
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id, key) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    // 16 versions of ONE (branch, key) at ascending lsns — like the checkpoint
    // key rewritten as nextXid advances. Payload encodes the lsn so we can tell
    // which version came back.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT 'm', '\\x03abcd01'::bytea, g*100, \
             set_byte('\\x00000000'::bytea, 0, g) FROM generate_series(1, 16) g"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    // EXACTLY read_meta_thin's read: hash (branch_id, key), lsn <= huge, limit 1.
    let read = RangeRead::new(
        &table,
        vec![
            BindValue::Text("m".into()),
            BindValue::Bytes(Bytes::from_static(&[0x03, 0xab, 0xcd, 0x01])),
        ],
    )
    .unwrap()
    .cond(&table, "lsn", CondOp::Le, BindValue::I64(850))
    .unwrap()
    .targets(&table, &["lsn"])
    .unwrap()
    .limit(1);
    let page = client.read_page(&table, &read, None).await.expect("read");
    let seen = page.rows.first().map(|r| r[0].as_i64().unwrap());
    println!(
        "repro meta bytea limit1 (2-col hash + BYTEA key, cap 850): got lsn={seen:?} (want Some(800) newest<=cap; bug=Some(100) oldest)"
    );
    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
    assert_eq!(
        seen,
        Some(800),
        "read_meta pattern returned a STALE version (want newest lsn=800 at or \
         below the cap) — branch checkpoint/nextXid would be stale, breaking \
         ancestor visibility"
    );
}

/// Repro for the safekeeper WAL-flush stall: the safekeeper drives reads (the
/// WAL sender's `fetch_chunks`) and writes (the consumer's `flush_batch`)
/// CONCURRENTLY on ONE client. No other repro exercised concurrent read+write,
/// and the integration logs show the flush task silently frozen. If the shim
/// deadlocks under concurrent read+write on one client, this hangs.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_concurrent_read_write() {
    use std::sync::Arc;
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "crw_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, seg BIGINT NOT NULL, off BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, seg, off))"
        ),
        &[],
    )
    .await
    .unwrap();
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT 'c', 1, g, '\\x01'::bytea FROM generate_series(0,50) g"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    // One client, 1 read + 1 write session (matches the safekeeper).
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        read_sessions: 1,
        write_sessions: 1,
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");
    let client = client; // Arc<TsClient>
    let (rc, wc, rt, wt) = (
        Arc::clone(&client),
        Arc::clone(&client),
        Arc::clone(&table),
        Arc::clone(&table),
    );

    // Writer: repeated upserts (like flush_batch). Reader: repeated reads
    // (like the WAL sender). Run concurrently; a deadlock hangs the test.
    let writer = tokio::spawn(async move {
        for i in 0..200i64 {
            let row = UpsertRow::new(
                &wt,
                vec![
                    BindValue::Text("c".into()),
                    BindValue::I64(1),
                    BindValue::I64(1000 + i),
                ],
                &[("payload", BindValue::Bytes(Bytes::from(vec![0xCD; 4096])))],
            )
            .unwrap();
            wc.upsert(&[row]).await.expect("concurrent upsert");
        }
        200
    });
    let reader = tokio::spawn(async move {
        let read = RangeRead::new(&rt, vec![BindValue::Text("c".into())])
            .unwrap()
            .cond(&rt, "seg", CondOp::Eq, BindValue::I64(1))
            .unwrap()
            .targets(&rt, &["off", "payload"])
            .unwrap()
            .limit(100);
        let mut n = 0;
        for _ in 0..200 {
            let p = rc
                .read_page(&rt, &read, None)
                .await
                .expect("concurrent read");
            n += p.rows.len();
        }
        n
    });
    let (w, r) = tokio::join!(writer, reader);
    println!(
        "repro concurrent: writer did {} upserts, reader read {} rows",
        w.unwrap(),
        r.unwrap()
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

/// Repro for the safekeeper WAL fetch: the "covering chunk" arm is the only
/// caller that uses `.descending().limit(1)` — the highest chunk_offset below
/// a position — and `fetch_chunks` issues many such ops in ONE batch. Neither
/// is exercised elsewhere. Insert chunks at ascending offsets, then check the
/// descending limit-1 read returns the MAX offset below the cutoff (not the
/// min), both standalone and inside a multi-op batch.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_wals_covering_read() {
    use yb_thin_client::{CondOp, RangeRead, ReadOp};
    const TABLE: &str = "wals_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, bucket_id SMALLINT NOT NULL, segno BIGINT NOT NULL,
                 chunk_offset BIGINT NOT NULL, chunk_size INTEGER NOT NULL, data BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id, bucket_id) HASH, segno, chunk_offset))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Chunks for (branch 'b', bucket 0, segno 1) at offsets 0,100,200,300.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT 'b', 0, 1, g*100, 100, '\\x02'::bytea \
             FROM generate_series(0, 3) g"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    // Covering arm: highest chunk_offset < 250 → expect 200.
    let covering = RangeRead::new(&table, vec![BindValue::Text("b".into()), BindValue::I16(0)])
        .unwrap()
        .cond(&table, "segno", CondOp::Eq, BindValue::I64(1))
        .unwrap()
        .cond(&table, "chunk_offset", CondOp::Lt, BindValue::I64(250))
        .unwrap()
        .targets(&table, &["chunk_offset", "chunk_size"])
        .unwrap()
        .descending()
        .limit(1);
    // Forward arm: chunk_offset >= 250, ascending → expect 300.
    let forward = RangeRead::new(&table, vec![BindValue::Text("b".into()), BindValue::I16(0)])
        .unwrap()
        .cond(&table, "segno", CondOp::Eq, BindValue::I64(1))
        .unwrap()
        .cond(&table, "chunk_offset", CondOp::Ge, BindValue::I64(250))
        .unwrap()
        .targets(&table, &["chunk_offset", "chunk_size"])
        .unwrap()
        .limit(100);

    // Standalone covering read.
    let cov = client
        .read_page(&table, &covering, None)
        .await
        .expect("covering");
    let cov_off = cov.rows.first().map(|r| r[0].as_i64().unwrap());
    // Multi-op batch (covering + forward), like fetch_chunks.
    let batch = client
        .read(
            &[
                ReadOp {
                    table: &table,
                    read: &covering,
                    paging_state: None,
                },
                ReadOp {
                    table: &table,
                    read: &forward,
                    paging_state: None,
                },
            ],
            0,
        )
        .await
        .expect("batch");
    let b_cov = batch.ops[0].rows.first().map(|r| r[0].as_i64().unwrap());
    let b_fwd: Vec<i64> = batch.ops[1]
        .rows
        .iter()
        .map(|r| r[0].as_i64().unwrap())
        .collect();
    println!(
        "repro wals: standalone covering offset={cov_off:?} (want Some(200)); \
         batch covering={b_cov:?} (want Some(200)); batch forward={b_fwd:?} (want [300])"
    );
    assert_eq!(
        cov_off,
        Some(200),
        "covering read (descending limit 1) returned wrong offset"
    );
    assert_eq!(
        b_cov,
        Some(200),
        "covering arm in batch returned wrong offset"
    );
    assert_eq!(
        b_fwd,
        vec![300],
        "forward arm in batch returned wrong offsets"
    );

    // --- large-payload thin WRITE (mirrors flush_batch_thin) -------------
    // The safekeeper flushes a whole coalesced run as one row's `data` BYTEA;
    // a real batch is hundreds of KB, which no earlier repro exercised. Use a
    // dedicated 1-write-session client (like the safekeeper) and round-trip a
    // ~409 KB chunk through the thin client.
    let wclient = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        write_sessions: 1,
        read_sessions: 1,
        ..Default::default()
    })
    .expect("connect (1 write session)");
    let wtable = wclient.open_table(db_oid, table_oid).expect("open_table w");
    let big = vec![0xABu8; 409 * 1024];
    let row = UpsertRow::new(
        &wtable,
        vec![
            BindValue::Text("wbig".into()),
            BindValue::I16(1),
            BindValue::I64(7),
            BindValue::I64(0),
        ],
        &[
            ("chunk_size", BindValue::I32(big.len() as i32)),
            ("data", BindValue::Bytes(bytes::Bytes::from(big.clone()))),
        ],
    )
    .expect("build big wals upsert");
    wclient
        .upsert(&[row])
        .await
        .expect("thin upsert of large chunk");
    // Verify via YSQL that the whole payload landed.
    let yr = ysql
        .query(
            &format!("SELECT chunk_size, length(data) FROM {TABLE} WHERE branch_id='wbig' AND bucket_id=1 AND segno=7 AND chunk_offset=0"),
            &[],
        )
        .await
        .unwrap();
    println!(
        "repro wals: large write ({} bytes) → YSQL sees {} row(s): chunk_size={:?} data_len={:?}",
        big.len(),
        yr.len(),
        yr.first().map(|x| x.get::<_, i32>(0)),
        yr.first().map(|x| x.get::<_, i32>(1)),
    );
    assert_eq!(yr.len(), 1, "large thin write did not land");
    assert_eq!(
        yr[0].get::<_, i32>(1) as usize,
        big.len(),
        "large data payload truncated/corrupted"
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

/// Repro for the branch-read wedge: a MULTI-PAGE clustered scan over the real
/// `pages` schema. `read_scan` pages the range and pins pages 2+ to the first
/// page's `used_read_time_ht`; if the shim returns 0 for that, the snapshot
/// isn't pinned across pages and the scan can miss rows. `N` rows read with a
/// small `limit` forces several pages; we assert every row comes back.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_pages_multipage_scan() {
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "pages_repro";
    const N: i64 = 250; // blocks 0..N, one row each
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, spcnode BIGINT NOT NULL, dbnode BIGINT NOT NULL,
                 relnode BIGINT NOT NULL, forknum SMALLINT NOT NULL, blknum BIGINT NOT NULL,
                 is_image BOOLEAN NOT NULL, lsn BIGINT NOT NULL, will_init BOOLEAN NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id, spcnode, dbnode, relnode, forknum) HASH,
                              blknum, lsn DESC, is_image DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Insert N rows (branch 'b', identity 1/2/3/0), one version per block.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} \
             SELECT 'b', 1, 2, 3, 0, g, true, 100, true, '\\x01'::bytea \
             FROM generate_series(0, {}) g",
            N - 1
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;

    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    // A one-fork multi-block scan (blknum in [0, N)), lsn <= 100, small page
    // limit to force paging — mirrors PageStore::scan_one_fork.
    let read = RangeRead::new(
        &table,
        vec![
            BindValue::Text("b".into()),
            BindValue::I64(1),
            BindValue::I64(2),
            BindValue::I64(3),
            BindValue::I16(0),
        ],
    )
    .unwrap()
    .cond(&table, "blknum", CondOp::Ge, BindValue::I64(0))
    .unwrap()
    .cond(&table, "blknum", CondOp::Lt, BindValue::I64(N))
    .unwrap()
    .cond(&table, "lsn", CondOp::Le, BindValue::I64(100))
    .unwrap()
    .targets(
        &table,
        &["blknum", "lsn", "is_image", "will_init", "payload"],
    )
    .unwrap()
    .limit(32); // < N, so the scan pages

    let mut blks = std::collections::BTreeSet::new();
    client
        .read_scan(
            &table,
            &read,
            || (),
            |(), rows| -> anyhow::Result<bool> {
                for r in rows {
                    blks.insert(r[0].as_i64()?);
                }
                // `false` = keep paging: this scan collects the whole range, so
                // it must run to exhaustion. Returning `true` (stop) here after
                // the first page is exactly the bug that lost rows 33..250.
                Ok(false)
            },
        )
        .await
        .expect("read_scan");
    assert_eq!(
        blks.len() as i64,
        N,
        "multi-page scan lost rows: got {} of {N} blocks",
        blks.len()
    );
    println!("repro: multi-page scan OK — {} of {N} blocks", blks.len());

    // --- thin-client WRITE round-trip with distinct lsn/is_image ---------
    // The pages key has three range columns in clustering order
    // (blknum, lsn DESC, is_image DESC) — a different order than the table
    // definition (blknum, is_image, lsn). Write via the thin client with
    // distinct lsn (777) and is_image (false), read back, and cross-check via
    // YSQL, to catch any key-column misordering in the write path.
    let row = UpsertRow::new(
        &table,
        vec![
            BindValue::Text("w".into()),
            BindValue::I64(1),
            BindValue::I64(2),
            BindValue::I64(3),
            BindValue::I16(0),
            BindValue::I64(9),      // blknum
            BindValue::I64(777),    // lsn
            BindValue::Bool(false), // is_image
        ],
        &[
            ("will_init", BindValue::Bool(true)),
            (
                "payload",
                BindValue::Bytes(bytes::Bytes::from_static(b"pg")),
            ),
        ],
    )
    .expect("build pages upsert");
    client.upsert(&[row]).await.expect("thin upsert");

    let wread = RangeRead::new(
        &table,
        vec![
            BindValue::Text("w".into()),
            BindValue::I64(1),
            BindValue::I64(2),
            BindValue::I64(3),
            BindValue::I16(0),
        ],
    )
    .unwrap()
    .cond(&table, "blknum", CondOp::Eq, BindValue::I64(9))
    .unwrap()
    .cond(&table, "lsn", CondOp::Le, BindValue::I64(1000))
    .unwrap()
    .targets(&table, &["lsn", "is_image", "will_init", "payload"])
    .unwrap()
    .limit(32);
    let page = client
        .read_page(&table, &wread, None)
        .await
        .expect("read_page w");
    assert_eq!(page.rows.len(), 1, "thin write not readable back");
    let r = &page.rows[0];
    let (rlsn, ris_image, rwill_init, rpayload) = (
        r[0].as_i64().unwrap(),
        r[1].as_bool().unwrap(),
        r[2].as_bool().unwrap(),
        r[3].as_bytes().unwrap().to_vec(),
    );
    // YSQL cross-check: how did the row actually land on disk?
    let yrows = ysql
        .query(
            &format!(
                "SELECT lsn, is_image, will_init FROM {TABLE} WHERE branch_id='w' AND blknum=9"
            ),
            &[],
        )
        .await
        .unwrap();
    println!(
        "repro: thin write→read: lsn={rlsn} is_image={ris_image} will_init={rwill_init} payload={rpayload:?}; \
         YSQL sees {} row(s): lsn={:?} is_image={:?} will_init={:?}",
        yrows.len(),
        yrows.first().map(|x| x.get::<_, i64>(0)),
        yrows.first().map(|x| x.get::<_, bool>(1)),
        yrows.first().map(|x| x.get::<_, bool>(2)),
    );
    assert_eq!(
        rlsn, 777,
        "lsn wrong after thin write→read (key-order bug?)"
    );
    assert!(
        !ris_image,
        "is_image wrong after thin write→read (key-order bug?)"
    );
    assert!(rwill_init, "will_init wrong after thin write→read");
    assert_eq!(rpayload, b"pg", "payload wrong after thin write→read");

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();
}

/// Repro for the `branch_ancestor` integration wedge: read-after-write
/// VISIBILITY across sessions on one client, modelling the safekeeper
/// exactly. The consumer writes a WAL chunk on the WRITE session; the WAL
/// sender then reads the "covering chunk" (highest offset below a cutoff,
/// `.descending().limit(1)`) on the READ session to serve it downstream. If
/// the read (`read_time_ht = 0`, a shim-chosen *clamped* read point) picks a
/// snapshot behind the just-committed write, the sender serves stale WAL and
/// the ancestor pageserver lags the branch fork point — the exact failure the
/// integration test hits, with no hang and no error. This asserts each
/// write is immediately visible to a fresh cross-session read; if the clamp
/// makes reads trail writes, we measure and report the staleness window.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_read_after_write_visibility() {
    use std::time::Instant;
    use yb_thin_client::RangeRead;
    const TABLE: &str = "raw_repro";
    const N: i64 = 400;
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // A WAL-log-shaped table: latest write = highest lsn for the branch.
    ysql.execute(
        &format!(
            "CREATE TABLE {TABLE} (
                 branch_id TEXT NOT NULL, lsn BIGINT NOT NULL, payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC))"
        ),
        &[],
    )
    .await
    .unwrap();
    let (db_oid, table_oid) = table_oids(&ysql, TABLE).await;
    // One client, 1 read + 1 write session — the safekeeper's exact config.
    let client = TsClient::connect(&TsClientConfig {
        tserver_addrs: tserver_addrs(),
        read_sessions: 1,
        write_sessions: 1,
        ..Default::default()
    })
    .expect("connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    // Read a specific lsn by EQUALITY on the read session — unambiguous
    // visibility: 1 row => the write is visible, 0 rows => not yet.
    macro_rules! visible {
        ($c:expr, $t:expr, $lsn:expr) => {{
            let r = RangeRead::new($t, vec![BindValue::Text("raw".into())])
                .unwrap()
                .cond($t, "lsn", yb_thin_client::CondOp::Eq, BindValue::I64($lsn))
                .unwrap()
                .targets($t, &["lsn"])
                .unwrap()
                .limit(1);
            let p = $c.read_page($t, &r, None).await.expect("read");
            (p.rows.len() == 1, p.used_read_time_ht)
        }};
    }

    let mut stale = 0usize;
    let mut max_lag = std::time::Duration::ZERO;
    let mut worst = -1i64;
    for i in 0..N {
        let row = UpsertRow::new(
            &table,
            vec![BindValue::Text("raw".into()), BindValue::I64(i)],
            &[("payload", BindValue::Bytes(Bytes::from(vec![0xEE; 256])))],
        )
        .unwrap();
        client.upsert(&[row]).await.expect("upsert");

        // Fresh cross-session read of the just-committed key. Must be visible
        // immediately; if it trails, spin (bounded) and time the window.
        let started = Instant::now();
        let (mut vis, _) = visible!(&client, &table, i);
        if !vis {
            loop {
                if started.elapsed() > std::time::Duration::from_secs(5) {
                    // Diagnose: is this session-pinned, or globally invisible?
                    let (same_i, ht_i) = visible!(&client, &table, i);
                    let (same_0, _) = visible!(&client, &table, 0);
                    let fresh = TsClient::connect(&TsClientConfig {
                        tserver_addrs: tserver_addrs(),
                        read_sessions: 1,
                        write_sessions: 1,
                        ..Default::default()
                    })
                    .expect("fresh connect");
                    let ftable = fresh.open_table(db_oid, table_oid).expect("fresh open");
                    let (fresh_i, ht_f) = visible!(&fresh, &ftable, i);
                    panic!(
                        "wrote lsn={i}, not seen after 5s.\n  \
                         same session: sees lsn={i}? {same_i} (used_read_time_ht={ht_i}); sees lsn=0? {same_0}\n  \
                         FRESH client: sees lsn={i}? {fresh_i} (used_read_time_ht={ht_f})\n  \
                         (YSQL has lsn=0..={i} committed)"
                    );
                }
                tokio::task::yield_now().await;
                let (v, _) = visible!(&client, &table, i);
                if v {
                    vis = true;
                    break;
                }
            }
            let lag = started.elapsed();
            stale += 1;
            if lag > max_lag {
                max_lag = lag;
                worst = i;
            }
        }
        let _ = vis;
        if i < 4 {
            println!("wrote lsn={i} → visible after {:?}", started.elapsed());
        }
    }
    println!(
        "repro RAW visibility: {N} write→read cycles on one client (1r+1w session), \
         {stale} reads did NOT immediately see the committed write; worst lag {max_lag:?} at lsn={worst}"
    );

    ysql.execute(&format!("DROP TABLE {TABLE}"), &[])
        .await
        .unwrap();

    assert_eq!(
        stale, 0,
        "{stale}/{N} cross-session reads did not immediately see the committed write \
         (worst lag {max_lag:?} at lsn={worst}) — read-after-write staleness in the thin lane"
    );
}
