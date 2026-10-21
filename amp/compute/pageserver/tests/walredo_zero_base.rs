//! walredo against an all-zero base page.
//!
//! `yb_amp_zeroextend` WAL-logs every newly extended block as an all-zero FPI,
//! so that image is a fresh block's anchor. With `full_page_writes = off`, an
//! extension that writes Generic WAL against a fresh page (RUM's non-root
//! split) then leaves a chain of a zero image plus Generic deltas. Stock
//! `generic_redo` reads the block with `RBM_NORMAL`, which treats an all-zero
//! page as missing, so the deltas were skipped and walredo returned a zero
//! page without an error.
//!
//! The records are built by hand in the PG18 `XLogRecord` layout; walredo
//! decodes them without checking the CRC. These tests need a real walredo
//! subprocess, so they need the PostgreSQL install that CI builds before it
//! runs the unit tests. walredo reports why it refused a record only on its
//! stderr, which the pageserver logs, so the tests read it back from the log.

use std::io::Write;
use std::sync::{Mutex, Once};
use std::time::Duration;

use bytes::{BufMut, Bytes, BytesMut};
use pageserver::config::PageServerConf;
use pageserver::walredo::{PostgresRedoManager, RedoAttemptType};
use pageserver_api::key::{Key, rel_block_to_key};
use pageserver_api::reltag::RelTag;
use postgres_ffi::PgMajorVersion;
use postgres_ffi::pg_constants::{
    BKPBLOCK_HAS_DATA, BKPBLOCK_HAS_IMAGE, RM_HEAP_ID, XLOG_HEAP_LOCK, XLR_BLOCK_ID_DATA_SHORT,
};
use tracing::Instrument;
use utils::id::BranchId;
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;

const PG: PgMajorVersion = PgMajorVersion::PG18;
const BLCKSZ: usize = 8192;
/// access/rmgrlist.h
const RM_GENERIC_ID: u8 = 20;
/// access/rmgr.h: the id DocumentDB's DOCDB_EXRUM registers, never loaded in walredo.
const RM_EXPERIMENTAL_ID: u8 = 128;
/// access/xlogrecord.h (PG15+)
const BKPIMAGE_APPLY: u8 = 0x02;
const SIZE_OF_PAGE_HEADER: usize = 24;

const REL: RelTag = RelTag {
    spcnode: 1663,
    dbnode: 16384,
    relnode: 24576,
    forknum: 0,
};
const BLKNO: u32 = 7;

static LOGGING: Once = Once::new();
static LOGS: Mutex<Vec<u8>> = Mutex::new(Vec::new());

/// Tees the log to stdout and into [`LOGS`].
struct Capture;

impl Write for Capture {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        LOGS.lock().unwrap().extend_from_slice(buf);
        std::io::stdout().write_all(buf)?;
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        std::io::stdout().flush()
    }
}

fn run<F>(tag: &str, body: impl FnOnce(PostgresRedoManager, BranchId) -> F)
where
    F: std::future::Future<Output = ()>,
{
    LOGGING.call_once(|| {
        use tracing_subscriber::prelude::*;
        tracing_subscriber::registry()
            .with(
                tracing_subscriber::fmt::layer()
                    .with_ansi(false)
                    .with_writer(|| Capture)
                    .with_filter(tracing_subscriber::filter::LevelFilter::INFO),
            )
            // The pageserver's span assertions need it.
            .with(tracing_error::ErrorLayer::default())
            .init();
    });
    let repo_dir = camino::Utf8PathBuf::from(format!(
        "/tmp/walredo_zero_base_{}_{tag}",
        std::process::id()
    ));
    std::fs::create_dir_all(&repo_dir).unwrap();
    let conf: &'static PageServerConf = Box::leak(Box::new(PageServerConf::dummy_conf(repo_dir)));
    let branch_id = BranchId::generate();
    let mgr = PostgresRedoManager::new(conf, branch_id);
    let span = tracing::info_span!("test", branch_id = %branch_id);
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(body(mgr, branch_id).instrument(span));
}

/// Whether a walredo child of `branch` wrote `msg` to its stderr. The
/// pageserver logs that stderr from a task of its own, so wait for it.
async fn walredo_logged(branch: BranchId, msg: &str) -> bool {
    let branch = branch.to_string();
    for _ in 0..100 {
        let logs = String::from_utf8_lossy(&LOGS.lock().unwrap()).into_owned();
        if logs.lines().any(|l| {
            l.contains("wal-redo-postgres-stderr") && l.contains(&branch) && l.contains(msg)
        }) {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    false
}

fn key() -> Key {
    rel_block_to_key(REL, BLKNO)
}

fn zero_page() -> Bytes {
    Bytes::from(vec![0u8; BLCKSZ])
}

/// A standard-layout page: one 32-byte item under a 16-byte special space, the
/// shape `PageInit` + `PageAddItem` leaves.
fn standard_page(fill: u8) -> Vec<u8> {
    let special = BLCKSZ - 16;
    let upper = special - 32;
    let lower = SIZE_OF_PAGE_HEADER + 4;
    let mut p = vec![0u8; BLCKSZ];
    p[12..14].copy_from_slice(&(lower as u16).to_le_bytes());
    p[14..16].copy_from_slice(&(upper as u16).to_le_bytes());
    p[16..18].copy_from_slice(&(special as u16).to_le_bytes());
    p[18..20].copy_from_slice(&((BLCKSZ as u16) | 4).to_le_bytes());
    // ItemIdData { lp_off:15, lp_flags:2 = LP_NORMAL, lp_len:15 }
    let lp: u32 = (upper as u32) | (1 << 15) | (32 << 17);
    p[24..28].copy_from_slice(&lp.to_le_bytes());
    p[upper..special].fill(fill);
    p[special..].fill(0x5c);
    p
}

/// [`standard_page`] after a second 32-byte item was added, as a tuple insert
/// leaves it.
fn standard_page_with_second_item(fill: u8, second: u8) -> Vec<u8> {
    let mut p = standard_page(fill);
    let upper = BLCKSZ - 16 - 64;
    let lower = SIZE_OF_PAGE_HEADER + 8;
    p[12..14].copy_from_slice(&(lower as u16).to_le_bytes());
    p[14..16].copy_from_slice(&(upper as u16).to_le_bytes());
    let lp: u32 = (upper as u32) | (1 << 15) | (32 << 17);
    p[28..32].copy_from_slice(&lp.to_le_bytes());
    p[upper..upper + 32].fill(second);
    p
}

fn with_lsn(mut page: Vec<u8>, lsn: Lsn) -> Vec<u8> {
    page[0..4].copy_from_slice(&((lsn.0 >> 32) as u32).to_le_bytes());
    page[4..8].copy_from_slice(&(lsn.0 as u32).to_le_bytes());
    page
}

/// Generic WAL delta fragments, as `computeDelta` writes them: `{u16 offset,
/// u16 length, data}`.
fn delta(fragments: &[(usize, &[u8])]) -> Vec<u8> {
    let mut d = Vec::new();
    for (off, data) in fragments {
        d.extend_from_slice(&(*off as u16).to_le_bytes());
        d.extend_from_slice(&(data.len() as u16).to_le_bytes());
        d.extend_from_slice(data);
    }
    d
}

/// `computeDelta` of `target` against an all-zero page: the bytes below
/// `pd_lower` and from `pd_upper` on.
fn delta_from_zero(target: &[u8]) -> Vec<u8> {
    let lower = u16::from_le_bytes([target[12], target[13]]) as usize;
    let upper = u16::from_le_bytes([target[14], target[15]]) as usize;
    delta(&[(0, &target[..lower]), (upper, &target[upper..])])
}

enum Block<'a> {
    Data(&'a [u8]),
    Image(&'a [u8]),
    Ref,
}

/// One XLogRecord referencing `(REL, BLKNO)` as block 0.
fn record(rmid: u8, info: u8, block: Block, main_data: &[u8]) -> NeonWalRecord {
    let (fork_flags, data, image): (u8, &[u8], &[u8]) = match block {
        Block::Data(d) => (BKPBLOCK_HAS_DATA, d, &[]),
        Block::Image(i) => (BKPBLOCK_HAS_IMAGE, &[], i),
        Block::Ref => (0, &[], &[]),
    };
    let mut body = BytesMut::new();
    body.put_u8(0); // block id
    body.put_u8(fork_flags | REL.forknum);
    body.put_u16_le(data.len() as u16);
    if !image.is_empty() {
        // XLogRecordBlockImageHeader: length, hole_offset, bimg_info (no hole)
        body.put_u16_le(image.len() as u16);
        body.put_u16_le(0);
        body.put_u8(BKPIMAGE_APPLY);
    }
    body.put_u32_le(REL.spcnode);
    body.put_u32_le(REL.dbnode);
    body.put_u32_le(REL.relnode);
    body.put_u32_le(BLKNO);
    if !main_data.is_empty() {
        body.put_u8(XLR_BLOCK_ID_DATA_SHORT);
        body.put_u8(main_data.len() as u8);
    }
    body.put_slice(image);
    body.put_slice(data);
    body.put_slice(main_data);

    let mut rec = BytesMut::new();
    rec.put_u32_le(24 + body.len() as u32); // xl_tot_len
    rec.put_u32_le(0); // xl_xid
    rec.put_u64_le(0); // xl_prev
    rec.put_u8(info);
    rec.put_u8(rmid);
    rec.put_u16_le(0); // padding
    rec.put_u32_le(0); // xl_crc, not checked by walredo
    rec.put_slice(&body);
    NeonWalRecord::Postgres {
        will_init: !image.is_empty(),
        rec: rec.freeze(),
    }
}

fn generic(block: Block) -> NeonWalRecord {
    record(RM_GENERIC_ID, 0, block, &[])
}

/// `assert_eq!` on two 8 KiB pages prints both in full; say where they differ.
fn assert_page(got: &[u8], want: &[u8]) {
    if let Some(off) = got.iter().zip(want).position(|(a, b)| a != b) {
        let zero = got.iter().all(|b| *b == 0);
        panic!("page differs from offset {off} (got an all-zero page: {zero})");
    }
    assert_eq!(got.len(), want.len());
}

const BASE_LSN: Lsn = Lsn(0x0100_0000);
const LSN_1: Lsn = Lsn(0x0100_0100);
const LSN_2: Lsn = Lsn(0x0100_0200);

async fn redo(
    mgr: &PostgresRedoManager,
    base: Option<Bytes>,
    records: Vec<(Lsn, NeonWalRecord)>,
) -> anyhow::Result<Vec<u8>> {
    let lsn = records.last().unwrap().0;
    let img = mgr
        .request_redo(
            key(),
            lsn,
            base.map(|b| (BASE_LSN, b)),
            records,
            PG,
            RedoAttemptType::ReadPage,
        )
        .await?;
    Ok(img.to_vec())
}

/// The RUM case: the zeroextend FPI is the base, and the split logs the new
/// page as a delta against zero.
#[test]
fn a_generic_delta_onto_an_all_zero_base_reconstructs_the_page() {
    run("delta_zero_base", |mgr, _| async move {
        let target = standard_page(0xab);
        let got = redo(
            &mgr,
            Some(zero_page()),
            vec![(LSN_1, generic(Block::Data(&delta_from_zero(&target))))],
        )
        .await
        .expect("redo");
        assert_page(&got, &with_lsn(target, LSN_1));
    });
}

/// Later deltas on the reconstructed page go through stock `generic_redo`.
#[test]
fn deltas_after_the_first_apply_on_top_of_it() {
    run("delta_chain", |mgr, _| async move {
        let v1 = standard_page(0xab);
        let upper = BLCKSZ - 16 - 32;
        let patch = [0xcd_u8; 4];
        let mut v2 = v1.clone();
        v2[upper..upper + 4].copy_from_slice(&patch);
        let got = redo(
            &mgr,
            Some(zero_page()),
            vec![
                (LSN_1, generic(Block::Data(&delta_from_zero(&v1)))),
                (LSN_2, generic(Block::Data(&delta(&[(upper, &patch)])))),
            ],
        )
        .await
        .expect("redo");
        assert_page(&got, &with_lsn(v2, LSN_2));
    });
}

#[test]
fn a_generic_full_image_still_restores() {
    run("full_image", |mgr, _| async move {
        let target = standard_page(0x11);
        let got = redo(
            &mgr,
            Some(zero_page()),
            vec![(LSN_1, generic(Block::Image(&target)))],
        )
        .await
        .expect("redo");
        assert_page(&got, &with_lsn(target, LSN_1));
    });
}

/// An older walredo turned pages like these into zero images while the compute
/// kept logging Generic deltas against its real copy. Applied onto zero, such
/// a delta leaves a partial page, or one with no header at all. The page stays
/// all-zero, as before, rather than either being stored or failing the
/// materialiser on every retry.
#[test]
fn a_generic_delta_not_computed_against_zero_leaves_the_page_zero() {
    run("delta_not_from_zero", |mgr, branch| async move {
        let v2 = standard_page_with_second_item(0xab, 0xcd);
        let upper2 = BLCKSZ - 16 - 64;
        // computeDelta(standard_page -> v2): pd_lower/pd_upper, the new line
        // pointer and the new item. The rest of the header is unchanged, so absent.
        let insert = delta(&[
            (12, &v2[12..16]),
            (28, &v2[28..32]),
            (upper2, &v2[upper2..upper2 + 32]),
        ]);
        let body_only = delta(&[(4000, &[0xee; 8])]);
        for d in [insert, body_only] {
            let got = redo(
                &mgr,
                Some(zero_page()),
                vec![(LSN_1, generic(Block::Data(&d)))],
            )
            .await
            .expect("redo");
            assert_page(&got, &zero_page());
        }
        assert!(
            walredo_logged(branch, "was not computed against an all-zero page").await,
            "walredo did not report the delta it left unapplied"
        );
    });
}

/// `heap_xlog_lock` on an all-zero page used to be skipped and the zero page
/// returned.
#[test]
fn a_non_init_record_onto_an_all_zero_base_fails() {
    run("heap_lock_zero_base", |mgr, branch| async move {
        // xl_heap_lock { xmax, offnum, infobits_set, flags }
        let mut xlrec = Vec::new();
        xlrec.extend_from_slice(&1000u32.to_le_bytes());
        xlrec.extend_from_slice(&1u16.to_le_bytes());
        xlrec.extend_from_slice(&[0, 0]);
        let res = redo(
            &mgr,
            Some(zero_page()),
            vec![(
                LSN_1,
                record(RM_HEAP_ID, XLOG_HEAP_LOCK, Block::Ref, &xlrec),
            )],
        )
        .await;
        assert!(res.is_err(), "expected an error, got a page");
        assert!(
            walredo_logged(branch, "does not initialize block 7").await,
            "walredo failed, but not on the all-zero-base check"
        );
    });
}

#[test]
fn a_record_for_an_unloaded_rmgr_fails() {
    run("unloaded_rmgr", |mgr, branch| async move {
        let target = standard_page(0xab);
        let res = redo(
            &mgr,
            Some(zero_page()),
            vec![(
                LSN_1,
                record(
                    RM_EXPERIMENTAL_ID,
                    0,
                    Block::Data(&delta_from_zero(&target)),
                    &[],
                ),
            )],
        )
        .await;
        assert!(res.is_err(), "expected an error, got a page");
        assert!(
            walredo_logged(
                branch,
                "resource manager 128, which is not loaded in walredo"
            )
            .await,
            "walredo failed, but not on the unloaded-rmgr check"
        );
    });
}
