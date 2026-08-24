//! Find the last valid WAL **record boundary** at or below a byte
//! position, by replaying the real [`WalStreamDecoder`] over raw WAL
//! bytes.
//!
//! Both the pageserver (`reset-consistent-lsn`) and the safekeeper
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
//!
//! [`WalStreamTracker`] answers the same question for a stream being
//! written, from the bytes in hand rather than from the rows. It lives here so
//! both sides run the one decoder: a writer and a restart scan that disagree on
//! where a record ends leave the branch truncating to a point the writer never
//! called a boundary.

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
    /// bytes, or `None` when no chunk covers it — the WAL ends here.
    ///
    /// `Err` means the *read* failed. The bytes may well be there; this
    /// attempt could not see them, so no caller may conclude anything about
    /// where the WAL ends from it (#756).
    async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>>;

    /// A contiguous run starting with the chunk covering `lsn`, ascending: every
    /// chunk begins where the one before it ended, and the run stops at the first
    /// byte the source does not hold. Empty means nothing covers `lsn` —
    /// `chunk_at`'s `None`, read the same way (#756). How many to return is the
    /// implementation's choice; contiguity is not, which is what the name is for.
    ///
    /// Batched because a `wals` read is a round trip, so the one-chunk-per-call
    /// scan cost one query per chunk — thousands on OLTP WAL (#929).
    ///
    /// The default returns the covering chunk alone, which satisfies the contract
    /// (a run of one) without batching. It is a default so that adding this did
    /// not break every implementor, and nothing in production takes it:
    /// [`find_end_of_wal`] is the only caller and `YbWalSource` overrides it.
    /// `decode_forward`, the other forward scan, still reads a chunk at a time.
    async fn contiguous_chunks_from(&self, lsn: Lsn) -> anyhow::Result<Vec<(Lsn, Vec<u8>)>> {
        Ok(self.chunk_at(lsn).await?.into_iter().collect())
    }

    /// Read exactly `len` bytes starting at `lsn`, assembling across
    /// chunk (and segment) boundaries. Errors on a hole: the caller asked
    /// for a span it already expects to be there.
    async fn read_exact(&self, lsn: Lsn, len: usize) -> anyhow::Result<Vec<u8>> {
        let mut out = Vec::with_capacity(len);
        let mut pos = lsn;
        while out.len() < len {
            let (chunk_start, data) = self
                .chunk_at(pos)
                .await?
                .with_context(|| format!("WAL hole at {pos}: no covering chunk"))?;
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
/// `target` is **inclusive** — the last byte of interest, not a one-past-the-end
/// offset. A caller holding an exclusive end passes `end - 1`; passing the end
/// itself probes a page that holds no bytes and errors on the hole (#678).
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
/// `floor` is the lowest record boundary the caller knows still *exists* — the
/// oldest surviving one, not the oldest the branch ever had, since retention
/// drops whole segments from underneath (#788). `Lsn(0)` if the caller has none.
/// The backward walk stops there: probing below `floor` reads a hole (the store no
/// longer holds those bytes), and when a single record spans all the way back
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
        // Feed the next chunk of WAL bytes. A failed read is not a stop
        // reason — it is not evidence about the WAL's shape at all.
        match reader
            .chunk_at(pos)
            .await
            .with_context(|| format!("reading the WAL chunk at {pos}"))?
        {
            Some((chunk_start, data)) => {
                let from = (pos.0 - chunk_start.0) as usize;
                decoder.feed_bytes(&data[from..]);
                pos = Lsn(chunk_start.0 + data.len() as u64);
            }
            None => {
                stop = Some(format!("WAL ended at {pos} before reaching target"));
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
// A port of upstream Neon's `postgres_ffi::xlog_utils::find_end_of_wal` — the
// local-segment-file scanner the safekeeper used to recompute flush_lsn on
// restart before WAL moved to YB. The decode loop is identical; only the byte
// source differs: WAL chunks from a `WalSource` instead of `read`s from segment
// files. A torn trailing page makes the decoder error, and `result` (the end of
// the last complete record) is returned — exactly as upstream truncated a
// partial tail.
//
// Upstream could also treat a failed *read* as the end of the WAL, because a
// read error against a local segment file really did mean there were no more
// segments. Against a `wals` table it does not: the rows may be there and
// durable while a query fails. Hence the `Option` in `chunk_at` — only `None`
// ends the scan, and a read failure propagates (#756). Returning a short
// boundary instead would make the caller DELETE durable WAL above it.
pub async fn find_end_of_wal<S: WalSource>(
    reader: &S,
    start_lsn: Lsn, // start reading WAL at this point; must point at record start_lsn.
    pg_version: PgMajorVersion,
) -> anyhow::Result<Lsn> {
    let mut result = start_lsn;
    let mut curr_lsn = start_lsn;
    let mut decoder = WalStreamDecoder::new(start_lsn, pg_version);

    // loop over batches of WAL chunks
    loop {
        let chunks = reader
            .contiguous_chunks_from(curr_lsn)
            .await
            .with_context(|| format!("end-of-WAL scan: reading the chunks at {curr_lsn}"))?;
        if chunks.is_empty() {
            return Ok(result); // no more WAL
        }
        for (chunk_start, data) in chunks {
            // The source broke its contract; returning `result` here, as the holes
            // below do, would read as end-of-WAL and truncate durable rows (#756).
            if chunk_start > curr_lsn || curr_lsn.0 - chunk_start.0 >= data.len() as u64 {
                bail!(
                    "end-of-WAL scan: chunk at {chunk_start} (+{} bytes) does not cover {curr_lsn}",
                    data.len()
                );
            }
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
}

/// End of WAL for a stream being **written**, tracked as the bytes arrive.
///
/// The scans above rediscover the boundary by replaying the rows; this reports
/// it while the writer still holds the bytes, so the write path never pays a
/// read to learn where its own last record ended.
pub struct WalStreamTracker {
    decoder: WalStreamDecoder,
    /// Moves backwards on `reset`, so this is the most recent boundary, not the
    /// highest ever seen.
    record_end: Lsn,
    /// Tracked by arithmetic rather than read back from `decoder`, whose
    /// `available()` drops below the bytes handed in when `poll_decode` consumes a
    /// page header and then rejects it.
    byte_end: Lsn,
}

impl WalStreamTracker {
    /// `start_lsn` must be a record boundary — the rehydrated end of WAL, or
    /// `timeline_start_lsn` on a fresh branch. It is the boundary until bytes
    /// arrive, which is what an empty stream should report.
    pub fn new(start_lsn: Lsn, pg_version: PgMajorVersion) -> Self {
        Self {
            decoder: WalStreamDecoder::new(start_lsn, pg_version),
            record_end: start_lsn,
            byte_end: start_lsn,
        }
    }

    /// The last boundary known: the end of the most recent complete record fed,
    /// or the `start_lsn` the stream was seeded at if nothing has been.
    pub fn record_end(&self) -> Lsn {
        self.record_end
    }

    /// End of the bytes fed so far — a byte position, mid-record as often as not,
    /// which is what [`Self::record_end`] lags. Where the next feed must start, and
    /// exact even when the feed that delivered them failed to decode.
    pub fn byte_end(&self) -> Lsn {
        self.byte_end
    }

    /// Extend the stream with the bytes at `startpos` and return the boundary they
    /// establish. `startpos` is a byte position, mid-record as often as not — only
    /// [`Self::new`] and [`Self::reset`] take a boundary.
    ///
    /// Errors if `startpos` does not continue where the last feed ended, because
    /// both ways of getting there are caller bugs: bytes were skipped, leaving a
    /// hole, or the stream moved back without a [`Self::reset`] to say so. Papering
    /// over either would report a boundary for a stream that does not exist.
    pub fn feed(&mut self, startpos: Lsn, buf: &[u8]) -> anyhow::Result<Lsn> {
        if self.byte_end != startpos {
            bail!(
                "WAL stream feed at {startpos} is not contiguous with {}; \
                 move the stream with reset()",
                self.byte_end,
            );
        }
        self.decoder.feed_bytes(buf);
        // Before the decode, so a torn record still leaves the byte end exact.
        self.byte_end = Lsn(startpos.0 + buf.len() as u64);
        while let Some((lsn, _rec)) = self.decoder.poll_decode()? {
            self.record_end = lsn;
        }
        Ok(self.record_end)
    }

    /// Move the stream to `lsn` — the only way to move it — discarding decoder
    /// state: a truncation deleted the bytes above it, so nothing half-decoded from
    /// them survives. `lsn` must be a record boundary, as in [`Self::new`].
    pub fn reset(&mut self, lsn: Lsn) {
        self.decoder = WalStreamDecoder::new(lsn, self.decoder.pg_version);
        self.record_end = lsn;
        self.byte_end = lsn;
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
        async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>> {
            let end = self.start.0 + self.buf.len() as u64;
            if lsn.0 < self.start.0 || lsn.0 >= end {
                return Ok(None);
            }
            Ok(Some((self.start, self.buf.clone())))
        }
    }

    /// The same WAL cut into fixed-size chunks. `per_fetch: 1` is the trait
    /// default's one-chunk-per-call behaviour.
    struct ChunkedWal {
        start: Lsn,
        buf: Vec<u8>,
        chunk: usize,
        per_fetch: usize,
    }

    impl ChunkedWal {
        fn at(&self, lsn: Lsn) -> Option<(Lsn, Vec<u8>)> {
            let end = self.start.0 + self.buf.len() as u64;
            if lsn.0 < self.start.0 || lsn.0 >= end {
                return None;
            }
            let idx = ((lsn.0 - self.start.0) as usize) / self.chunk;
            let from = idx * self.chunk;
            let to = min(from + self.chunk, self.buf.len());
            Some((Lsn(self.start.0 + from as u64), self.buf[from..to].to_vec()))
        }
    }

    impl WalSource for ChunkedWal {
        fn seg(&self) -> u64 {
            WAL_SEGMENT_SIZE as u64
        }
        async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>> {
            Ok(self.at(lsn))
        }
        async fn contiguous_chunks_from(&self, lsn: Lsn) -> anyhow::Result<Vec<(Lsn, Vec<u8>)>> {
            let mut out = Vec::new();
            let mut pos = lsn;
            while out.len() < self.per_fetch {
                match self.at(pos) {
                    Some((s, d)) => {
                        pos = Lsn(s.0 + d.len() as u64);
                        out.push((s, d));
                    }
                    None => break,
                }
            }
            Ok(out)
        }
    }

    const START: Lsn = Lsn(WAL_SEGMENT_SIZE as u64);

    /// A batched `contiguous_chunks_from` must reach the same end of WAL as the
    /// one-chunk-per-call default, whatever the chunk size (#929).
    #[tokio::test]
    async fn bulk_and_single_chunk_reads_agree() {
        let (buf, starts) = gen_wal(400, 16);
        let end = Lsn(START.0 + buf.len() as u64);
        // `per_fetch: 3` with tiny chunks makes the scan cross many batches, so a
        // short batch must not read as the end of WAL.
        for (chunk, per_fetch) in [(64usize, 3usize), (512, 64), (4096, 64), (1 << 20, 64)] {
            let single = ChunkedWal {
                start: START,
                buf: buf.clone(),
                chunk,
                per_fetch: 1,
            };
            let bulk = ChunkedWal {
                start: START,
                buf: buf.clone(),
                chunk,
                per_fetch,
            };
            let a = find_end_of_wal(&single, starts[10], PgMajorVersion::PG18)
                .await
                .expect("single");
            let b = find_end_of_wal(&bulk, starts[10], PgMajorVersion::PG18)
                .await
                .expect("bulk");
            assert_eq!(a, b, "chunk size {chunk}: bulk and single disagree");
            assert_eq!(a, end, "chunk size {chunk}: wrong end of WAL");
        }
    }

    /// The batch is not trusted to be contiguous: the caller stops at the first
    /// chunk that does not cover where it is, which is the hole `chunk_at`'s
    /// `None` used to report. Seeding past the WAL must not walk off the end.
    #[tokio::test]
    async fn bulk_read_stops_at_a_hole() {
        let (buf, starts) = gen_wal(200, 16);
        let cut = starts[100];
        // Serve only the first half; everything at or above `cut` is absent.
        let src = ChunkedWal {
            start: START,
            buf: buf[..(cut.0 - START.0) as usize].to_vec(),
            chunk: 512,
            per_fetch: 64,
        };
        let got = find_end_of_wal(&src, starts[5], PgMajorVersion::PG18)
            .await
            .expect("scan");
        assert_eq!(
            got, cut,
            "the scan must stop at the last complete record before the hole"
        );
    }

    /// A batch that does not cover where the scan is breaks the contract. Reading
    /// it as end-of-WAL would truncate durable WAL (#756), so it must fail.
    #[tokio::test]
    async fn bulk_read_rejects_a_batch_that_does_not_cover() {
        let (buf, starts) = gen_wal(200, 16);
        struct Skipping(ChunkedWal);
        impl WalSource for Skipping {
            fn seg(&self) -> u64 {
                self.0.seg()
            }
            async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>> {
                self.0.chunk_at(lsn).await
            }
            async fn contiguous_chunks_from(
                &self,
                lsn: Lsn,
            ) -> anyhow::Result<Vec<(Lsn, Vec<u8>)>> {
                // Skip the covering chunk and hand back the one after it.
                let skipped = Lsn(lsn.0 + self.0.chunk as u64);
                self.0.contiguous_chunks_from(skipped).await
            }
        }
        let src = Skipping(ChunkedWal {
            start: START,
            buf,
            chunk: 512,
            per_fetch: 4,
        });
        let err = find_end_of_wal(&src, starts[0], PgMajorVersion::PG18)
            .await
            .expect_err("a non-covering batch must fail the scan, not end it");
        assert!(
            format!("{err:#}").contains("does not cover"),
            "unexpected error: {err:#}"
        );
    }

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

    /// The writer-side tracker and the scan must name the same boundary for the
    /// same bytes, however the writer's pieces happen to fall.
    #[tokio::test]
    async fn tracker_agrees_with_the_scan() {
        let (buf, starts) = gen_wal(400, 16);
        let max = *starts.last().unwrap();
        let buf = buf[..(max.0 - START.0) as usize].to_vec();
        let scanned = find_end_of_wal(
            &MemWal {
                start: START,
                buf: buf.clone(),
            },
            START,
            PgMajorVersion::PG18,
        )
        .await
        .unwrap();

        for piece in [1usize, 7, 100, XLOG_BLCKSZ - 1, XLOG_BLCKSZ, 1 << 16] {
            let mut t = WalStreamTracker::new(START, PgMajorVersion::PG18);
            let mut pos = START;
            for chunk in buf.chunks(piece) {
                t.feed(pos, chunk).unwrap();
                pos = Lsn(pos.0 + chunk.len() as u64);
            }
            assert_eq!(
                t.record_end(),
                scanned,
                "tracker fed in {piece}-byte pieces disagreed with the scan"
            );
        }
    }

    /// A trailing partial record leaves the boundary at the last complete one,
    /// and `reset` puts the stream back where a truncation left it.
    #[tokio::test]
    async fn tracker_holds_at_the_last_complete_record() {
        let (buf, starts) = gen_wal(400, 16);
        let max = *starts.last().unwrap();
        let torn = (max.0 - START.0) as usize + 8;

        let mut t = WalStreamTracker::new(START, PgMajorVersion::PG18);
        t.feed(START, &buf[..torn]).unwrap();
        assert_eq!(t.record_end(), max, "a partial tail is not a boundary");
        assert_eq!(
            t.byte_end(),
            Lsn(START.0 + torn as u64),
            "end() is the byte end, mid-record and eight bytes past the boundary"
        );

        let back = starts[42];
        t.reset(back);
        assert_eq!(t.record_end(), back);
        // And it decodes forward from there, rather than from the pre-reset state.
        t.feed(back, &buf[(back.0 - START.0) as usize..torn])
            .unwrap();
        assert_eq!(t.record_end(), max);
    }

    /// A feed whose bytes fail to decode still moves the byte end by exactly what it
    /// handed in. The decoder cannot be asked for this: `poll_decode` consumes a page
    /// header before rejecting it, so `available()` would come back short.
    #[test]
    fn tracker_byte_end_is_exact_after_a_decode_error() {
        let (mut buf, _starts) = gen_wal(4000, 16);
        // Tear the page header that begins the last full page, as the scan tests do.
        let last_page = (buf.len() - 1) / XLOG_BLCKSZ * XLOG_BLCKSZ;
        assert!(last_page > 0, "test needs multi-page WAL");
        buf[last_page] = 0;
        buf[last_page + 1] = 0;

        let mut t = WalStreamTracker::new(START, PgMajorVersion::PG18);
        t.feed(START, &buf)
            .expect_err("the torn page header must surface");
        assert_eq!(
            t.byte_end(),
            Lsn(START.0 + buf.len() as u64),
            "the byte end is what was handed in, not what decoded"
        );
        // And the next feed is accepted as contiguous with it.
        let more = vec![0u8; 16];
        let at = Lsn(START.0 + buf.len() as u64);
        t.feed(at, &more).expect_err("still torn, but not a gap");
        assert_eq!(t.byte_end(), Lsn(at.0 + more.len() as u64));
    }

    /// A feed that doesn't continue the stream is a caller bug either way it goes,
    /// and must not be absorbed: the boundary would then describe a stream with a
    /// hole in it, or one whose tail was replaced.
    #[test]
    fn tracker_rejects_a_non_contiguous_feed() {
        let (buf, starts) = gen_wal(400, 16);
        let max = *starts.last().unwrap();
        let end = (max.0 - START.0) as usize;
        let mut t = WalStreamTracker::new(START, PgMajorVersion::PG18);
        t.feed(START, &buf[..end]).unwrap();
        assert_eq!(t.record_end(), max);

        for (label, at) in [("backwards", starts[42]), ("forward", Lsn(max.0 + 4096))] {
            let err = t.feed(at, &buf[..8]).expect_err("must not be absorbed");
            assert!(
                format!("{err:#}").contains("not contiguous"),
                "{label}: unexpected error: {err:#}"
            );
            assert_eq!(
                t.record_end(),
                max,
                "{label}: a refused feed changes nothing"
            );
        }

        // And the stream carries on once reset says where it is now.
        let back = starts[42];
        t.reset(back);
        t.feed(back, &buf[(back.0 - START.0) as usize..end])
            .unwrap();
        assert_eq!(t.record_end(), max);
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

    /// Serves `buf` in `chunk`-sized pieces below `fail_at`, then fails the
    /// *read* — the shape of a failed `wals` query or a stored chunk this
    /// build can't decode, not the "no covering chunk" hole that legitimately
    /// ends a scan.
    struct FlakyWal {
        start: Lsn,
        buf: Vec<u8>,
        chunk: usize,
        fail_at: Lsn,
    }

    impl WalSource for FlakyWal {
        fn seg(&self) -> u64 {
            WAL_SEGMENT_SIZE as u64
        }
        async fn chunk_at(&self, lsn: Lsn) -> anyhow::Result<Option<(Lsn, Vec<u8>)>> {
            if lsn >= self.fail_at {
                bail!("injected wals read failure at {lsn}");
            }
            let served = min(self.buf.len(), (self.fail_at.0 - self.start.0) as usize);
            let off = (lsn.0 - self.start.0) as usize;
            let piece = off / self.chunk * self.chunk;
            let end = min(piece + self.chunk, served);
            Ok(Some((
                Lsn(self.start.0 + piece as u64),
                self.buf[piece..end].to_vec(),
            )))
        }
    }

    /// A failed read is not the end of the WAL (#756). `chunk_at` errors both
    /// when the WAL genuinely ended (no covering chunk — [`clean_wal_reaches_end`])
    /// and when the read itself failed, and this scan cannot tell them apart:
    /// it hands back the boundary it had reached. `YbWalStorage::new` trusts
    /// that short answer and DELETEs every `wals` row above it — rows whose
    /// commits the safekeeper already acked, and which no peer can restore.
    #[tokio::test]
    async fn read_failure_is_not_end_of_wal() {
        let (buf, starts) = gen_wal(4000, 16);
        let durable_end = *starts.last().unwrap();
        let buf = buf[..(durable_end.0 - START.0) as usize].to_vec();

        // Everything below the midpoint reads back fine, so a scan that takes
        // the failure for end-of-WAL stops around there while the WAL is
        // durable all the way up to `durable_end`.
        let fail_at = Lsn(START.0 + (buf.len() / 2) as u64);
        let src = FlakyWal {
            start: START,
            buf,
            chunk: XLOG_BLCKSZ,
            fail_at,
        };

        let err = match find_end_of_wal(&src, START, PgMajorVersion::PG18).await {
            Err(e) => e,
            Ok(end) => panic!(
                "a failed read was reported as end-of-WAL at {end}, {} bytes below the durable \
                 end {durable_end}; every wals row above {end} then gets DELETEd",
                durable_end.0 - end.0
            ),
        };
        assert!(
            format!("{err:#}").contains("injected wals read failure"),
            "the read failure must propagate, got: {err:#}"
        );
    }

    /// The same distinction one layer up. `decode_forward` reports a stop reason
    /// when the WAL runs out, and `reset-consistent-lsn` prints that as a note
    /// and still picks a boundary from the walk it got. A failed read must not
    /// look like that: the boundaries collected so far say nothing about where
    /// the WAL ends, so a `consistent_lsn` derived from them rewinds further
    /// than necessary on evidence that doesn't exist.
    #[tokio::test]
    async fn decode_forward_propagates_a_read_failure() {
        let (buf, starts) = gen_wal(4000, 16);
        let durable_end = *starts.last().unwrap();
        let buf = buf[..(durable_end.0 - START.0) as usize].to_vec();

        let fail_at = Lsn(START.0 + (buf.len() / 2) as u64);
        let src = FlakyWal {
            start: START,
            buf,
            chunk: XLOG_BLCKSZ,
            fail_at,
        };

        let err = match decode_forward(&src, START, durable_end, PgMajorVersion::PG18).await {
            Err(e) => e,
            Ok((boundaries, stop)) => panic!(
                "a failed read was reported as a stop reason ({stop:?}); the walk stopped at \
                 {:?}, below the durable end {durable_end}",
                boundaries.last()
            ),
        };
        assert!(
            format!("{err:#}").contains("injected wals read failure"),
            "the read failure must propagate, got: {err:#}"
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

    /// WAL that stops mid-record — the partial tail record must not be counted.
    #[tokio::test]
    async fn partial_tail_record_not_counted() {
        let (buf, starts) = gen_wal(4000, 16);
        let len = buf.len() / XLOG_BLCKSZ * XLOG_BLCKSZ;
        let cut = Lsn(START.0 + len as u64);
        // Boundaries are the record starts plus the last record's end.
        let full_end = Lsn(START.0 + buf.len() as u64);
        assert!(
            !starts.contains(&cut) && cut != full_end,
            "test needs a mid-record cut"
        );
        let src = MemWal {
            start: START,
            buf: buf[..len].to_vec(),
        };

        let anchor = find_anchor(&src, Lsn(cut.0 - 1), START).await.unwrap();
        assert!(anchor < cut, "anchor {anchor} must fall below {cut}");

        let end = find_end_of_wal(&src, anchor, PgMajorVersion::PG18)
            .await
            .unwrap();
        assert!(end < cut, "torn tail must be truncated below {cut}");
        assert!(
            starts.contains(&end),
            "end-of-WAL {end} must be a real record boundary"
        );
    }

    /// `target` is inclusive: an exclusive end must fail loudly rather than
    /// silently anchoring somewhere plausible, so callers convert (#678).
    #[tokio::test]
    async fn find_anchor_rejects_exclusive_page_aligned_end() {
        let (buf, _starts) = gen_wal(4000, 16);
        let len = buf.len() / XLOG_BLCKSZ * XLOG_BLCKSZ;
        let exclusive_end = Lsn(START.0 + len as u64);
        let src = MemWal {
            start: START,
            buf: buf[..len].to_vec(),
        };

        let err = find_anchor(&src, exclusive_end, START)
            .await
            .expect_err("an exclusive end has no bytes on its own page");
        assert!(
            format!("{err:#}").contains("WAL hole"),
            "expected a hole past end-of-WAL, got: {err:#}"
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
