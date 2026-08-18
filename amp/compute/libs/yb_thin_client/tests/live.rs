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
//!
//! Fixture DDL is DROP IF EXISTS + CREATE IF NOT EXISTS: `execute` replays
//! on transient errors, and a bare CREATE returns `42P07` — #508.

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
        // A live test against a cluster that never answers should hang
        // visibly rather than fail as a timeout that reads like a bug in
        // the code under test.
        connect_timeout_ms: 0,
        statement_timeout_ms: 0,
        tcp_user_timeout_ms: 0,
        // A live test's connection outlives nothing; recycling it mid-test
        // would only add a reconnect to the thing under test.
        connection_lifetime_ms: 0,
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
}

/// A `uuid` key column binds as bytea, and the 16 raw bytes are the value.
///
/// This is what lets `amp_storage`'s `branch_id` be `uuid` while the thin lane
/// still writes it: the shim reports a column's bind type from its DocDB type,
/// and DocDB types `uuid` exactly as `bytea` (binary). Nothing in the pinned ABI
/// names uuid, so this test is the contract — a shim that started reporting
/// something else, or encoding the bytes differently, would leave every
/// thin-lane page write under a branch id nobody can read back.
///
/// The YSQL cross-check is the half that matters: the row the thin lane wrote
/// carries the uuid the control plane knows, not 16 bytes of garbage.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn uuid_key_column_binds_as_bytea() {
    const TABLE: &str = "thin_uuid_key";
    // Deliberately contains a 0x00 byte: the DocDB key encoding zero-escapes
    // it, so a round trip that mangles escaping fails here.
    const ID_BYTES: [u8; 16] = [
        0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee,
        0xff,
    ];
    const ID_TEXT: &str = "00112233-4455-6677-8899-aabbccddeeff";

    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id uuid NOT NULL,
                 lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, lsn DESC))"
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
    .expect("thin-client connect");
    let table = client.open_table(db_oid, table_oid).expect("open_table");

    let col = table
        .columns
        .iter()
        .find(|c| c.name == "branch_id")
        .expect("branch_id column");
    assert_eq!(
        col.value_type,
        yb_thin_client::ValueType::Bytea,
        "a uuid column must report as bytea — bytes are the correct bind"
    );

    let id = BindValue::Bytes(Bytes::from_static(&ID_BYTES));
    client
        .upsert(&[UpsertRow::new(
            &table,
            vec![id.clone(), BindValue::I64(7)],
            &[("payload", BindValue::Bytes(Bytes::from_static(b"uuid-key")))],
        )
        .expect("build upsert row")])
        .await
        .expect("upsert");

    // The same bytes address the row for a read.
    let read = RangeRead::new(&table, vec![id])
        .unwrap()
        .targets(&table, &["lsn", "payload"])
        .unwrap()
        .limit(10);
    let batch = client
        .read(
            &[ReadOp {
                table: &table,
                read: &read,
                paging_state: None,
            }],
            0,
        )
        .await
        .expect("read");
    let rows = &batch.ops[0].rows;
    assert_eq!(rows.len(), 1, "expected the one row we wrote");
    assert_eq!(rows[0][0].as_i64().unwrap(), 7, "lsn");
    assert_eq!(
        rows[0][1].as_bytes().unwrap().as_ref(),
        b"uuid-key",
        "payload"
    );

    // ... and YSQL sees that row under the uuid those bytes spell.
    let text = ysql
        .query(
            &format!("SELECT branch_id::text FROM {TABLE} WHERE lsn = 7"),
            &[],
        )
        .await
        .expect("ysql read-back");
    assert_eq!(text.len(), 1, "expected the one row we wrote");
    assert_eq!(
        text[0].get::<_, String>(0),
        ID_TEXT,
        "the thin lane's bytes must be the uuid the control plane knows"
    );

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
    // Distills amp_storage.meta's read shape: hash key + trailing `lsn DESC`
    // (the real table has a `key` range column in between; see
    // `repro_meta_bytea_limit1_newest` for the exact shape).
    ysql.execute(
        &format!(
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL, tag BIGINT NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC))"
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
                let read = RangeRead::new(&t, vec![BindValue::Text("m".into())])
                    .unwrap()
                    .cond(&t, "key", CondOp::Eq, BindValue::Bytes(Bytes::from(key)))
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
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC))"
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
    let read = RangeRead::new(&table, vec![BindValue::Text("m".into())])
        .unwrap()
        .cond(&table, "key", CondOp::Eq, BindValue::Bytes(Bytes::from(ck)))
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
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC))"
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
    let read = RangeRead::new(&tb, vec![BindValue::Text("m".into())])
        .unwrap()
        .cond(&tb, "key", CondOp::Eq, BindValue::Bytes(kbytes.clone()))
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
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
/// table: PK `((branch_id) HASH, key ASC, lsn DESC)`, a BYTEA key bound as an
/// equality cond, and MANY lsn versions of one key (the checkpoint's
/// `nextXid` is rewritten on every xid-advancing WAL record). It relies on a
/// forward `limit(1)` returning the row with the HIGHEST lsn (newest). If the
/// shim's forward `limit(1)` on this BYTEA-range-key + many-version shape
/// returns an OLDER row, a branch reads a stale checkpoint → stale `nextXid`
/// → ancestor tuples appear "in the future" → every user table reads "does
/// not exist".
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id TEXT NOT NULL, key BYTEA NOT NULL, lsn BIGINT NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY ((branch_id) HASH, key ASC, lsn DESC))"
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

    // EXACTLY read_meta_thin's read: hash (branch_id), key = Eq cond,
    // lsn <= cap, limit 1.
    let read = RangeRead::new(&table, vec![BindValue::Text("m".into())])
        .unwrap()
        .cond(
            &table,
            "key",
            CondOp::Eq,
            BindValue::Bytes(Bytes::from_static(&[0x03, 0xab, 0xcd, 0x01])),
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
        "repro meta bytea limit1 (branch hash + BYTEA range key, cap 850): got lsn={seen:?} (want Some(800) newest<=cap; bug=Some(100) oldest)"
    );
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
}

/// The "covering chunk" arm is the only caller that uses
/// `.descending().limit(1)` — the highest chunk_offset below a position — and
/// `fetch_chunks` issues many such ops in ONE batch. Insert chunks at ascending
/// offsets, then check the descending limit-1 read returns the MAX offset below
/// the cutoff (not the min), both standalone and inside a multi-op batch.
///
/// Kept HASH-sharded deliberately, even though production `wals` is now
/// all-range: this is the suite's coverage of descending limit-1 on a
/// hash-keyed table — the shape `slru_pages` and `meta` reads still use
/// (a `branch_id` hash prefix plus range-column conds). The range-sharded
/// shape of the real `wals`, including the covering arm across tablet
/// boundaries, is covered by `repro_wals_range_sharded`.
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
}

/// The `pages` bucket function, mirroring `page_store::page_bucket_id`.
/// Duplicated rather than imported (this crate must not depend on pageserver
/// internals); if the two ever drift, this test fails loudly rather than
/// silently reading a subset.
fn pages_bucket_of(relnode: i64, blknum: i64) -> i16 {
    ((relnode / 7 + relnode / 13 + blknum / 256) % 6) as i16
}

/// Repro for the branch-read wedge: a MULTI-PAGE clustered scan over the real
/// `pages` schema. `read_scan` pages the range and pins pages 2+ to the first
/// page's `used_read_time_ht`; if the shim returns 0 for that, the snapshot
/// isn't pinned across pages and the scan can miss rows. `N` rows read with a
/// small `limit` forces several pages; we assert every row comes back.
///
/// The schema is the post-bucketization one: all-range key led by `bucket_id`,
/// pre-split into 6 tablets. `N` deliberately spans several 2MB stretches, so
/// the fork's blocks live in several buckets on DIFFERENT tablets and a
/// correct scan is one bucket-pinned read each — mirroring how
/// `PageStore::scan_one_fork` now fans out over `page_buckets_for_range`.
/// A single unpinned read would leave the leading key column unbound.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_pages_multipage_scan() {
    use yb_thin_client::{CondOp, RangeRead};
    const TABLE: &str = "pages_repro";
    const N: i64 = 1000; // blocks 0..N — 4 stretches of 256, so 4 buckets
    const REL: i64 = 3;
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    ysql.execute(
        &format!(
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 bucket_id SMALLINT NOT NULL,
                 branch_id TEXT NOT NULL, spcnode BIGINT NOT NULL, dbnode BIGINT NOT NULL,
                 relnode BIGINT NOT NULL, forknum SMALLINT NOT NULL, blknum BIGINT NOT NULL,
                 is_image BOOLEAN NOT NULL, lsn BIGINT NOT NULL, will_init BOOLEAN NOT NULL,
                 payload BYTEA NOT NULL,
                 PRIMARY KEY (bucket_id ASC, branch_id ASC, spcnode ASC, dbnode ASC,
                              relnode ASC, forknum ASC, blknum ASC,
                              lsn DESC, is_image DESC))
             SPLIT AT VALUES ((1), (2), (3), (4), (5))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Insert N rows (branch 'b', identity 1/2/REL/0), one version per block.
    // bucket_id is computed in SQL with the same integer-division expression as
    // the migration's CHECK, which is what makes the Rust helper above a real
    // cross-check rather than a restatement.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} \
             SELECT ((({REL} / 7) + ({REL} / 13) + (g / 256)) % 6)::smallint, \
                    'b', 1, 2, {REL}, 0, g, true, 100, true, '\\x01'::bytea \
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

    assert_eq!(
        table.num_hash_columns(),
        0,
        "bucketized pages must be range-sharded"
    );

    // The buckets [0, N) actually touches, derived rather than hardcoded.
    let want_buckets: std::collections::BTreeSet<i16> =
        (0..N).map(|b| pages_bucket_of(REL, b)).collect();
    assert!(
        want_buckets.len() > 1,
        "N={N} must span >1 bucket or this test would not exercise fan-out, got {want_buckets:?}"
    );

    // A one-fork multi-block scan (blknum in [0, N)), lsn <= 100, small page
    // limit to force paging — mirrors PageStore::scan_one_fork: the range key
    // prefix binds (bucket, branch, spc, db, rel, fork) and blknum stays a
    // cond, one read per touched bucket.
    let mut blks = std::collections::BTreeSet::new();
    for bucket in &want_buckets {
        let read = RangeRead::new_range(
            &table,
            vec![
                BindValue::I16(*bucket),
                BindValue::Text("b".into()),
                BindValue::I64(1),
                BindValue::I64(2),
                BindValue::I64(REL),
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
        .limit(32); // < the per-bucket row count, so the scan pages

        client
            .read_scan(
                &table,
                &read,
                || (),
                |(), rows| -> anyhow::Result<bool> {
                    for r in rows {
                        let blk = r[0].as_i64()?;
                        // Every row a bucket-pinned read returns must belong to
                        // that bucket, or the leading bound was not applied.
                        assert_eq!(
                            pages_bucket_of(REL, blk),
                            *bucket,
                            "bucket {bucket} read returned blknum {blk} from another bucket"
                        );
                        assert!(blks.insert(blk), "blknum {blk} returned twice");
                    }
                    // `false` = keep paging: this scan collects the whole range,
                    // so it must run to exhaustion. Returning `true` (stop) here
                    // after the first page is exactly the bug that lost rows.
                    Ok(false)
                },
            )
            .await
            .expect("read_scan");
    }
    assert_eq!(
        blks.len() as i64,
        N,
        "multi-page scan lost rows: got {} of {N} blocks across {} bucket(s)",
        blks.len(),
        want_buckets.len()
    );
    println!(
        "repro: multi-page scan OK — {} of {N} blocks across buckets {:?}",
        blks.len(),
        want_buckets
    );

    // --- thin-client WRITE round-trip with distinct lsn/is_image ---------
    // `key_values` go in PRIMARY KEY order, which is NOT the declaration
    // order: `bucket_id` leads, and (lsn DESC, is_image DESC) cluster in the
    // opposite order to how the table declares (is_image, lsn). Write via the
    // thin client with distinct lsn (777) and is_image (false), read back, and
    // cross-check via YSQL, to catch any key-column misordering in the write
    // path — including a bucket bound to the wrong column, which would land
    // the row where no reader looks.
    let w_blk: i64 = 9;
    let w_bucket = pages_bucket_of(REL, w_blk);
    let row = UpsertRow::new(
        &table,
        vec![
            BindValue::I16(w_bucket),
            BindValue::Text("w".into()),
            BindValue::I64(1),
            BindValue::I64(2),
            BindValue::I64(REL),
            BindValue::I16(0),
            BindValue::I64(w_blk),  // blknum
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

    // Mirrors PageStore::read_chain_thin: a known blknum determines the bucket,
    // so the whole 7-column prefix binds and this is a single-tablet read with
    // `blknum` in the prefix rather than a trailing cond.
    let wread = RangeRead::new_range(
        &table,
        vec![
            BindValue::I16(w_bucket),
            BindValue::Text("w".into()),
            BindValue::I64(1),
            BindValue::I64(2),
            BindValue::I64(REL),
            BindValue::I16(0),
            BindValue::I64(w_blk),
        ],
    )
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
                // Deliberately does NOT bind bucket_id: an unpinned scan finds
                // the row wherever it landed, so this catches a thin-client
                // write that stamped the wrong bucket (which the pinned read
                // above would miss by looking in the same wrong place).
                "SELECT lsn, is_image, will_init FROM {TABLE} \
                 WHERE branch_id='w' AND blknum={w_blk}"
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

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
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
            "CREATE TABLE IF NOT EXISTS {TABLE} (
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

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();

    assert_eq!(
        stale, 0,
        "{stale}/{N} cross-session reads did not immediately see the committed write \
         (worst lag {max_lag:?} at lsn={worst}) — read-after-write staleness in the thin lane"
    );
}

/// Repro for a RANGE-sharded `wals`: the table is about to move from
/// `PRIMARY KEY ((branch_id, bucket_id) HASH, …)` to an all-range PK split
/// with `SPLIT AT VALUES`, and nothing in this suite has ever sent the shim a
/// read spec with `n_hash = 0`. Every other test binds a full hash prefix, so
/// every other read is single-tablet by construction and the key range is
/// pinned by the hash key rather than by the conds.
///
/// Range sharding changes four things at once, all of them untested:
///   1. `n_hash = 0` in `ybthin_read_spec` — a code path with no coverage.
///   2. Filtering is entirely by cond. A read that ignored the `branch_id`
///      bound would silently return other branches' rows instead of erroring,
///      so the fixture brackets branch `'b'` with branches `'a'` and `'c'`
///      that must never appear.
///   3. One scan can cross tablet boundaries. `repro_pages_multipage_scan`
///      pages within a single tablet; paging across a split point is new.
///   4. A reverse scan must start from the LAST tablet in range. If it starts
///      at the first, `descending().limit(1)` — the safekeeper's covering-chunk
///      arm — returns the oldest chunk instead of the newest.
///
/// Split points put segno 1 in tablet 1, segno 2..3 in tablet 2, segno 4..5 in
/// tablet 3, so the assertions below straddle boundaries rather than sitting
/// inside one tablet.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_wals_range_sharded() {
    use yb_thin_client::{ColKind, CondOp, YbError};
    const TABLE: &str = "wals_range_repro";
    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // The wals schema with an all-range PK. Split on the (branch_id, bucket_id,
    // segno) prefix — a split point shorter than the full key is legal, the
    // remaining columns read as -Inf.
    ysql.execute(
        &format!(
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 branch_id TEXT NOT NULL, bucket_id SMALLINT NOT NULL, segno BIGINT NOT NULL,
                 chunk_offset BIGINT NOT NULL, chunk_size INTEGER NOT NULL, data BYTEA NOT NULL,
                 PRIMARY KEY (branch_id ASC, bucket_id ASC, segno ASC, chunk_offset ASC))
             SPLIT AT VALUES (('b', 0, 2), ('b', 0, 4))"
        ),
        &[],
    )
    .await
    .unwrap();
    // Branch 'b': segno 1..5 × chunk_offset 0,100,200,300 = 20 rows, spread
    // over all three tablets.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT 'b', 0, s, o*100, 100, '\\x02'::bytea \
             FROM generate_series(1, 5) s, generate_series(0, 3) o"
        ),
        &[],
    )
    .await
    .unwrap();
    // Bracket rows in the first and last tablet ('a' < 'b' < 'c'). A read that
    // drops the branch_id cond, or a reverse scan that starts past the branch's
    // upper bound, picks these up.
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} VALUES \
             ('a', 0, 9, 0, 100, '\\x0a'::bytea), ('c', 0, 9, 0, 100, '\\x0c'::bytea)"
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

    // The shim must report every key column as RANGE and none as HASH —
    // `RangeRead::new` sizes its hash prefix off this, and `cond` refuses any
    // column it doesn't see as ColKind::Range.
    let n_hash = table.num_hash_columns();
    let kinds: Vec<(&str, ColKind)> = table
        .columns
        .iter()
        .map(|c| (c.name.as_str(), c.kind))
        .collect();
    println!("repro wals range: num_hash_columns={n_hash} (want 0); schema kinds={kinds:?}");
    assert_eq!(
        n_hash, 0,
        "range-sharded table reported hash columns; open_table schema is wrong: {kinds:?}"
    );
    assert_eq!(
        table.num_key_columns(),
        4,
        "expected 4 range key columns: {kinds:?}"
    );
    for want in ["branch_id", "bucket_id", "segno", "chunk_offset"] {
        assert_eq!(
            table.col(want).unwrap().kind,
            ColKind::Range,
            "key column {want} is not ColKind::Range: {kinds:?}"
        );
    }

    // Every read below binds a range key PREFIX (branch_id, bucket_id) rather
    // than a hash prefix — the range-sharded analogue, and the thing the
    // 2026.1.1.0-b0 shim added `range_values` for. Columns past the prefix
    // (segno, chunk_offset) are still bounded with cond().
    let branch_b = || {
        RangeRead::new_range(&table, vec![BindValue::Text("b".into()), BindValue::I16(0)]).unwrap()
    };

    // Single-page reads first: they are the cheapest signal, and a failure
    // here reports a wrong row rather than making the paged scan below chase
    // a broken cursor.
    //
    // (1) Reverse scan over the whole branch: must start in the LAST tablet in
    // range and stop at the branch's upper bound, not run on into branch 'c'.
    let newest = branch_b()
        .targets(&table, &["segno", "chunk_offset"])
        .unwrap()
        .descending()
        .limit(1);
    let newest = client
        .read_page(&table, &newest, None)
        .await
        .expect("descending newest");
    let newest_key = newest
        .rows
        .first()
        .map(|r| (r[0].as_i64().unwrap(), r[1].as_i64().unwrap()));

    // (2) Reverse scan whose upper bound sits exactly on the tablet-2/3 split
    // point ('b', 0, 4): the newest key strictly below it is the last row of
    // tablet 2.
    //
    // This one MUST be paged, not read as a single page. The scan opens in
    // tablet 3 (segno 4, 5), where nothing satisfies `segno < 4`, so the first
    // page comes back EMPTY with a continuation — an empty page is "this
    // tablet had no match", not "the scan is done". Reading only the first
    // page here reports no covering row and looks exactly like a shim bug.
    // `read_scan` pages until a row appears; `limit(1)` still caps each page,
    // and returning `true` from the callback stops at the first hit.
    let below_split_read = branch_b()
        .cond(&table, "segno", CondOp::Lt, BindValue::I64(4))
        .unwrap()
        .targets(&table, &["segno", "chunk_offset"])
        .unwrap()
        .descending()
        .limit(1);
    let below_split_key: Option<(i64, i64)> = client
        .read_scan(
            &table,
            &below_split_read,
            || None,
            |acc: &mut Option<(i64, i64)>, rows| {
                if let Some(r) = rows.first() {
                    *acc = Some((r[0].as_i64().unwrap(), r[1].as_i64().unwrap()));
                }
                Ok::<bool, YbError>(acc.is_some())
            },
        )
        .await
        .expect("descending below split");

    // (3) The safekeeper's covering-chunk arm verbatim — highest chunk_offset
    // below a cutoff within one segno — now with no hash prefix to pin it.
    let covering = branch_b()
        .cond(&table, "segno", CondOp::Eq, BindValue::I64(3))
        .unwrap()
        .cond(&table, "chunk_offset", CondOp::Lt, BindValue::I64(250))
        .unwrap()
        .targets(&table, &["segno", "chunk_offset"])
        .unwrap()
        .descending()
        .limit(1);
    let covering = client
        .read_page(&table, &covering, None)
        .await
        .expect("covering");
    let covering_key = covering
        .rows
        .first()
        .map(|r| (r[0].as_i64().unwrap(), r[1].as_i64().unwrap()));

    println!(
        "repro wals range: newest={newest_key:?} (want Some((5, 300))); \
         newest below split point ('b',0,4)={below_split_key:?} (want Some((3, 300))); \
         covering segno=3 offset<250 => {covering_key:?} (want Some((3, 200)))"
    );
    // Asserted after the paged scan below, so one run reports every broken
    // read rather than stopping at the first.

    // (4) Multi-tablet forward scan, paged small so it must cross both splits.
    // `read_scan` pins pages 2+ to the first page's snapshot; if a continuation
    // can't carry across a tablet boundary this loses rows — or, if the shim
    // hands back a token that never advances, loops forever. Cap the page count
    // and stop the scan at the cap so that case fails with a diagnostic instead
    // of hanging the suite; 20 rows at 3 per page needs 7 pages.
    const MAX_PAGES: usize = 32;
    let scan = branch_b()
        .targets(&table, &["segno", "chunk_offset"])
        .unwrap()
        .limit(3);
    let mut pages = 0usize;
    let scanned: Vec<(i64, i64)> = client
        .read_scan(
            &table,
            &scan,
            Vec::new,
            |acc: &mut Vec<(i64, i64)>, rows| {
                pages += 1;
                for r in rows {
                    acc.push((r[0].as_i64().unwrap(), r[1].as_i64().unwrap()));
                }
                Ok::<bool, YbError>(pages >= MAX_PAGES)
            },
        )
        .await
        .expect("multi-tablet scan");
    let want: Vec<(i64, i64)> = (1..=5)
        .flat_map(|s| (0..4).map(move |o| (s, o * 100)))
        .collect();
    let stray: Vec<&(i64, i64)> = scanned.iter().filter(|(s, _)| *s == 9).collect();
    println!(
        "repro wals range: paged scan took {pages} page(s) (limit 3/page), returned {} rows \
         (want {}), strays from branch a/c: {stray:?}",
        scanned.len(),
        want.len()
    );
    assert!(
        pages < MAX_PAGES,
        "paged scan hit the {MAX_PAGES}-page cap after {} rows — the continuation token is \
         not advancing across tablet boundaries (an uncapped scan here spins forever)",
        scanned.len()
    );
    assert!(
        stray.is_empty(),
        "read with no hash prefix returned rows outside branch 'b' — the branch_id \
         cond is not bounding the scan: {stray:?}"
    );
    assert_eq!(
        scanned, want,
        "paged scan across 3 tablets lost or reordered rows"
    );
    assert_eq!(
        newest_key,
        Some((5, 300)),
        "descending limit-1 did not start from the last tablet in range"
    );
    assert_eq!(
        below_split_key,
        Some((3, 300)),
        "descending limit-1 with the bound on a tablet split point returned the wrong row"
    );
    assert_eq!(
        covering_key,
        Some((3, 200)),
        "covering read (descending limit 1) returned wrong offset"
    );

    // (5) Writes with no hash prefix, one row per tablet in ONE Perform — the
    // shim has to route each row to a different tablet off its range key alone.
    let rows: Vec<UpsertRow> = [1i64, 3, 5]
        .iter()
        .map(|&segno| {
            UpsertRow::new(
                &table,
                vec![
                    BindValue::Text("b".into()),
                    BindValue::I16(0),
                    BindValue::I64(segno),
                    BindValue::I64(400),
                ],
                &[
                    ("chunk_size", BindValue::I32(64)),
                    ("data", BindValue::Bytes(Bytes::from_static(b"range-write"))),
                ],
            )
            .expect("build upsert row")
        })
        .collect();
    client.upsert(&rows).await.expect("cross-tablet upsert");
    let written = ysql
        .query(
            &format!(
                "SELECT segno, chunk_size, encode(data, 'escape') FROM {TABLE} \
                 WHERE branch_id = 'b' AND bucket_id = 0 AND chunk_offset = 400 ORDER BY segno"
            ),
            &[],
        )
        .await
        .unwrap();
    let written: Vec<(i64, i32, String)> = written
        .iter()
        .map(|r| {
            (
                r.get::<_, i64>(0),
                r.get::<_, i32>(1),
                r.get::<_, String>(2),
            )
        })
        .collect();
    println!("repro wals range: cross-tablet upsert landed {written:?} (want segno 1, 3, 5)");
    assert_eq!(
        written,
        vec![
            (1, 64, "range-write".to_string()),
            (3, 64, "range-write".to_string()),
            (5, 64, "range-write".to_string()),
        ],
        "one-Perform upsert did not land one row in each of the three tablets"
    );

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
}

/// Repro for #739: on a RANGE-sharded table, `range_values` sets only where a
/// scan STARTS. Nothing sets where it ENDS, so every read runs from its start
/// key to the physical end of the table — visiting every remaining tablet and
/// returning an empty page plus a continuation for each.
///
/// The safekeeper's covering arm is where that becomes a wedge: it binds the
/// `(bucket_id, branch_id, segno)` prefix, filters `chunk_offset <
/// byte_offset`, and scans DESCENDING for one row. On `amp_storage.wals` —
/// pre-split `((1), (2), (3), (4), (5))` — the bucket-5 arm therefore walks
/// down through every tablet of every lower bucket. Buckets whose arm finds
/// its row stop on page 1; the buckets whose arm matches nothing pay the full
/// walk, and bucket 5 pays the longest one. On a production-sized `wals` that
/// walk exceeds the 64-barren-round guard, the fetch fails, and WAL replay
/// stops for good.
///
/// The table below is the production `wals` shape with one chunk per bucket,
/// all at the same offset. Every arm's key range lives inside ONE tablet, so
/// every arm must settle in ONE page. What actually happens is `bucket + 1`
/// pages — the walk, measured. The `descending` covering arm and the full-key
/// point read are both here because they fail identically: a point read that
/// has already returned its row on page 1 still walks the rest of the table,
/// which is what shows this is about the missing end bound rather than about
/// empty result sets, reverse scans, or the open-ended last tablet.
#[tokio::test]
#[ignore = "requires a live YugabyteDB + the real libyb_thin_client.so on LD_LIBRARY_PATH"]
async fn repro_range_scan_walks_past_its_key_range() {
    use yb_thin_client::CondOp;
    const TABLE: &str = "wals_scan_walk_repro";
    const SEGNO: i64 = 4410;
    /// The one chunk each bucket holds, ABOVE the fetch position — so the
    /// covering arm's `chunk_offset < POS` matches nothing, in any bucket.
    const CHUNK_OFFSET: i64 = 7414080;
    const POS: i64 = 7337520;
    /// Enough to distinguish "walks the table" from "settles"; the real fetch
    /// gives up at 64 barren rounds.
    const MAX_PAGES: usize = 128;
    const BUCKETS: i16 = 6;

    let ysql = ysql().await;
    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
    // The production `wals` shape and split: bucket-leading all-range PK, one
    // tablet per bucket.
    ysql.execute(
        &format!(
            "CREATE TABLE IF NOT EXISTS {TABLE} (
                 bucket_id SMALLINT NOT NULL, branch_id TEXT NOT NULL, segno BIGINT NOT NULL,
                 chunk_offset BIGINT NOT NULL, chunk_size INTEGER NOT NULL, data BYTEA NOT NULL,
                 PRIMARY KEY (bucket_id ASC, branch_id ASC, segno ASC, chunk_offset ASC))
             SPLIT AT VALUES ((1), (2), (3), (4), (5))"
        ),
        &[],
    )
    .await
    .unwrap();
    ysql.execute(
        &format!(
            "INSERT INTO {TABLE} SELECT b::smallint, 'b', {SEGNO}, {CHUNK_OFFSET}, 100, \
             '\\x02'::bytea FROM generate_series(0, {}) b",
            BUCKETS - 1
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

    // Page a read by hand rather than through `read_scan`, so a scan that
    // refuses to end is a reported page count instead of a hang.
    let page_count = |read: RangeRead| {
        let client = client.clone();
        let table = table.clone();
        async move {
            let mut paging_state: Option<Bytes> = None;
            let (mut pages, mut rows) = (0usize, 0usize);
            loop {
                if pages >= MAX_PAGES {
                    return (pages, rows, false);
                }
                let page = client
                    .read_page(&table, &read, paging_state.as_deref())
                    .await
                    .expect("read page");
                pages += 1;
                rows += page.rows.len();
                match page.paging_state {
                    Some(ps) => paging_state = Some(ps),
                    None => return (pages, rows, true),
                }
            }
        }
    };

    // (1) The safekeeper's covering arm, once per bucket. Same bound, same
    //     empty result everywhere — the bucket is the only variable.
    let mut covering: Vec<(i16, usize)> = Vec::new();
    for bucket in 0..BUCKETS {
        let read = RangeRead::new_range(
            &table,
            vec![
                BindValue::I16(bucket),
                BindValue::Text("b".into()),
                BindValue::I64(SEGNO),
            ],
        )
        .unwrap()
        .cond(&table, "chunk_offset", CondOp::Lt, BindValue::I64(POS))
        .unwrap()
        .targets(&table, &["chunk_offset", "chunk_size", "data"])
        .unwrap()
        .descending()
        .limit(1);
        let (pages, rows, exhausted) = page_count(read).await;
        assert_eq!(
            rows, 0,
            "bucket {bucket}: the only chunk is at {CHUNK_OFFSET}, which does not satisfy \
             chunk_offset < {POS} — the fixture is wrong, not the scan"
        );
        assert!(
            exhausted,
            "bucket {bucket}: covering arm still continuing after {MAX_PAGES} pages"
        );
        covering.push((bucket, pages));
    }

    // (2) The same walk with no empty result set to blame: a full-key POINT
    //     read, which returns its row on page 1 and has nothing left to find.
    //     Run it in both directions, each from the end of the table it scans
    //     AWAY from, so each one has the whole table ahead of it: descending
    //     from the last bucket, ascending from the first.
    let point = |bucket: i16, descending: bool| {
        let mut r = RangeRead::new_range(
            &table,
            vec![
                BindValue::I16(bucket),
                BindValue::Text("b".into()),
                BindValue::I64(SEGNO),
                BindValue::I64(CHUNK_OFFSET),
            ],
        )
        .unwrap()
        .targets(&table, &["chunk_offset"])
        .unwrap()
        .limit(1);
        if descending {
            r = r.descending();
        }
        r
    };
    let mut points: Vec<(&str, usize)> = Vec::new();
    for (label, bucket, descending) in [
        ("descending from the last bucket", BUCKETS - 1, true),
        ("ascending from the first bucket", 0, false),
    ] {
        let (pages, rows, exhausted) = page_count(point(bucket, descending)).await;
        assert_eq!(
            rows, 1,
            "{label}: the full-key point read must find its row"
        );
        assert!(
            exhausted,
            "{label}: point read still continuing after {MAX_PAGES} pages"
        );
        points.push((label, pages));
    }

    println!(
        "repro #739: covering arm (bucket, pages) = {covering:?}; \
         full-key point reads (direction, pages) = {points:?}. Want 1 page each — every one \
         of these key ranges lives in a single tablet."
    );

    let walked: Vec<(i16, usize)> = covering.iter().copied().filter(|(_, p)| *p > 1).collect();
    assert!(
        walked.is_empty(),
        "covering arm walked past its key range: (bucket, pages) = {walked:?}. Each of these \
         binds a (bucket_id, branch_id, segno) prefix whose whole key range is inside one \
         tablet, so one page must exhaust the scan; instead the page count tracks how many \
         tablets sit BELOW the bucket, i.e. the descending scan runs from its start key to \
         the beginning of the table. In `fetch_chunks_thin` this is the WAL-replay wedge \
         (#739): on a production-sized `wals` the walk outruns the barren-round guard."
    );
    let point_walked: Vec<&(&str, usize)> = points.iter().filter(|(_, p)| *p > 1).collect();
    assert!(
        point_walked.is_empty(),
        "a full-key point read returned its row on page 1 and then kept paging: {point_walked:?} \
         (direction, pages) — one page per tablet still ahead of it. `range_values` fixes where \
         a scan STARTS but nothing bounds where it ENDS, so this is not about reverse scans, \
         empty result sets, or the open-ended last tablet: every read on a range-sharded table \
         runs to the physical end of the table in whichever direction it scans."
    );

    ysql.execute(&format!("DROP TABLE IF EXISTS {TABLE}"), &[])
        .await
        .unwrap();
}
