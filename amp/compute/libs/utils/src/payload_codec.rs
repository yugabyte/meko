//! Self-describing compression envelope for the `BYTEA` payload columns the
//! pageserver stores in YB.
//!
//! # Why
//!
//! The pageserver writes 8 KiB Postgres pages (and WAL-record deltas) into
//! `amp_storage.pages.payload`, and equivalent blobs into
//! `amp_storage.slru_pages.payload` and `amp_storage.meta.payload`. None of them
//! is compressed by YSQL: YB stores values inline in DocDB rather than through
//! TOAST, so what the Rust layer binds is what DocDB stores verbatim.
//! Compressing here shrinks the Raft log, the Raft replication RPCs (×RF), the
//! memtable, and the byte volume every compaction has to move. DocDB's own SST
//! block compression only covers the last of those, and only after the bytes
//! have already been through Raft.
//!
//! Postgres pages compress unusually well for their size — the zeroed
//! free-space hole between `pd_lower`/`pd_upper`, the aligned line-pointer
//! array, and tuple padding are all trivially compressible.
//!
//! # Format
//!
//! ```text
//! byte 0    : codec tag
//! bytes 1.. : codec-defined body
//!
//! tag 0x00 (Raw) : payload verbatim, no length
//! tag 0x01 (Lz4) : 4-byte LE uncompressed size, then an LZ4 *block*
//! ```
//!
//! The envelope is deliberately self-describing rather than deriving the
//! decompressed length from out-of-band state. `amp_storage.pages` images are
//! `BLCKSZ` today, for instance, but coupling the storage format to that would
//! make a schema invariant load-bearing for decode correctness. Five bytes on
//! compressed rows is cheaper than that coupling.
//!
//! # Read/write asymmetry
//!
//! [`decode`] accepts *any* known tag regardless of how the caller is
//! configured to write; [`encode`] emits the caller's configured codec. So
//! flipping a writer's codec (or restarting with a different one) needs no
//! migration and no dual-write window: new rows land in the new format and
//! old rows stay readable, including within a single page's delta chain.
//! Selecting `Raw` is a write-side choice only — it never rewrites or
//! decompresses what is already stored.
//!
//! An unrecognised tag is a hard error, never a fall back to "treat it as
//! raw bytes". A binary that silently misread a future codec's body as page
//! contents would surface as walredo corruption far from the cause.

use anyhow::{Context, Result, bail};
use bytes::Bytes;

/// Envelope tag byte: payload stored verbatim.
const TAG_RAW: u8 = 0x00;
/// Envelope tag byte: 4-byte LE uncompressed length, then an LZ4 block.
const TAG_LZ4: u8 = 0x01;

/// Bytes of envelope overhead for [`TAG_RAW`] (the tag itself).
const RAW_OVERHEAD: usize = 1;
/// Bytes of envelope overhead for [`TAG_LZ4`] (tag + 4-byte LE length).
const LZ4_OVERHEAD: usize = 5;

/// Payloads below this stay [`TAG_RAW`] whatever the configured codec.
///
/// Deltas are frequently a handful of bytes — a `ClearVisibilityMapFlags`
/// record is one — and LZ4 on those spends CPU to grow the row. The whole
/// win is on the 8 KiB image path, which this threshold leaves untouched.
pub const MIN_COMPRESS_BYTES: usize = 256;

/// Which codec [`encode`] should emit. Decode always handles every variant.
///
/// Mirrors the wire tags above, but kept as a separate type so callers
/// configure intent (`Lz4`) rather than a magic byte.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Codec {
    /// Wrap in the envelope but never compress.
    Raw,
    /// Compress with LZ4 when it pays; fall back to [`Codec::Raw`] per-row.
    Lz4,
}

/// Why [`encode`] stored a payload raw despite a compressing [`Codec`].
///
/// Returned so callers can distinguish "not worth trying" from "tried and
/// lost" in metrics; the two have very different tuning implications.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RawReason {
    /// The caller asked for [`Codec::Raw`].
    Configured,
    /// Shorter than [`MIN_COMPRESS_BYTES`].
    TooSmall,
    /// Compressed to at least its original size.
    Incompressible,
}

/// What [`encode`] did, for metrics. `logical_len` is the caller's input
/// length; the stored length is the returned buffer's.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    /// Stored compressed under the named codec.
    Compressed(Codec),
    /// Stored verbatim, for this reason.
    Raw(RawReason),
}

/// Wrap `plain` in the envelope, compressing with `codec` when that pays.
///
/// Always returns at least a tag byte, so the result is never empty even for
/// an empty payload — which keeps the column's `NOT NULL` meaningful and
/// lets [`decode`] reject a truly empty value as corruption.
pub fn encode(plain: &[u8], codec: Codec) -> (Bytes, Outcome) {
    let raw = |reason| {
        let mut out = Vec::with_capacity(RAW_OVERHEAD + plain.len());
        out.push(TAG_RAW);
        out.extend_from_slice(plain);
        (Bytes::from(out), Outcome::Raw(reason))
    };

    match codec {
        Codec::Raw => return raw(RawReason::Configured),
        Codec::Lz4 if plain.len() < MIN_COMPRESS_BYTES => return raw(RawReason::TooSmall),
        Codec::Lz4 => {}
    }

    // Build the envelope in one allocation: tag, LE length, then compress
    // straight into the tail. `compress_into` needs the destination sized to
    // the worst case, which for LZ4 can exceed the input.
    let bound = lz4_flex::block::get_maximum_output_size(plain.len());
    let mut out = vec![0u8; LZ4_OVERHEAD + bound];
    out[0] = TAG_LZ4;
    out[1..LZ4_OVERHEAD].copy_from_slice(&(plain.len() as u32).to_le_bytes());

    match lz4_flex::block::compress_into(plain, &mut out[LZ4_OVERHEAD..]) {
        Ok(n) if LZ4_OVERHEAD + n < RAW_OVERHEAD + plain.len() => {
            out.truncate(LZ4_OVERHEAD + n);
            // `truncate` moves `len` but never `capacity`, and
            // `Bytes::from(Vec<u8>)` only takes its no-copy
            // `into_boxed_slice` path when `len == capacity` — otherwise it
            // keeps the vector's raw parts, pinning the WORST-CASE
            // allocation for the lifetime of the `Bytes` and every clone.
            // A caller encodes a whole flush before writing any of it, so
            // without this the buffers stay as large as if nothing had been
            // compressed. Costs one realloc + a copy of the (smaller)
            // compressed bytes, against an LZ4 pass we just paid for.
            out.shrink_to_fit();
            debug_assert_eq!(
                out.len(),
                out.capacity(),
                "Bytes::from must get len == capacity or it retains the \
                 worst-case LZ4 buffer"
            );
            (Bytes::from(out), Outcome::Compressed(Codec::Lz4))
        }
        // Either it grew, or the encoder refused. Both mean "store raw" —
        // there is no payload LZ4 can reject that we cannot store verbatim.
        Ok(_) => raw(RawReason::Incompressible),
        Err(_) => raw(RawReason::Incompressible),
    }
}

/// Unwrap an envelope produced by [`encode`], whatever codec wrote it.
///
/// [`Codec::Raw`] payloads are returned as a zero-copy slice of `stored`;
/// compressed ones allocate exactly the decoded length.
pub fn decode(stored: &Bytes) -> Result<Bytes> {
    let (&tag, body) = stored
        .split_first()
        .context("payload envelope is empty (expected at least a codec tag)")?;

    match tag {
        TAG_RAW => Ok(stored.slice(RAW_OVERHEAD..)),
        TAG_LZ4 => {
            let len_bytes: [u8; 4] = body
                .get(..4)
                .context("LZ4 payload envelope truncated before its length prefix")?
                .try_into()
                .expect("slice of exactly 4 bytes");
            let logical = u32::from_le_bytes(len_bytes) as usize;

            // Decompress into an exactly-sized buffer rather than going
            // through `decompress_size_prepended`: the length is already in
            // hand, and `decompress_into` lets the read path avoid the
            // grow-and-copy on a per-row-per-chain-step hot path.
            let mut out = vec![0u8; logical];
            let n = lz4_flex::block::decompress_into(&body[4..], &mut out)
                .context("LZ4 payload decompression failed")?;
            if n != logical {
                bail!("LZ4 payload decompressed to {n} bytes, envelope declared {logical}");
            }
            Ok(Bytes::from(out))
        }
        other => bail!(
            "unknown payload codec tag 0x{other:02x} \
             (this build supports 0x{TAG_RAW:02x}=raw, 0x{TAG_LZ4:02x}=lz4); \
             the row was written by a newer build, or by one that bypassed \
             the codec entirely"
        ),
    }
}

/// The codec a stored envelope was written with, for diagnostics.
///
/// Returns the tag's name rather than a [`Codec`] so unknown tags can be
/// reported instead of erroring — inspection tooling wants to *show* an
/// unreadable row, not fail on it.
pub fn stored_codec_name(stored: &[u8]) -> &'static str {
    match stored.first() {
        Some(&TAG_RAW) => "raw",
        Some(&TAG_LZ4) => "lz4",
        Some(_) => "unknown",
        None => "empty",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Stand-in for a real heap page: a header-ish prefix, a zeroed
    /// free-space hole, and some repetitive tuple bytes. Compressible the
    /// way an actual `BLCKSZ` page is, unlike a single repeated byte.
    fn pagelike() -> Vec<u8> {
        let mut p = vec![0u8; 8192];
        for (i, b) in p[..64].iter_mut().enumerate() {
            *b = i as u8;
        }
        for (i, b) in p[6000..].iter_mut().enumerate() {
            *b = (i % 17) as u8;
        }
        p
    }

    /// Not `rand`: the test must be deterministic, and a counter-fed xorshift
    /// is incompressible enough to exercise the fallback.
    fn incompressible(len: usize) -> Vec<u8> {
        let mut s = 0x2545_F491_4F6C_DD1Du64;
        (0..len)
            .map(|_| {
                s ^= s << 13;
                s ^= s >> 7;
                s ^= s << 17;
                (s >> 24) as u8
            })
            .collect()
    }

    fn roundtrip(plain: &[u8], codec: Codec) -> Outcome {
        let (stored, outcome) = encode(plain, codec);
        let back = decode(&stored).expect("decode");
        assert_eq!(&back[..], plain, "roundtrip mismatch under {codec:?}");
        outcome
    }

    #[test]
    fn pagelike_payload_compresses_and_roundtrips() {
        let plain = pagelike();
        let outcome = roundtrip(&plain, Codec::Lz4);
        assert_eq!(outcome, Outcome::Compressed(Codec::Lz4));

        let (stored, _) = encode(&plain, Codec::Lz4);
        assert_eq!(stored[0], TAG_LZ4);
        assert!(
            stored.len() < plain.len() / 2,
            "expected >2x on a page-like payload, got {} -> {}",
            plain.len(),
            stored.len()
        );
    }

    #[test]
    fn incompressible_payload_falls_back_to_raw() {
        let plain = incompressible(8192);
        let outcome = roundtrip(&plain, Codec::Lz4);
        assert_eq!(outcome, Outcome::Raw(RawReason::Incompressible));
        // The fallback must never store more than tag + payload.
        let (stored, _) = encode(&plain, Codec::Lz4);
        assert_eq!(stored.len(), plain.len() + RAW_OVERHEAD);
    }

    #[test]
    fn sub_threshold_payload_stays_raw() {
        // A 1-byte delta, the ClearVisibilityMapFlags case.
        let outcome = roundtrip(&[0x01], Codec::Lz4);
        assert_eq!(outcome, Outcome::Raw(RawReason::TooSmall));

        let plain = vec![0u8; MIN_COMPRESS_BYTES - 1];
        assert_eq!(
            roundtrip(&plain, Codec::Lz4),
            Outcome::Raw(RawReason::TooSmall)
        );
        // Exactly at the threshold it is eligible, and zeros compress.
        let plain = vec![0u8; MIN_COMPRESS_BYTES];
        assert_eq!(
            roundtrip(&plain, Codec::Lz4),
            Outcome::Compressed(Codec::Lz4)
        );
    }

    #[test]
    fn raw_codec_never_compresses() {
        let plain = pagelike();
        let outcome = roundtrip(&plain, Codec::Raw);
        assert_eq!(outcome, Outcome::Raw(RawReason::Configured));
        let (stored, _) = encode(&plain, Codec::Raw);
        assert_eq!(stored[0], TAG_RAW);
        assert_eq!(stored.len(), plain.len() + RAW_OVERHEAD);
    }

    #[test]
    fn empty_payload_roundtrips() {
        assert_eq!(
            roundtrip(&[], Codec::Lz4),
            Outcome::Raw(RawReason::TooSmall)
        );
        assert_eq!(
            roundtrip(&[], Codec::Raw),
            Outcome::Raw(RawReason::Configured)
        );
    }

    /// The point of the tag: rows written under one codec stay readable
    /// after the writer is reconfigured to another.
    #[test]
    fn mixed_codecs_decode_with_one_reader() {
        let plain = pagelike();
        let (lz4_row, _) = encode(&plain, Codec::Lz4);
        let (raw_row, _) = encode(&plain, Codec::Raw);
        assert_ne!(lz4_row, raw_row);
        assert_eq!(&decode(&lz4_row).unwrap()[..], &plain[..]);
        assert_eq!(&decode(&raw_row).unwrap()[..], &plain[..]);
    }

    #[test]
    fn unknown_tag_is_rejected() {
        let err = decode(&Bytes::from_static(&[0x7f, 0xde, 0xad])).unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("unknown payload codec tag 0x7f"), "{msg}");
    }

    #[test]
    fn empty_envelope_is_rejected() {
        assert!(decode(&Bytes::new()).is_err());
    }

    #[test]
    fn truncated_lz4_envelope_is_rejected() {
        // Tag present, length prefix cut short.
        assert!(decode(&Bytes::from_static(&[TAG_LZ4, 0x00, 0x20])).is_err());

        // Full header, corrupt body.
        let plain = pagelike();
        let (stored, _) = encode(&plain, Codec::Lz4);
        let truncated = stored.slice(..stored.len() - 8);
        assert!(decode(&truncated).is_err());
    }

    /// A row whose declared length disagrees with its body must fail rather
    /// than hand a short buffer to walredo.
    #[test]
    fn length_prefix_mismatch_is_rejected() {
        let plain = pagelike();
        let (stored, _) = encode(&plain, Codec::Lz4);
        let mut tampered = stored.to_vec();
        tampered[1..5].copy_from_slice(&(plain.len() as u32 + 64).to_le_bytes());
        assert!(decode(&Bytes::from(tampered)).is_err());
    }

    #[test]
    fn stored_codec_name_reports_tags() {
        let plain = pagelike();
        assert_eq!(stored_codec_name(&encode(&plain, Codec::Lz4).0), "lz4");
        assert_eq!(stored_codec_name(&encode(&plain, Codec::Raw).0), "raw");
        assert_eq!(stored_codec_name(&[0x7f]), "unknown");
        assert_eq!(stored_codec_name(&[]), "empty");
    }
}
