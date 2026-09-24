//! Fixtures shared by `page_store`'s tests and `page_store::gc`'s: the
//! process-wide store, the branch/key/row builders and the hot-path lane
//! sample counter.

use super::*;

pub(super) fn test_yb_config() -> YbConnConfig {
    let host = std::env::var("YB_TEST_HOST").unwrap_or_else(|_| "127.0.0.1".to_string());
    let port = std::env::var("YB_TEST_PORT").unwrap_or_else(|_| "5433".to_string());
    YbConnConfig {
        host_ports: format!("{host}:{port}").parse().unwrap(),
        user: std::env::var("YB_TEST_USER").unwrap_or_else(|_| "yugabyte".to_string()),
        password_env: None,
        dbname: std::env::var("YB_TEST_DB").unwrap_or_else(|_| "yb_storage".to_string()),
        sslmode: std::env::var("YB_TEST_SSLMODE").ok(),
        sslrootcert: std::env::var("YB_TEST_SSLROOTCERT").ok(),
        sslcert: std::env::var("YB_TEST_SSLCERT").ok(),
        sslkey: std::env::var("YB_TEST_SSLKEY").ok(),
        disable_transactional_writes: false,
        tserver_rpc_addrs: Some(
            std::env::var("YB_TEST_TSERVER_RPC")
                .unwrap_or_else(|_| "127.0.0.1:9100".to_string())
                .parse()
                .unwrap(),
        ),
        tsclient_read_sessions: None,
        // Exercise the pooled-write fan-out rather than a single session.
        tsclient_write_sessions: Some(2),
        tsclient_sessions_per_conn: None,
        connect_timeout_ms: ysql_client::DEFAULT_CONNECT_TIMEOUT_MS,
        statement_timeout_ms: ysql_client::DEFAULT_STATEMENT_TIMEOUT_MS,
        tcp_user_timeout_ms: 0,
        connection_lifetime_ms: ysql_client::DEFAULT_CONNECTION_LIFETIME_MS,
    }
}

/// All tests run against the production `amp_storage` schema
/// (created by the controller's migration on a
/// real `yb_amp_local`-style cluster). Per-test isolation comes
/// from `rand_branch()` — every test writes
/// to a distinct `branch_id` namespace, so
/// concurrent tests don't observe each other's data and rows
/// from prior runs are harmless.
///
/// Requires the YB cluster to have the diesel migration applied
/// (e.g. `yb_amp_local init && yb_amp_local start` once). The tests
/// don't create the schema themselves; that's the migration's
/// job and there's only one source of truth for it.
static SHARED_STORE: tokio::sync::OnceCell<PageStore> = tokio::sync::OnceCell::const_new();
// Never assert exact DELETE rowcounts in these tests: ysql_client
// retries transient YB errors at-least-once, so a retry after a
// committed-but-unacked first attempt reports the SECOND
// execution's count (undercounts under load — seen in CI). Assert
// the surviving rows instead; `== 0` counts are retry-safe.
pub(super) const PROD_SCHEMA: &str = "amp_storage";

/// Create a branch the way production does: the controller provisions the
/// row, then the pageserver fills in its own column group.
pub(super) async fn create_branch_state_row(
    store: &PageStore,
    row: &BranchStateRow,
) -> anyhow::Result<()> {
    store
        .provision_branch_row_for_test(row.branch_id, row.pg_version)
        .await?;
    store.update_branch_initial_state(row).await
}

pub(super) async fn make_store(_suffix: &str) -> (PageStore, String) {
    let store = SHARED_STORE
        .get_or_init(|| async {
            let cfg = PageStoreConfig {
                yb: test_yb_config(),
                schema: PROD_SCHEMA.to_string(),
                scale_factor: pageserver_api::config::DEFAULT_SCALE_FACTOR,
                payload_compression:
                    pageserver_api::config::defaults::DEFAULT_YB_PAYLOAD_COMPRESSION,
            };
            PageStore::connect(cfg, "page_store_test", true)
                .await
                .expect("connect to local YB; run `yb_amp_local init && start` first to migrate the schema")
        })
        .await
        .clone();
    (store, PROD_SCHEMA.to_string())
}

pub(super) async fn drop_schema(_store: &PageStore, _schema: &str) {
    // No-op: the schema is the production one, owned by the
    // diesel migration. Per-test isolation comes from random
    // branch IDs.
}

pub(super) fn rand_branch() -> BranchId {
    BranchId::generate()
}

/// Build a synthetic relation-page Key:
/// `field1=0, spcnode, dbnode, relnode=16384+ord, forknum=0, blknum`.
/// `field4 != 0` is required by `is_rel_block_key`.
pub(super) fn page_key(spcnode: u32, dbnode: u32, relnode: u32, fork: u8, blknum: u32) -> Bytes {
    columns_to_key_bytes(spcnode, dbnode, relnode, fork as i16, blknum)
}

pub(super) fn page_row(
    key: Bytes,
    lsn: u64,
    is_image: bool,
    will_init: bool,
    payload: &[u8],
) -> Arc<PageRow> {
    Arc::new(PageRow {
        key: Key::from_slice(&key),
        lsn: Lsn(lsn),
        is_image,
        will_init,
        payload: Bytes::copy_from_slice(payload),
    })
}

pub(super) fn sample_branch_state_row(branch: BranchId) -> BranchStateRow {
    BranchStateRow {
        branch_id: branch,
        consistent_lsn: Lsn(90),
        applied_gc_cutoff_lsn: Lsn(50),
        ancestor_branch_id: None,
        ancestor_lsn: None,
        pg_version: 180_002,
        initdb_lsn: None,
        prev_record_lsn: None,
    }
}

/// Sample count of each op served by the hot-path lane
/// (`pageserver_yb_store_op_seconds{op, path="yb_thin_client"}`, observed
/// only on the success arm). Lets a test assert the op it drove actually
/// reached the lane and came back clean.
pub(super) fn thin_op_count(op: &str) -> u64 {
    crate::metrics::YB_STORE_OP_SECONDS
        .with_label_values(&[op, "yb_thin_client"])
        .get_sample_count()
}
