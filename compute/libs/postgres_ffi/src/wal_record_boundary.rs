//! Find the last valid WAL **record boundary** at or below a byte
//! position, by replaying the real [`WalStreamDecoder`] over raw WAL
//! bytes.
//!
//! Both the pageserver (`yb-reset-consistent-lsn`) and the safekeeper
//! (rehydrating its end-of-WAL on restart) need this: the raw "max byte
//! offset" persisted in `wals` can land **mid-record** when an
//! ungraceful compute shutdown leaves a torn trailing page. Treating that
//! byte offset as the durable end-of-WAL makes a reader decode into the
//! torn page and fail (`invalid xlog page header`). The honest end-of-WAL
//! is the largest *complete record* boundary `<= max`, which is exactly
//! where Postgres crash recovery stops.
//!
//! [`WalSource`] abstracts the byte source so callers plug in their own
//! `wals` access (and tests an in-memory buffer); [`find_anchor`] +
//! [`decode_forward`] are the generic scan. [`last_valid_boundary`] wraps
//! them for the common "truncate a torn tail" case.

use std::cmp::min;

use anyhow::{Context, bail};
use utils::lsn::Lsn;

use crate::v18::bindings::XLOG_PAGE_MAGIC;
use crate::v18::xlog_utils::XLP_FIRST_IS_CONTRECORD;
use crate::waldecoder::WalStreamDecoder;
use crate::{PgMajorVersion, XLOG_BLCKSZ, XLOG_SIZE_OF_XLOG_LONG_PHD, XLOG_SIZE_OF_XLOG_SHORT_PHD};

/// How far below `target` we are willing to scan, page by page, looking
/// for a WAL page that begins a fresh record (no continuation). One real
/// WAL record almost never exceeds a few MiB; 64 MiB is a generous bound
/// past which we'd rather fail loudly than scan into the next tenant's
/// history.
pub const MAX_ANCHOR_SCAN_BYTES: u64 = 64 * 1024 * 1024;

/// A source of raw WAL bytes addressed by LSN. The DB-backed readers
/// resolve chunks from a `wals` table; tests supply an in-memory buffer.
/// [`find_anchor`] / [`decode_forward`] are generic over this so they can
/// run without a database.
#[allow(async_fn_in_trait)]
pub trait WalSource {
    /// WAL segment size, for LSN ↔ `(segno, offset)` mapping.
    fn seg(&self) -> u64;

    /// Fetch the chunk covering `lsn`: its absolute start LSN and raw
    /// bytes. Errors if no chunk covers `lsn` (a WAL hole).
    async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<(Lsn, Vec<u8>)>;

    /// Read exactly `len` bytes starting at `lsn`, assembling across
    /// chunk (and segment) boundaries. Errors on a hole.
    async fn read_exact(&self, lsn: Lsn, len: usize) -> anyhow::Result<Vec<u8>> {
        let mut out = Vec::with_capacity(len);
        let mut pos = lsn;
        while out.len() < len {
            let (chunk_start, data) = self.chunk_at(pos).await?;
            let from = (pos.0 - chunk_start.0) as usize;
            let take = min(data.len() - from, len - out.len());
            out.extend_from_slice(&data[from..from + take]);
            pos = Lsn(pos.0 + take as u64);
        }
        Ok(out)
    }
}

/// The size of the page header at a page-aligned `lsn`: long header at a
/// segment boundary, short header otherwise.
pub fn page_header_size(lsn: Lsn, seg: u64) -> usize {
    if lsn.0 % seg == 0 {
        XLOG_SIZE_OF_XLOG_LONG_PHD
    } else {
        XLOG_SIZE_OF_XLOG_SHORT_PHD
    }
}

/// Walk back from `target`'s page to the first *real* WAL page that
/// begins a fresh record — one whose `xlp_magic` is `XLOG_PAGE_MAGIC`
/// **and** whose `XLP_FIRST_IS_CONTRECORD` bit is clear (so the page is
/// not a record continuation). That page boundary is itself a guaranteed
/// record boundary `<= target`: seeding the decoder there makes it
/// consume the page header and the first record exactly as a real reader
/// does, so the boundaries it then reports use the same (page-boundary)
/// representation that gets persisted.
///
/// The magic check is load-bearing: `pg_switch_wal()` (an XLOG_SWITCH
/// record) zero-pads the rest of the segment, so the pages after it have
/// `xlp_magic == 0` and, incidentally, `xlp_info == 0` — the contrecord
/// bit clear. Without the magic check, such a zeroed padding page would
/// be wrongly accepted as an anchor; seeding the decoder there fails with
/// `invalid xlog page header` and wedges the safekeeper on restart (#181).
/// Requiring a valid magic walks the scan back past the padding to the
/// real page that holds the XLOG_SWITCH, which the decoder then skips to
/// the next segment. The same conservatism handles a torn trailing page
/// (zeroed magic, no preceding switch): the scan lands on the last real
/// page and `find_end_of_wal` stops at the last complete record.
///
/// `floor` is the lowest known WAL record boundary — the start of the
/// persisted WAL (`Lsn(0)` if the caller has none). The backward walk stops
/// there: probing below `floor` reads a hole (the store never held those
/// bytes), and when a single record spans all the way back to the WAL start
/// — e.g. wal_craft's segment-filling padding record right before an
/// XLOG_SWITCH — no fresh-record page exists above `floor` to anchor on.
/// `floor` is itself a valid seed even when it lands mid-page: the decoder
/// starts cleanly at a record boundary and parses page headers as it later
/// crosses page boundaries. Without this stop, a mid-page `floor` makes the
/// page-aligned walk overshoot to the page *below* the store, error on the
/// hole, and abort the whole end-of-WAL scan — leaving the safekeeper's
/// `write_lsn` short of the post-switch segment boundary, which wedges the
/// catch-up reader (#181).
pub async fn find_anchor<S: WalSource>(reader: &S, target: Lsn, floor: Lsn) -> anyhow::Result<Lsn> {
    let blk = XLOG_BLCKSZ as u64;
    let floor_page = Lsn(floor.0 - floor.0 % blk);
    let mut page = Lsn(target.0 - target.0 % blk);
    let scan_floor = target.0.saturating_sub(MAX_ANCHOR_SCAN_BYTES);
    loop {
        // Reached the page holding the known WAL start: anchor at `floor`
        // itself rather than reading below it. `Lsn(0)` means "no known
        // floor" — fall through and walk to the bottom as before.
        if floor.0 != 0 && page <= floor_page {
            return Ok(floor);
        }
        let hdr_size = page_header_size(page, reader.seg());
        let hdr = reader
            .read_exact(page, hdr_size)
            .await
            .with_context(|| format!("reading WAL page header at {page}"))?;
        // xlp_magic is the leading u16; xlp_info the u16 right after it
        // (offset 2). See XLogPageHeaderData layout in postgres_ffi.
        let xlp_magic = u16::from_le_bytes([hdr[0], hdr[1]]);
        let xlp_info = u16::from_le_bytes([hdr[2], hdr[3]]);
        if xlp_magic == XLOG_PAGE_MAGIC as u16 && xlp_info & XLP_FIRST_IS_CONTRECORD == 0 {
            return Ok(page);
        }
        if page.0 == 0 || page.0 <= scan_floor {
            bail!(
                "no fresh-record WAL page found within {} MiB below {target}; target may sit inside \
                 an unusually large record or outside the persisted WAL range",
                MAX_ANCHOR_SCAN_BYTES / (1024 * 1024)
            );
        }
        page = Lsn(page.0 - blk);
    }
}

/// Seed the real `WalStreamDecoder` at `anchor` and replay forward over
/// the WAL bytes, collecting record boundaries until one exceeds `target`
/// (or the WAL runs out / a hole / a decode error stops us). Returns the
/// boundaries (ascending, starting with `anchor`) and, if the walk
/// stopped before passing `target`, a human-readable reason.
pub async fn decode_forward<S: WalSource>(
    reader: &S,
    anchor: Lsn,
    target: Lsn,
    pg_version: PgMajorVersion,
) -> anyhow::Result<(Vec<Lsn>, Option<String>)> {
    let mut decoder = WalStreamDecoder::new(anchor, pg_version);
    let mut boundaries = vec![anchor];
    let mut pos = anchor;
    let mut stop: Option<String> = None;

    'outer: loop {
        loop {
            match decoder.poll_decode() {
                Ok(Some((next_lsn, _rec))) => {
                    boundaries.push(next_lsn);
                    if next_lsn > target {
                        break 'outer;
                    }
                }
                Ok(None) => break, // need more bytes
                Err(e) => {
                    stop = Some(format!("decode error: {e}"));
                    break 'outer;
                }
            }
        }
        // Feed the next chunk of WAL bytes.
        match reader.chunk_at(pos).await {
            Ok((chunk_start, data)) => {
                let from = (pos.0 - chunk_start.0) as usize;
                decoder.feed_bytes(&data[from..]);
                pos = Lsn(chunk_start.0 + data.len() as u64);
            }
            Err(e) => {
                stop = Some(format!("WAL ended before reaching target: {e}"));
                break 'outer;
            }
        }
    }
    Ok((boundaries, stop))
}

// Returns (aligned) end_lsn of the last record in the WAL `reader`.
// start_lsn must point to some previously known record boundary (beginning of
// the next record). If no valid record after is found, start_lsn is returned
// back.
//
// A line-by-line port of upstream Neon's
// `postgres_ffi::xlog_utils::find_end_of_wal` — the local-segment-file scanner
// the safekeeper used to recompute flush_lsn on restart before WAL moved to
// YB. The decode loop is identical; only the byte source differs: WAL chunks
// from a `WalSource` instead of `read`s from segment files. A torn trailing
// page makes the decoder error, and `result` (the end of the last complete
// record) is returned — exactly as upstream truncated a partial tail.
pub async fn find_end_of_wal<S: WalSource>(
    reader: &S,
    start_lsn: Lsn, // start reading WAL at this point; must point at record start_lsn.
    pg_version: PgMajorVersion,
) -> anyhow::Result<Lsn> {
    let mut result = start_lsn;
    let mut curr_lsn = start_lsn;
    let mut decoder = WalStreamDecoder::new(start_lsn, pg_version);

    // loop over WAL chunks
    loop {
        let (chunk_start, data) = match reader.chunk_at(curr_lsn).await {
            // no more WAL
            Err(_) => return Ok(result),
            Ok(chunk) => chunk,
        };
        let seg_offs = (curr_lsn.0 - chunk_start.0) as usize;
        curr_lsn = Lsn(chunk_start.0 + data.len() as u64);
        decoder.feed_bytes(&data[seg_offs..]);

        // advance result past all completely read records
        loop {
            match decoder.poll_decode() {
                Ok(Some(record)) => result = record.0,
                Err(_e) => {
                    // torn / partial tail: stop at the last complete record
                    return Ok(result);
                }
                Ok(None) => break, // need more data
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::WAL_SEGMENT_SIZE;
    use crate::XLOG_BLCKSZ;
    use crate::v18::wal_generator::{LogicalMessageGenerator, WalGenerator};

    /// In-memory WAL covering `[start, start + buf.len())`, served as one
    /// chunk. A segment-aligned `start` makes the buffer open with a long
    /// page header, so `find_anchor` exercises both header sizes.
    struct MemWal {
        start: Lsn,
        buf: Vec<u8>,
    }

    impl WalSource for MemWal {
        fn seg(&self) -> u64 {
            WAL_SEGMENT_SIZE as u64
        }
        async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<(Lsn, Vec<u8>)> {
            let end = self.start.0 + self.buf.len() as u64;
            if lsn.0 < self.start.0 || lsn.0 >= end {
                bail!(
                    "WAL hole at {lsn} (mem buffer is [{}, {}))",
                    self.start,
                    Lsn(end)
                );
            }
            Ok((self.start, self.buf.clone()))
        }
    }

    const START: Lsn = Lsn(WAL_SEGMENT_SIZE as u64);

    /// Generate `count` uniform logical-message records from `START`,
    /// returning the bytes and the true record-start boundaries.
    fn gen_wal(count: usize, value_size: usize) -> (Vec<u8>, Vec<Lsn>) {
        let mut g = WalGenerator::new(LogicalMessageGenerator::new(c"t", &[]), START);
        let mut buf = Vec::new();
        let mut starts = Vec::new();
        for _ in 0..count {
            let (lsn, bytes) = g.append_logical_message(c"t", &vec![7u8; value_size]);
            starts.push(lsn);
            buf.extend_from_slice(&bytes);
        }
        (buf, starts)
    }

    /// Clean WAL: scanning from the start reaches the end of the last
    /// complete record (no truncation).
    #[tokio::test]
    async fn clean_wal_reaches_end() {
        let (buf, starts) = gen_wal(4000, 16);
        // Trim the buffer to a record boundary so the end is well-defined.
        let max = *starts.last().unwrap();
        let src = MemWal {
            start: START,
            buf: buf[..(max.0 - START.0) as usize].to_vec(),
        };
        let got = find_end_of_wal(&src, START, PgMajorVersion::PG18)
            .await
            .unwrap();
        assert_eq!(got, max, "clean WAL must scan through to the end boundary");
    }

    /// Torn tail: corrupt the last page header's magic so the decoder
    /// errors when it reaches that page. `find_end_of_wal` must return the
    /// last complete record boundary *before* the torn page — strictly
    /// below the raw end offset, and a real boundary.
    #[tokio::test]
    async fn torn_tail_stops_at_last_valid_boundary() {
        let (mut buf, starts) = gen_wal(4000, 16);
        // Tear the page header that begins the last full 8 KiB page.
        let last_page = (buf.len() - 1) / XLOG_BLCKSZ * XLOG_BLCKSZ;
        assert!(last_page > 0, "test needs multi-page WAL");
        buf[last_page] = 0; // xlp_magic byte 0
        buf[last_page + 1] = 0; // xlp_magic byte 1

        let torn_page_lsn = START.0 + last_page as u64;
        let src = MemWal { start: START, buf };

        let got = find_end_of_wal(&src, START, PgMajorVersion::PG18)
            .await
            .unwrap();
        assert!(
            got < Lsn(torn_page_lsn),
            "must stop below the torn page {} (got {got})",
            Lsn(torn_page_lsn)
        );
        assert!(
            starts.contains(&got),
            "stop point {got} must be a real record boundary"
        );
    }

    /// XLOG_SWITCH (`pg_switch_wal()`) zero-pads the rest of the segment,
    /// so the pages after it have `xlp_magic == 0` and `xlp_info == 0`
    /// (the contrecord bit clear). `find_anchor` must NOT accept such a
    /// zeroed page as a record anchor: it has to walk back to the last
    /// *real* WAL page. Regression test for #181 — accepting the padding
    /// page seeded the decoder inside the zeros, which failed with
    /// "invalid xlog page header" and wedged the safekeeper on restart.
    #[tokio::test]
    async fn find_anchor_skips_xlog_switch_padding() {
        let (buf, _starts) = gen_wal(4000, 16);
        // Truncate to a page boundary, then append zeroed pages to mimic
        // the segment padding an XLOG_SWITCH leaves behind.
        let real_end = (buf.len() - 1) / XLOG_BLCKSZ * XLOG_BLCKSZ;
        assert!(real_end > 0, "test needs multi-page WAL");
        let mut padded = buf[..real_end].to_vec();
        padded.resize(real_end + 3 * XLOG_BLCKSZ, 0);
        let src = MemWal {
            start: START,
            buf: padded,
        };

        // Target lands inside the zeroed padding — exactly the persisted
        // "max byte offset" an XLOG_SWITCH produces.
        let padding_start = Lsn(START.0 + real_end as u64);
        let target = Lsn(padding_start.0 + XLOG_BLCKSZ as u64 + 100);
        let anchor = find_anchor(&src, target, Lsn(0)).await.unwrap();

        assert!(
            anchor < padding_start,
            "anchor {anchor} must fall below the zeroed padding at {padding_start}"
        );
        // The anchor must be a real WAL page that begins a fresh record,
        // never a zeroed padding page.
        let hdr = src
            .read_exact(anchor, page_header_size(anchor, src.seg()))
            .await
            .unwrap();
        let magic = u16::from_le_bytes([hdr[0], hdr[1]]);
        let info = u16::from_le_bytes([hdr[2], hdr[3]]);
        assert_eq!(
            magic, XLOG_PAGE_MAGIC as u16,
            "anchor must land on a real WAL page, not zeroed padding"
        );
        assert_eq!(
            info & XLP_FIRST_IS_CONTRECORD,
            0,
            "anchor page must begin a fresh record"
        );
    }

    /// The persisted WAL can begin at a mid-page record boundary (the
    /// safekeeper's `timeline_start_lsn`), and a single record can span from
    /// there across several pages — e.g. wal_craft's segment-filling padding
    /// record right before an XLOG_SWITCH. Then every page *above* the start
    /// is a contrecord continuation, so the walk-back finds no fresh-record
    /// page and, page-aligning down, probes the page *below* the store — a
    /// hole, since the start page's pre-`floor` bytes were never persisted.
    /// Without a floor the scan errors there and the caller falls back to the
    /// raw end offset, leaving `write_lsn` short of the post-switch segment
    /// and wedging the reader. Passing the start as `floor` must stop the
    /// walk and anchor at that boundary. Regression test for #181.
    #[tokio::test]
    async fn find_anchor_stops_at_midpage_floor() {
        // One record big enough to span several pages, so the pages above the
        // start are all contrecord continuations.
        let (buf, _starts) = gen_wal(1, 3 * XLOG_BLCKSZ);
        // The first record begins just past START's long page header — a
        // mid-page boundary, exactly like a real `timeline_start_lsn`.
        let hdr = page_header_size(START, WAL_SEGMENT_SIZE as u64);
        let floor = Lsn(START.0 + hdr as u64);
        assert_ne!(floor.0 % XLOG_BLCKSZ as u64, 0, "floor must be mid-page");

        // Serve the WAL as if the store begins at `floor`: the start page's
        // header bytes [START, floor) are absent — a hole below `floor`.
        let src = MemWal {
            start: floor,
            buf: buf[hdr..].to_vec(),
        };
        // Target lands in a continuation page of the big record.
        let target = Lsn(floor.0 + 2 * XLOG_BLCKSZ as u64);

        // No floor: the walk overshoots below the store and errors.
        assert!(
            find_anchor(&src, target, Lsn(0)).await.is_err(),
            "no-floor walk must overshoot the mid-page start and fail"
        );
        // With the floor: anchor exactly at the mid-page WAL start.
        let anchor = find_anchor(&src, target, floor).await.unwrap();
        assert_eq!(anchor, floor, "must anchor at the mid-page WAL start");
    }
}
