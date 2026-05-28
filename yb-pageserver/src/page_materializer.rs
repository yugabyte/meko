//! Production [`Materializer`] impl that drives walredo for the
//! [`PageWriter`]'s `ForceImage` path.
//!
//! Algorithm:
//! 1. Sort `prior_chain` ascending so the anchor (lowest LSN
//!    `will_init=true` row) is first.
//! 2. If the anchor is an image, hand it to walredo as `base_img`.
//!    Otherwise treat it as the first WAL record applied.
//! 3. Decode every other row's `payload` as a `NeonWalRecord` and feed
//!    them to walredo in ascending-LSN order, followed by the `current`
//!    entry's `NeonWalRecord`.
//! 4. Return the 8 KiB page bytes walredo produced.
//!
//! Tests live alongside `PageWriter` in `page_writer::tests` with mock
//! materializers. A walredo-driven end-to-end test would require a real
//! `PostgresRedoManager` subprocess and Postgres binaries; that's
//! deferred to Phase 3 integration testing.

use std::sync::Arc;

use anyhow::{Result, bail};
use bytes::Bytes;
use futures::FutureExt;
use futures::future::BoxFuture;
use pageserver_api::key::Key;
use postgres_ffi::PgMajorVersion;
use utils::bin_ser::BeSer;
use utils::lsn::Lsn;
use wal_decoder::models::record::NeonWalRecord;
use wal_decoder::models::value::Value;

use crate::page_store::PageRow;
use crate::page_writer::Materializer;
use crate::tenant::WalRedoManager;
use crate::walredo::RedoAttemptType;

/// [`Materializer`] backed by the existing walredo subprocess. Production
/// wiring uses this; tests use the mocks under `page_writer::tests`.
///
/// Wraps the enum [`WalRedoManager`] (not the raw
/// `PostgresRedoManager`) so the Timeline's existing
/// `Arc<WalRedoManager>` can be reused directly, and so unit tests can
/// inject the `Test` variant when needed.
pub struct WalRedoMaterializer {
    walredo: Arc<WalRedoManager>,
    pg_version: PgMajorVersion,
}

impl WalRedoMaterializer {
    pub(crate) fn new(walredo: Arc<WalRedoManager>, pg_version: PgMajorVersion) -> Self {
        Self {
            walredo,
            pg_version,
        }
    }
}

impl Materializer for WalRedoMaterializer {
    fn materialize<'a>(
        &'a self,
        key: Key,
        target_lsn: Lsn,
        prior_chain: Vec<PageRow>,
        current: &'a Value,
    ) -> BoxFuture<'a, Result<Bytes>> {
        async move {
            let mut ascending = prior_chain;
            ascending.sort_by(|a, b| a.lsn.cmp(&b.lsn));
            let (base_img, mut records) = decode_chain_for_walredo(&ascending)?;
            match current {
                Value::WalRecord(rec) => records.push((target_lsn, rec.clone())),
                Value::Image(_) => {
                    bail!(
                        "WalRedoMaterializer: ForceImage selected with a `Value::Image` current — ImageRule should have passed it through"
                    );
                }
            }
            if records.is_empty() {
                bail!(
                    "WalRedoMaterializer: empty record set for key={key} lsn={target_lsn} — no chain to replay"
                );
            }
            self.walredo
                .request_redo(
                    key,
                    target_lsn,
                    base_img,
                    records,
                    self.pg_version,
                    RedoAttemptType::ReadPage,
                )
                .await
                .map_err(|e| anyhow::Error::from(e).context("walredo"))
        }
        .boxed()
    }
}

/// Convert an ascending-LSN chain into walredo inputs.
///
/// If the anchor (lowest-LSN row) is `is_image`, it becomes the
/// `base_img`. Otherwise the anchor is decoded as a `NeonWalRecord` and
/// prepended to `records` — walredo handles `will_init` records that
/// reset the page state from scratch.
fn decode_chain_for_walredo(
    ascending: &[PageRow],
) -> Result<(Option<(Lsn, Bytes)>, Vec<(Lsn, NeonWalRecord)>)> {
    let (anchor, rest) = match ascending.split_first() {
        Some(s) => s,
        None => return Ok((None, Vec::new())),
    };
    let mut base_img: Option<(Lsn, Bytes)> = None;
    let mut records: Vec<(Lsn, NeonWalRecord)> = Vec::with_capacity(rest.len() + 1);
    if anchor.is_image {
        base_img = Some((anchor.lsn, anchor.payload.clone()));
    } else {
        let rec = NeonWalRecord::des(&anchor.payload)?;
        records.push((anchor.lsn, rec));
    }
    for row in rest {
        let rec = NeonWalRecord::des(&row.payload)?;
        records.push((row.lsn, rec));
    }
    Ok((base_img, records))
}

#[cfg(test)]
mod tests {
    //! These tests cover the pure `decode_chain_for_walredo` helper.
    //! End-to-end materialisation requires a real walredo subprocess +
    //! Postgres binaries and is exercised by Phase 3 integration tests.

    use super::*;
    use bytes::Bytes;
    use utils::bin_ser::BeSer;

    fn img_row(lsn: u64) -> PageRow {
        PageRow {
            key: Bytes::from_static(b"k"),
            lsn: Lsn(lsn),
            is_image: true,
            will_init: true,
            payload: Bytes::from(vec![0u8; 8192]),
        }
    }

    fn delta_row(lsn: u64, will_init: bool) -> PageRow {
        let rec = NeonWalRecord::Postgres {
            will_init,
            rec: Bytes::from_static(b"bytes"),
        };
        PageRow {
            key: Bytes::from_static(b"k"),
            lsn: Lsn(lsn),
            is_image: false,
            will_init,
            payload: Bytes::from(rec.ser().unwrap()),
        }
    }

    #[test]
    fn decode_with_image_anchor() {
        let chain = vec![img_row(10), delta_row(20, false), delta_row(30, false)];
        let (base, records) = decode_chain_for_walredo(&chain).unwrap();
        assert!(base.is_some());
        let (base_lsn, _) = base.unwrap();
        assert_eq!(base_lsn, Lsn(10));
        assert_eq!(records.len(), 2);
        assert_eq!(records[0].0, Lsn(20));
        assert_eq!(records[1].0, Lsn(30));
    }

    #[test]
    fn decode_with_will_init_delta_anchor() {
        let chain = vec![delta_row(10, true), delta_row(20, false)];
        let (base, records) = decode_chain_for_walredo(&chain).unwrap();
        assert!(base.is_none(), "no image — walredo replays from scratch");
        assert_eq!(records.len(), 2);
        assert_eq!(records[0].0, Lsn(10));
        match &records[0].1 {
            NeonWalRecord::Postgres { will_init, .. } => assert!(*will_init),
            _ => panic!("expected will_init Postgres record"),
        }
    }

    #[test]
    fn decode_empty_chain() {
        let (base, records) = decode_chain_for_walredo(&[]).unwrap();
        assert!(base.is_none());
        assert!(records.is_empty());
    }

    #[test]
    fn decode_single_anchor_only() {
        let chain = vec![img_row(42)];
        let (base, records) = decode_chain_for_walredo(&chain).unwrap();
        assert_eq!(base.unwrap().0, Lsn(42));
        assert!(records.is_empty());
    }

    #[test]
    fn decode_rejects_malformed_delta_payload() {
        let bad = PageRow {
            key: Bytes::from_static(b"k"),
            lsn: Lsn(20),
            is_image: false,
            will_init: false,
            payload: Bytes::from_static(b"not-a-valid-NeonWalRecord"),
        };
        let chain = vec![img_row(10), bad];
        let err = decode_chain_for_walredo(&chain).unwrap_err();
        let msg = format!("{err}").to_lowercase();
        assert!(
            msg.contains("deserialize") || msg.contains("decode") || msg.contains("custom"),
            "expected a deserialize error, got: {err:?}"
        );
    }
}
