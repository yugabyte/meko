//! Describes the legacy now hopefully no longer modified per-timeline metadata.
//!
//! It is stored in `index_part.json` managed by [`remote_timeline_client`]. For many tenants and
//! their timelines, this struct and its original serialization format is still needed because
//! they were written a long time ago.
//!
//! Instead of changing and adding versioning to this, just change [`IndexPart`] with soft json
//! versioning.
//!
//! To clean up this module we need to migrate all index_part.json files to a later version.
//! While doing this, we need to be mindful about s3 based recovery as well, so it might take
//! however long we keep the old versions to be able to delete the old code. After that, we can
//! remove everything else than [`TimelineMetadataBodyV2`], rename it as `TimelineMetadata` and
//! move it to `index.rs`. Before doing all of this, we need to keep the structures for backwards
//! compatibility.
//!
//! [`remote_timeline_client`]: super::remote_timeline_client
//! [`IndexPart`]: super::remote_timeline_client::index::IndexPart

use anyhow::ensure;
use postgres_ffi::PgMajorVersion;
use serde::{Deserialize, Serialize};
use utils::bin_ser::{BeSer, SerializeError};
use utils::id::BranchId;
use utils::lsn::Lsn;

/// Use special format number to enable backward compatibility.
const METADATA_FORMAT_VERSION: u16 = 4;

/// Previous supported format versions.
///
/// In practice, none of these should remain, all are [`METADATA_FORMAT_VERSION`], but confirming
/// that requires a scrubber run which is yet to be done.
const METADATA_OLD_FORMAT_VERSION: u16 = 3;

/// When the file existed on disk we assumed that a write of up to METADATA_MAX_SIZE bytes is atomic.
///
/// This is the same assumption that PostgreSQL makes with the control file,
///
/// see PG_CONTROL_MAX_SAFE_SIZE
const METADATA_MAX_SIZE: usize = 512;

/// Legacy metadata stored as a component of `index_part.json` per timeline.
///
/// Do not make new changes to this type or the module. In production, we have two different kinds
/// of serializations of this type: bincode and json. Bincode version reflects what used to be
/// stored on disk in earlier versions and does internal crc32 checksumming.
///
/// This type should not implement `serde::Serialize` or `serde::Deserialize` because there would
/// be a confusion whether you want the old version ([`TimelineMetadata::from_bytes`]) or the modern
/// as-exists in `index_part.json` ([`self::modern_serde`]).
///
/// ```compile_fail
/// #[derive(serde::Serialize)]
/// struct DoNotDoThis(pageserver::tenant::metadata::TimelineMetadata);
/// ```
///
/// ```compile_fail
/// #[derive(serde::Deserialize)]
/// struct NeitherDoThis(pageserver::tenant::metadata::TimelineMetadata);
/// ```
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineMetadata {
    hdr: TimelineMetadataHeader,
    body: TimelineMetadataBodyV2,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
struct TimelineMetadataHeader {
    checksum: u32,       // CRC of serialized metadata body
    size: u16,           // size of serialized metadata
    format_version: u16, // metadata format version (used for compatibility checks)
}

impl TryFrom<&TimelineMetadataBodyV2> for TimelineMetadataHeader {
    type Error = Crc32CalculationFailed;

    fn try_from(value: &TimelineMetadataBodyV2) -> Result<Self, Self::Error> {
        #[derive(Default)]
        struct Crc32Sink {
            crc: u32,
            count: usize,
        }

        impl std::io::Write for Crc32Sink {
            fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
                self.crc = crc32c::crc32c_append(self.crc, buf);
                self.count += buf.len();
                Ok(buf.len())
            }

            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }

        // jump through hoops to calculate the crc32 so that TimelineMetadata::ne works
        // across serialization versions
        let mut sink = Crc32Sink::default();
        <TimelineMetadataBodyV2 as utils::bin_ser::BeSer>::ser_into(value, &mut sink)
            .map_err(Crc32CalculationFailed)?;

        let size = METADATA_HDR_SIZE + sink.count;

        Ok(TimelineMetadataHeader {
            checksum: sink.crc,
            size: size as u16,
            format_version: METADATA_FORMAT_VERSION,
        })
    }
}

#[derive(thiserror::Error, Debug)]
#[error("re-serializing for crc32 failed")]
struct Crc32CalculationFailed(#[source] utils::bin_ser::SerializeError);

const METADATA_HDR_SIZE: usize = size_of::<TimelineMetadataHeader>();

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
struct TimelineMetadataBodyV2 {
    disk_consistent_lsn: Lsn,
    // This is only set if we know it. We track it in memory when the page
    // server is running, but we only track the value corresponding to
    // 'last_record_lsn', not 'disk_consistent_lsn' which can lag behind by a
    // lot. We only store it in the metadata file when we flush *all* the
    // in-memory data so that 'last_record_lsn' is the same as
    // 'disk_consistent_lsn'.  That's OK, because after page server restart, as
    // soon as we reprocess at least one record, we will have a valid
    // 'prev_record_lsn' value in memory again. This is only really needed when
    // doing a clean shutdown, so that there is no more WAL beyond
    // 'disk_consistent_lsn'
    prev_record_lsn: Option<Lsn>,
    ancestor_timeline: Option<BranchId>,
    ancestor_lsn: Lsn,

    // The LSN at which GC was last executed.  Synonym of [`Timeline::applied_gc_cutoff_lsn`].
    latest_gc_cutoff_lsn: Lsn,

    initdb_lsn: Lsn,
    pg_version: PgMajorVersion,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
struct TimelineMetadataBodyV1 {
    disk_consistent_lsn: Lsn,
    // This is only set if we know it. We track it in memory when the page
    // server is running, but we only track the value corresponding to
    // 'last_record_lsn', not 'disk_consistent_lsn' which can lag behind by a
    // lot. We only store it in the metadata file when we flush *all* the
    // in-memory data so that 'last_record_lsn' is the same as
    // 'disk_consistent_lsn'.  That's OK, because after page server restart, as
    // soon as we reprocess at least one record, we will have a valid
    // 'prev_record_lsn' value in memory again. This is only really needed when
    // doing a clean shutdown, so that there is no more WAL beyond
    // 'disk_consistent_lsn'
    prev_record_lsn: Option<Lsn>,
    ancestor_timeline: Option<BranchId>,
    ancestor_lsn: Lsn,
    latest_gc_cutoff_lsn: Lsn,
    initdb_lsn: Lsn,
}

impl TimelineMetadata {
    pub fn new(
        disk_consistent_lsn: Lsn,
        prev_record_lsn: Option<Lsn>,
        ancestor_timeline: Option<BranchId>,
        ancestor_lsn: Lsn,
        latest_gc_cutoff_lsn: Lsn,
        initdb_lsn: Lsn,
        pg_version: PgMajorVersion,
    ) -> Self {
        Self {
            hdr: TimelineMetadataHeader {
                checksum: 0,
                size: 0,
                format_version: METADATA_FORMAT_VERSION,
            },
            body: TimelineMetadataBodyV2 {
                disk_consistent_lsn,
                prev_record_lsn,
                ancestor_timeline,
                ancestor_lsn,
                latest_gc_cutoff_lsn,
                initdb_lsn,
                pg_version,
            },
        }
    }

    fn upgrade_timeline_metadata(metadata_bytes: &[u8]) -> anyhow::Result<Self> {
        let mut hdr = TimelineMetadataHeader::des(&metadata_bytes[0..METADATA_HDR_SIZE])?;

        // backward compatible only up to this version
        ensure!(
            hdr.format_version == METADATA_OLD_FORMAT_VERSION,
            "unsupported metadata format version {}",
            hdr.format_version
        );

        let metadata_size = hdr.size as usize;

        let body: TimelineMetadataBodyV1 =
            TimelineMetadataBodyV1::des(&metadata_bytes[METADATA_HDR_SIZE..metadata_size])?;

        let body = TimelineMetadataBodyV2 {
            disk_consistent_lsn: body.disk_consistent_lsn,
            prev_record_lsn: body.prev_record_lsn,
            ancestor_timeline: body.ancestor_timeline,
            ancestor_lsn: body.ancestor_lsn,
            latest_gc_cutoff_lsn: body.latest_gc_cutoff_lsn,
            initdb_lsn: body.initdb_lsn,
            pg_version: PgMajorVersion::PG17, // v17-only fork; this legacy-metadata default is otherwise unreachable
        };

        hdr.format_version = METADATA_FORMAT_VERSION;

        Ok(Self { hdr, body })
    }

    pub fn from_bytes(metadata_bytes: &[u8]) -> anyhow::Result<Self> {
        ensure!(
            metadata_bytes.len() == METADATA_MAX_SIZE,
            "metadata bytes size is wrong"
        );
        let hdr = TimelineMetadataHeader::des(&metadata_bytes[0..METADATA_HDR_SIZE])?;

        let metadata_size = hdr.size as usize;
        ensure!(
            metadata_size <= METADATA_MAX_SIZE,
            "corrupted metadata file"
        );
        let calculated_checksum = crc32c::crc32c(&metadata_bytes[METADATA_HDR_SIZE..metadata_size]);
        ensure!(
            hdr.checksum == calculated_checksum,
            "metadata checksum mismatch"
        );

        if hdr.format_version != METADATA_FORMAT_VERSION {
            // If metadata has the old format,
            // upgrade it and return the result
            TimelineMetadata::upgrade_timeline_metadata(metadata_bytes)
        } else {
            let body =
                TimelineMetadataBodyV2::des(&metadata_bytes[METADATA_HDR_SIZE..metadata_size])?;
            ensure!(
                body.disk_consistent_lsn.is_aligned(),
                "disk_consistent_lsn is not aligned"
            );
            Ok(TimelineMetadata { hdr, body })
        }
    }

    pub fn to_bytes(&self) -> Result<Vec<u8>, SerializeError> {
        let body_bytes = self.body.ser()?;
        let metadata_size = METADATA_HDR_SIZE + body_bytes.len();
        let hdr = TimelineMetadataHeader {
            size: metadata_size as u16,
            format_version: METADATA_FORMAT_VERSION,
            checksum: crc32c::crc32c(&body_bytes),
        };
        let hdr_bytes = hdr.ser()?;
        let mut metadata_bytes = vec![0u8; METADATA_MAX_SIZE];
        metadata_bytes[0..METADATA_HDR_SIZE].copy_from_slice(&hdr_bytes);
        metadata_bytes[METADATA_HDR_SIZE..metadata_size].copy_from_slice(&body_bytes);
        Ok(metadata_bytes)
    }

    /// [`Lsn`] that corresponds to the corresponding timeline directory
    /// contents, stored locally in the pageserver workdir.
    pub fn disk_consistent_lsn(&self) -> Lsn {
        self.body.disk_consistent_lsn
    }

    pub fn prev_record_lsn(&self) -> Option<Lsn> {
        self.body.prev_record_lsn
    }

    pub fn ancestor_timeline(&self) -> Option<BranchId> {
        self.body.ancestor_timeline
    }

    pub fn ancestor_lsn(&self) -> Lsn {
        self.body.ancestor_lsn
    }

    /// When reparenting, the `ancestor_lsn` does not change.
    ///
    /// Returns true if anything was changed.
    pub fn reparent(&mut self, timeline: &BranchId) {
        assert!(self.body.ancestor_timeline.is_some());
        // no assertion for redoing this: it's fine, we may have to repeat this multiple times over
        self.body.ancestor_timeline = Some(*timeline);
    }

    /// Returns true if anything was changed
    pub fn detach_from_ancestor(&mut self, branchpoint: &(BranchId, Lsn)) {
        // Detaching from ancestor now doesn't always detach directly to the direct ancestor, but we
        // ensure the LSN is the same. So we don't check the timeline ID.
        if self.body.ancestor_lsn != Lsn(0) {
            assert_eq!(self.body.ancestor_lsn, branchpoint.1);
        }
        self.body.ancestor_timeline = None;
        self.body.ancestor_lsn = Lsn(0);
    }

    pub fn latest_gc_cutoff_lsn(&self) -> Lsn {
        self.body.latest_gc_cutoff_lsn
    }

    pub fn initdb_lsn(&self) -> Lsn {
        self.body.initdb_lsn
    }

    pub fn pg_version(&self) -> PgMajorVersion {
        self.body.pg_version
    }
}

/// Limited history of earlier ancestors.
///
/// A timeline can have more than 1 earlier ancestor, in the rare case that it was repeatedly
/// reparented by having an later timeline be detached from it's ancestor.
///
/// Persisted as a JSONB column on `pageserver.timeline_state`. In yb-light
/// reparenting and detach-ancestor are unsupported, so this is always
/// `Default::default()`; the column is kept for forward-compat.
#[derive(Debug, PartialEq, Eq, Clone, serde::Serialize, serde::Deserialize, Default)]
pub struct Lineage {
    #[serde(skip_serializing_if = "is_false", default)]
    reparenting_history_truncated: bool,
    #[serde(skip_serializing_if = "Vec::is_empty", default)]
    reparenting_history: Vec<utils::id::BranchId>,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    original_ancestor: Option<(
        utils::id::BranchId,
        utils::lsn::Lsn,
        chrono::NaiveDateTime,
    )>,
}

fn is_false(b: &bool) -> bool {
    !b
}

pub(crate) mod modern_serde {
    use serde::{Deserialize, Serialize};

    use super::{TimelineMetadata, TimelineMetadataBodyV2, TimelineMetadataHeader};

    #[allow(dead_code)]
    pub(crate) fn deserialize<'de, D>(deserializer: D) -> Result<TimelineMetadata, D::Error>
    where
        D: serde::de::Deserializer<'de>,
    {
        // for legacy reasons versions 1-5 had TimelineMetadata serialized as a Vec<u8> field with
        // BeSer.
        struct Visitor;

        impl<'d> serde::de::Visitor<'d> for Visitor {
            type Value = TimelineMetadata;

            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("BeSer bytes or json structure")
            }

            fn visit_seq<A>(self, seq: A) -> Result<Self::Value, A::Error>
            where
                A: serde::de::SeqAccess<'d>,
            {
                use serde::de::Error;
                let de = serde::de::value::SeqAccessDeserializer::new(seq);
                Vec::<u8>::deserialize(de)
                    .map(|v| TimelineMetadata::from_bytes(&v).map_err(A::Error::custom))?
            }

            fn visit_map<A>(self, map: A) -> Result<Self::Value, A::Error>
            where
                A: serde::de::MapAccess<'d>,
            {
                use serde::de::Error;

                let de = serde::de::value::MapAccessDeserializer::new(map);
                let body = TimelineMetadataBodyV2::deserialize(de)?;
                let hdr = TimelineMetadataHeader::try_from(&body).map_err(A::Error::custom)?;

                Ok(TimelineMetadata { hdr, body })
            }
        }

        deserializer.deserialize_any(Visitor)
    }

    #[allow(dead_code)]
    pub(crate) fn serialize<S>(
        metadata: &TimelineMetadata,
        serializer: S,
    ) -> Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        // header is not needed, upon reading we've upgraded all v1 to v2
        metadata.body.serialize(serializer)
    }

    #[test]
    fn deserializes_bytes_as_well_as_equivalent_body_v2() {
        use super::{Lsn, METADATA_MAX_SIZE, PgMajorVersion};

        #[derive(serde::Deserialize, serde::Serialize)]
        struct Wrapper(
            #[serde(deserialize_with = "deserialize", serialize_with = "serialize")]
            TimelineMetadata,
        );

        // Build the blob the fork-native way rather than hardcoding an
        // upstream PG15 byte array: this is a v17-only fork (PgMajorVersion
        // has no PG15 variant), so a 15-tagged blob can't round-trip. The
        // 512-byte buffer `to_bytes` emits dwarfs the ~54-byte body — the
        // trailing-padding ("too many bytes") case this test guards — and
        // carries a real header + checksum, exercising the same BeSer path
        // on-disk metadata used.
        let bytes = TimelineMetadata::new(
            Lsn(0x149FD90),       // disk_consistent_lsn (8-aligned)
            Some(Lsn(0x149FD18)), // prev_record_lsn
            None,                 // ancestor_timeline
            Lsn(0),               // ancestor_lsn
            Lsn(0x149FD18),       // latest_gc_cutoff_lsn
            Lsn(0x149FD18),       // initdb_lsn
            PgMajorVersion::PG17,
        )
        .to_bytes()
        .unwrap();
        assert_eq!(bytes.len(), METADATA_MAX_SIZE, "to_bytes pads to the max size");

        // Legacy form: TimelineMetadata serialized as a `Vec<u8>` (visit_seq).
        let as_seq = serde_json::to_string(&bytes).unwrap();
        let wrapper_from_bytes = serde_json::from_str::<Wrapper>(&as_seq).unwrap();

        let serialized = serde_json::to_value(&wrapper_from_bytes).unwrap();

        assert_eq!(
            serialized,
            serde_json::json! {{
                "disk_consistent_lsn": "0/149FD90",
                "prev_record_lsn": "0/149FD18",
                "ancestor_timeline": null,
                "ancestor_lsn": "0/0",
                "latest_gc_cutoff_lsn": "0/149FD18",
                "initdb_lsn": "0/149FD18",
                "pg_version": 17
            }}
        );

        let wrapper_from_json = serde_json::value::from_value::<Wrapper>(serialized).unwrap();

        assert_eq!(wrapper_from_bytes.0, wrapper_from_json.0);
    }
}
