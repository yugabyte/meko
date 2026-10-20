//!
//! This provides an abstraction to store PostgreSQL relations and other files
//! in the key-value store that implements the Repository interface.
//!
//! (TODO: The line between PUT-functions here and walingest.rs is a bit blurry, as
//! walingest.rs handles a few things like implicit relation creation and extension.
//! Clarify that)
//!
use std::collections::{BTreeSet, HashMap, HashSet, hash_map};
use std::ops::ControlFlow;
use std::sync::Arc;

use crate::walingest::{WalIngestError, WalIngestErrorKind};
use crate::{PERF_TRACE_TARGET, ensure_walingest};
use anyhow::Context;
use bytes::{Buf, Bytes, BytesMut};
use enum_map::Enum;
use pageserver_api::key::{
    CHECKPOINT_KEY, CONTROLFILE_KEY, CompactKey, DBDIR_KEY, Key, RelDirExists, TWOPHASEDIR_KEY,
    rel_block_to_key, rel_size_to_key, rel_tag_sparse_key, rel_tag_sparse_key_range,
    relmap_file_key, repl_origin_key, repl_origin_key_range, slru_block_to_key, slru_dir_to_key,
    slru_segment_size_to_key, twophase_file_key,
};
use pageserver_api::keyspace::KeySpaceRandomAccum;
use pageserver_api::reltag::{BlockNumber, RelTag, SlruKind};
use postgres_ffi::{BLCKSZ, TransactionId};
use postgres_ffi_types::forknum::{FSM_FORKNUM, MAIN_FORKNUM, VISIBILITYMAP_FORKNUM};
use postgres_ffi_types::{Oid, RepOriginId, TimestampTz};
use serde::{Deserialize, Serialize};
use strum::IntoEnumIterator;
use tokio_util::sync::CancellationToken;
use tracing::{debug, info, info_span, warn};
use utils::bin_ser::{BeSer, DeserializeError};
use utils::lsn::Lsn;
use utils::pausable_failpoint;
use wal_decoder::models::record::NeonWalRecord;
use wal_decoder::models::value::Value;
use wal_decoder::serialized_batch::{SerializedValueBatch, ValueMeta};

use super::page_server::{PageReconstructError, Timeline};
use crate::aux_file;
use crate::context::{PerfInstrumentFutureExt, RequestContext, RequestContextBuilder};
use crate::keyspace::{KeySpace, KeySpaceAccum};
use crate::metrics::{
    RELSIZE_CACHE_MISSES_OLD, RELSIZE_LATEST_CACHE_ENTRIES, RELSIZE_LATEST_CACHE_HITS,
    RELSIZE_LATEST_CACHE_MISSES, RELSIZE_SNAPSHOT_CACHE_ENTRIES, RELSIZE_SNAPSHOT_CACHE_HITS,
    RELSIZE_SNAPSHOT_CACHE_MISSES,
};
use crate::page_server::timeline::{GetVectoredError, VersionedKeySpaceQuery};
use crate::span::debug_assert_current_span_has_branch_id;
use crate::walredo::apply_neon::clog_trailer;

/// Max delta records appended to the AUX_FILES_KEY (for aux v1). The write path will write a full image once this threshold is reached.
pub const MAX_AUX_FILE_DELTAS: usize = 1024;

/// Max number of aux-file-related delta layers. The compaction will create a new image layer once this threshold is reached.
pub const MAX_AUX_FILE_V2_DELTAS: usize = 16;

/// Rows per block fetched by [`Timeline::map_all_timestamps`]'s row-based
/// trailer read. The newest commit row is almost always row 1; the rest of the
/// budget absorbs a run of aborts on top of it before the block falls back to
/// page reconstruction.
const CLOG_TRAILER_ROWS: u64 = 16;

/// What the newest-first rows of a clog block say about its trailer.
#[derive(Debug, PartialEq, Eq)]
enum ClogTrailer {
    /// Settled by a commit row or an image: the trailer, or `None` when the
    /// page has none (an image without one).
    Known(Option<TimestampTz>),
    /// Not settled by the fetched prefix (aborts only, no rows, or a record
    /// this decoder does not know): reconstruct the page instead.
    Unknown,
}

/// The trailer of a clog page is the `timestamp` of the newest
/// `ClogSetCommitted` applied to it (`apply_neon`); aborts leave it alone and
/// an image already carries it. So, walking `rows` newest-first: a commit row
/// or an image settles the answer, an abort is skipped.
fn clog_trailer_from_rows(rows: &[crate::page_store::SlruPageRow]) -> anyhow::Result<ClogTrailer> {
    for row in rows {
        if row.is_image {
            return Ok(ClogTrailer::Known(clog_trailer(&row.payload)));
        }
        match NeonWalRecord::des(&row.payload)
            .with_context(|| format!("decoding clog delta at {}", row.lsn))?
        {
            NeonWalRecord::ClogSetCommitted { timestamp, .. } => {
                return Ok(ClogTrailer::Known(Some(timestamp)));
            }
            NeonWalRecord::ClogSetAborted { .. } => continue,
            _ => return Ok(ClogTrailer::Unknown),
        }
    }
    Ok(ClogTrailer::Unknown)
}

/// The five-way classification at the end of `find_lsn_for_timestamp`.
///
/// Pure because the segment visiting order in `map_all_timestamps` changes
/// `found_smaller` — a too-late probe stops at the first trailer >= T and
/// may see no smaller one on the way — without being allowed to change the
/// result. It cannot: a too-early probe walks every page and sets
/// `found_smaller` whenever any trailer exists, in any order; and if every
/// probe was too-late, `low` never moved, so `commit_lsn < min_lsn` and both
/// `(false, true)` and the guarded `(true, _)` arm return `Past(min_lsn)`.
/// Keep those two arms agreeing.
fn lsn_for_timestamp_result(
    found_smaller: bool,
    found_larger: bool,
    commit_lsn: Lsn,
    min_lsn: Lsn,
) -> LsnForTimestamp {
    match (found_smaller, found_larger) {
        // No commit records processed yet, e.g. just after importing.
        (false, false) => LsnForTimestamp::NoData(min_lsn),
        // No commit timestamp smaller than the request.
        (false, true) => LsnForTimestamp::Past(min_lsn),
        // `found_smaller` was set but the search never advanced `low`, so the
        // subtraction landed below `min_lsn`; never return that.
        (true, _) if commit_lsn < min_lsn => LsnForTimestamp::Past(min_lsn),
        // Only commits older than the request. Still valid for branch
        // creation; `update_gc_info()` ignores the LSN of a `Future` anyway.
        (true, false) => LsnForTimestamp::Future(commit_lsn),
        (true, true) => LsnForTimestamp::Present(commit_lsn),
    }
}

#[derive(Debug)]
pub enum LsnForTimestamp {
    /// Found commits both before and after the given timestamp
    Present(Lsn),

    /// Found no commits after the given timestamp, this means
    /// that the newest data in the branch is older than the given
    /// timestamp.
    ///
    /// All commits <= LSN happened before the given timestamp
    Future(Lsn),

    /// The queried timestamp is past our horizon we look back at (PITR)
    ///
    /// All commits > LSN happened after the given timestamp,
    /// but any commits < LSN might have happened before or after
    /// the given timestamp. We don't know because no data before
    /// the given lsn is available.
    Past(Lsn),

    /// We have found no commit with a timestamp,
    /// so we can't return anything meaningful.
    ///
    /// The associated LSN is the lower bound value we can safely
    /// create branches on, but no statement is made if it is
    /// older or newer than the timestamp.
    ///
    /// This variant can e.g. be returned right after a
    /// cluster import.
    NoData(Lsn),
}

/// Each request to page server contains LSN range: `not_modified_since..request_lsn`.
/// See comments libs/pageserver_api/src/models.rs.
/// Based on this range and `last_record_lsn` PS calculates `effective_lsn`.
/// But to distinguish requests from primary and replicas we need also to pass `request_lsn`.
#[derive(Debug, Clone, Copy, Default)]
pub struct LsnRange {
    pub effective_lsn: Lsn,
    pub request_lsn: Lsn,
}

impl LsnRange {
    pub fn at(lsn: Lsn) -> LsnRange {
        LsnRange {
            effective_lsn: lsn,
            request_lsn: lsn,
        }
    }
    pub fn is_latest(&self) -> bool {
        self.request_lsn == Lsn::MAX
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum CalculateLogicalSizeError {
    #[error("cancelled")]
    Cancelled,

    /// Something went wrong while reading the metadata we use to calculate logical size
    /// Note that cancellation variants of `PageReconstructError` are transformed to [`Self::Cancelled`]
    /// in the `From` implementation for this variant.
    #[error(transparent)]
    PageRead(PageReconstructError),

    /// Something went wrong deserializing metadata that we read to calculate logical size
    #[error("decode error: {0}")]
    Decode(#[from] DeserializeError),
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum CollectKeySpaceError {
    #[error(transparent)]
    Decode(#[from] DeserializeError),
    #[error(transparent)]
    PageRead(PageReconstructError),
    #[error("cancelled")]
    Cancelled,
}

impl From<PageReconstructError> for CollectKeySpaceError {
    fn from(err: PageReconstructError) -> Self {
        match err {
            PageReconstructError::Cancelled => Self::Cancelled,
            err => Self::PageRead(err),
        }
    }
}

impl From<PageReconstructError> for CalculateLogicalSizeError {
    fn from(pre: PageReconstructError) -> Self {
        match pre {
            PageReconstructError::Cancelled => Self::Cancelled,
            _ => Self::PageRead(pre),
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum RelationError {
    #[error("invalid relnode")]
    InvalidRelnode,
}

///
/// This impl provides all the functionality to store PostgreSQL relations, SLRUs,
/// and other special kinds of files, in a versioned key-value store. The
/// Timeline struct provides the key-value store.
///
/// This is a separate impl, so that we can easily include all these functions in a Timeline
/// implementation, and might be moved into a separate struct later.
impl Timeline {
    /// Start ingesting a WAL record, or other atomic modification of
    /// the timeline.
    ///
    /// This provides a transaction-like interface to perform a bunch
    /// of modifications atomically.
    ///
    /// To ingest a WAL record, call begin_modification(lsn) to get a
    /// DatadirModification object. Use the functions in the object to
    /// modify the repository state, updating all the pages and metadata
    /// that the WAL record affects. When you're done, call commit() to
    /// commit the changes.
    ///
    /// Lsn stored in modification is advanced by `ingest_record` and
    /// is used by `commit()` to update `last_record_lsn`.
    ///
    /// Calling commit() will flush all the changes and reset the state,
    /// so the `DatadirModification` struct can be reused to perform the next modification.
    ///
    /// Note that any pending modifications you make through the
    /// modification object won't be visible to calls to the 'get' and list
    /// functions of the timeline until you finish! And if you update the
    /// same page twice, the last update wins.
    ///
    pub fn begin_modification(&self, lsn: Lsn) -> DatadirModification
    where
        Self: Sized,
    {
        debug_assert!(lsn.is_valid(), "a modification starts at a real LSN");
        DatadirModification {
            tline: self,
            pending_lsns: Vec::new(),
            pending_metadata_pages: HashMap::new(),
            pending_data_batch: None,
            pending_nblocks: 0,
            pending_directory_entries: Vec::new(),
            pending_gc_drop_shrink_events: Vec::new(),
            pending_metadata_bytes: 0,
            lsn,
        }
    }

    //------------------------------------------------------------------------------
    // Public GET functions
    //------------------------------------------------------------------------------

    /// Look up given page version.
    pub(crate) async fn get_rel_page_at_lsn(
        &self,
        tag: RelTag,
        blknum: BlockNumber,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        match version {
            Version::LsnRange(lsns) => {
                let pages: smallvec::SmallVec<[_; 1]> = smallvec::smallvec![(tag, blknum)];
                let res = self
                    .get_rel_page_at_lsn_batched(
                        pages
                            .iter()
                            .map(|(tag, blknum)| (tag, blknum, lsns, ctx.attached_child())),
                        ctx,
                    )
                    .await;
                assert_eq!(res.len(), 1);
                res.into_iter().next().unwrap()
            }
            Version::Modified(modification) => {
                if tag.relnode == 0 {
                    return Err(PageReconstructError::Other(
                        RelationError::InvalidRelnode.into(),
                    ));
                }

                let nblocks = self.get_rel_size(tag, version, ctx).await?;
                if blknum >= nblocks {
                    debug!(
                        "read beyond EOF at {} blk {} at {}, size is {}: returning all-zeros page",
                        tag,
                        blknum,
                        version.get_lsn(),
                        nblocks
                    );
                    return Ok(ZERO_PAGE.clone());
                }

                let key = rel_block_to_key(tag, blknum);
                modification.get(key, ctx).await
            }
        }
    }

    /// Like [`Self::get_rel_page_at_lsn`], but returns a batch of pages.
    ///
    /// The ordering of the returned vec corresponds to the ordering of `pages`.
    ///
    /// NB: the read path must be cancellation-safe. The Tonic gRPC service will drop the future
    /// if the client goes away (e.g. due to timeout or cancellation).
    /// TODO: verify that it actually is cancellation-safe.
    pub(crate) async fn get_rel_page_at_lsn_batched(
        &self,
        pages: impl ExactSizeIterator<Item = (&RelTag, &BlockNumber, LsnRange, RequestContext)>,
        ctx: &RequestContext,
    ) -> Vec<Result<Bytes, PageReconstructError>> {
        debug_assert_current_span_has_branch_id();

        let mut slots_filled = 0;
        let page_count = pages.len();

        // Would be nice to use smallvec here but it doesn't provide the spare_capacity_mut() API.
        let mut result = Vec::with_capacity(pages.len());
        let result_slots = result.spare_capacity_mut();

        let mut keys_slots: HashMap<Key, smallvec::SmallVec<[(usize, RequestContext); 1]>> =
            HashMap::with_capacity(pages.len());

        let mut req_keyspaces: HashMap<Lsn, KeySpaceRandomAccum> =
            HashMap::with_capacity(pages.len());

        for (response_slot_idx, (tag, blknum, lsns, ctx)) in pages.enumerate() {
            if tag.relnode == 0 {
                result_slots[response_slot_idx].write(Err(PageReconstructError::Other(
                    RelationError::InvalidRelnode.into(),
                )));

                slots_filled += 1;
                continue;
            }
            let lsn = lsns.effective_lsn;
            let nblocks = {
                let ctx = RequestContextBuilder::from(&ctx)
                    .perf_span(|crnt_perf_span| {
                        info_span!(
                            target: PERF_TRACE_TARGET,
                            parent: crnt_perf_span,
                            "GET_REL_SIZE",
                            reltag=%tag,
                            lsn=%lsn,
                        )
                    })
                    .attached_child();

                match self
                    .get_rel_size(*tag, Version::LsnRange(lsns), &ctx)
                    .maybe_perf_instrument(&ctx, |crnt_perf_span| crnt_perf_span.clone())
                    .await
                {
                    Ok(nblocks) => nblocks,
                    Err(err) => {
                        result_slots[response_slot_idx].write(Err(err));
                        slots_filled += 1;
                        continue;
                    }
                }
            };

            if *blknum >= nblocks {
                debug!(
                    "read beyond EOF at {} blk {} at {}, size is {}: returning all-zeros page",
                    tag, blknum, lsn, nblocks
                );
                result_slots[response_slot_idx].write(Ok(ZERO_PAGE.clone()));
                slots_filled += 1;
                continue;
            }

            let key = rel_block_to_key(*tag, *blknum);

            let ctx = RequestContextBuilder::from(&ctx)
                .perf_span(|crnt_perf_span| {
                    info_span!(
                        target: PERF_TRACE_TARGET,
                        parent: crnt_perf_span,
                        "GET_BATCH",
                        batch_size = %page_count,
                    )
                })
                .attached_child();

            let key_slots = keys_slots.entry(key).or_default();
            key_slots.push((response_slot_idx, ctx));

            let acc = req_keyspaces.entry(lsn).or_default();
            acc.add_key(key);
        }

        let query: Vec<(Lsn, KeySpace)> = req_keyspaces
            .into_iter()
            .map(|(lsn, acc)| (lsn, acc.to_keyspace()))
            .collect();

        let query = VersionedKeySpaceQuery::scattered(query);
        let res = self
            .get_vectored(query, ctx)
            .maybe_perf_instrument(ctx, |current_perf_span| current_perf_span.clone())
            .await;

        match res {
            Ok(results) => {
                for (key, res) in results {
                    let mut key_slots = keys_slots.remove(&key).unwrap().into_iter();
                    let (first_slot, first_req_ctx) = key_slots.next().unwrap();

                    for (slot, req_ctx) in key_slots {
                        let clone = match &res {
                            Ok(buf) => Ok(buf.clone()),
                            Err(err) => Err(match err {
                                PageReconstructError::Cancelled => PageReconstructError::Cancelled,

                                x @ PageReconstructError::Other(_)
                                | x @ PageReconstructError::AncestorLsnTimeout(_)
                                | x @ PageReconstructError::WalRedo(_)
                                | x @ PageReconstructError::MissingKey(_) => {
                                    PageReconstructError::Other(anyhow::anyhow!(
                                        "there was more than one request for this key in the batch, error logged once: {x:?}"
                                    ))
                                }
                            }),
                        };

                        result_slots[slot].write(clone);
                        // There is no standardized way to express that the batched span followed from N request spans.
                        // So, abuse the system and mark the request contexts as follows_from the batch span, so we get
                        // some linkage in our trace viewer. It allows us to answer: which GET_VECTORED did this GET_PAGE wait for.
                        req_ctx.perf_follows_from(ctx);
                        slots_filled += 1;
                    }

                    result_slots[first_slot].write(res);
                    first_req_ctx.perf_follows_from(ctx);
                    slots_filled += 1;
                }

                // The row store leaves out a key with no row at or below its LSN.
                for (key, key_slots) in keys_slots.drain() {
                    for (slot, req_ctx) in key_slots {
                        result_slots[slot].write(Err(PageReconstructError::from(anyhow::anyhow!(
                            "base image for {key} not found"
                        ))));
                        req_ctx.perf_follows_from(ctx);
                        slots_filled += 1;
                    }
                }
            }
            Err(err) => {
                // this cannot really happen because get_vectored only errors globally on invalid LSN or too large batch size
                // (We enforce the max batch size outside of this function, in the code that constructs the batch request.)
                for (slot, req_ctx) in keys_slots.values().flatten() {
                    // this whole `match` is a lot like `From<GetVectoredError> for PageReconstructError`
                    // but without taking ownership of the GetVectoredError
                    let err = match &err {
                        GetVectoredError::Cancelled => Err(PageReconstructError::Cancelled),
                        // TODO: restructure get_vectored API to make this error per-key
                        GetVectoredError::MissingKey(err) => {
                            Err(PageReconstructError::Other(anyhow::anyhow!(
                                "whole vectored get request failed because one or more of the requested keys were missing: {err:?}"
                            )))
                        }
                        // TODO: restructure get_vectored API to make this error per-key
                        GetVectoredError::GetReadyAncestorError(err) => {
                            Err(PageReconstructError::Other(anyhow::anyhow!(
                                "whole vectored get request failed because one or more key required ancestor that wasn't ready: {err:?}"
                            )))
                        }
                        // TODO: restructure get_vectored API to make this error per-key
                        GetVectoredError::Other(err) => Err(PageReconstructError::Other(
                            anyhow::anyhow!("whole vectored get request failed: {err:?}"),
                        )),
                        // TODO: we can prevent this error class by moving this check into the type system
                        GetVectoredError::InvalidLsn(e) => {
                            Err(anyhow::anyhow!("invalid LSN: {e:?}").into())
                        }
                        // NB: this should never happen in practice because we limit batch size to be smaller than max_get_vectored_keys
                        // TODO: we can prevent this error class by moving this check into the type system
                        GetVectoredError::Oversized(err, max) => {
                            Err(anyhow::anyhow!("batching oversized: {err} > {max}").into())
                        }
                    };

                    req_ctx.perf_follows_from(ctx);
                    result_slots[*slot].write(err);
                }

                slots_filled += keys_slots.values().map(|slots| slots.len()).sum::<usize>();
            }
        };

        assert_eq!(slots_filled, page_count);
        // SAFETY:
        // 1. `result` and any of its uninint members are not read from until this point
        // 2. The length below is tracked at run-time and matches the number of requested pages.
        unsafe {
            result.set_len(page_count);
        }

        result
    }

    /// Get size of a database in blocks. This is only accurate on shard 0. It will undercount on
    /// other shards, by only accounting for relations the shard has pages for, and only accounting
    /// for pages up to the highest page number it has stored.
    pub(crate) async fn get_db_size(
        &self,
        spcnode: Oid,
        dbnode: Oid,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<usize, PageReconstructError> {
        let mut total_blocks = 0;

        let rels = self.list_rels(spcnode, dbnode, version, ctx).await?;

        if rels.is_empty() {
            return Ok(0);
        }

        for rel in rels {
            let n_blocks = self
                .get_rel_size_in_reldir(rel, version, false, ctx)
                .await?
                .expect("allow_missing=false");
            total_blocks += n_blocks as usize;
        }
        Ok(total_blocks)
    }

    /// Get size of a relation file. The relation must exist, otherwise an error is returned.
    ///
    /// This is only accurate on shard 0. On other shards, it will return the size up to the highest
    /// page number stored in the shard.
    pub(crate) async fn get_rel_size(
        &self,
        tag: RelTag,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<BlockNumber, PageReconstructError> {
        Ok(self
            .get_rel_size_in_reldir(tag, version, false, ctx)
            .await?
            .expect("allow_missing=false"))
    }

    /// Get size of a relation file. If `allow_missing` is true, returns None for missing relations,
    /// otherwise errors.
    ///
    /// INVARIANT: never returns None if `allow_missing=false`.
    ///
    pub(crate) async fn get_rel_size_in_reldir(
        &self,
        tag: RelTag,
        version: Version<'_>,
        allow_missing: bool,
        ctx: &RequestContext,
    ) -> Result<Option<BlockNumber>, PageReconstructError> {
        if tag.relnode == 0 {
            return Err(PageReconstructError::Other(
                RelationError::InvalidRelnode.into(),
            ));
        }

        if let Some(nblocks) = self.get_cached_rel_size(&tag, version) {
            return Ok(Some(nblocks));
        }

        if allow_missing && !self.get_rel_exists_in_reldir(tag, version, ctx).await? {
            return Ok(None);
        }

        if (tag.forknum == FSM_FORKNUM || tag.forknum == VISIBILITYMAP_FORKNUM)
            && !self.get_rel_exists_in_reldir(tag, version, ctx).await?
        {
            // FIXME: Postgres sometimes calls smgrcreate() to create
            // FSM, and smgrnblocks() on it immediately afterwards,
            // without extending it.  Tolerate that by claiming that
            // any non-existent FSM fork has size 0.
            return Ok(Some(0));
        }

        let key = rel_size_to_key(tag);
        let mut buf = match version.get(self, key, ctx).await {
            Ok(buf) => buf,
            // #36: the rel-directory can list an FSM/VM fork while its
            // rel-size key is absent at this LSN — e.g. crash-recovery
            // replay resuming at a `consistent_lsn` that fell between the
            // directory write and the first size write. FSM/VM are advisory
            // (rebuilt by VACUUM), so treat a missing size as 0 rather than
            // letting `MissingKey` wedge the walreceiver on an
            // `SMGR_TRUNCATE_FSM/_VM` ingest. Same spirit as the
            // smgrcreate-then-smgrnblocks tolerance above; scoped to FSM/VM
            // so a missing MAIN-fork size still surfaces as an error.
            Err(PageReconstructError::MissingKey(_))
                if tag.forknum == FSM_FORKNUM || tag.forknum == VISIBILITYMAP_FORKNUM =>
            {
                return Ok(Some(0));
            }
            Err(e) => return Err(e),
        };
        let nblocks = buf.get_u32_le();

        self.update_cached_rel_size(tag, version, nblocks);

        Ok(Some(nblocks))
    }

    /// Does the relation exist?
    ///
    /// Only shard 0 has a full view of the relations. Other shards only know about relations that
    /// the shard stores pages for.
    ///
    pub(crate) async fn get_rel_exists(
        &self,
        tag: RelTag,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<bool, PageReconstructError> {
        self.get_rel_exists_in_reldir(tag, version, ctx).await
    }

    async fn get_rel_exists_in_reldir_v2(
        &self,
        tag: RelTag,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<bool, PageReconstructError> {
        let key = rel_tag_sparse_key(tag.spcnode, tag.dbnode, tag.relnode, tag.forknum);
        let buf = RelDirExists::decode_option(version.sparse_get(self, key, ctx).await?).map_err(
            |_| {
                PageReconstructError::Other(anyhow::anyhow!(
                    "invalid reldir key: decode failed, {}",
                    key
                ))
            },
        )?;
        let exists_v2 = buf == RelDirExists::Exists;
        Ok(exists_v2)
    }

    /// Does the relation exist?
    ///
    /// One sparse key per relation, so this is a point read: there is no
    /// directory to deserialize, and nothing for a caller looping over
    /// relations to hoist.
    pub(crate) async fn get_rel_exists_in_reldir(
        &self,
        tag: RelTag,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<bool, PageReconstructError> {
        if tag.relnode == 0 {
            return Err(PageReconstructError::Other(
                RelationError::InvalidRelnode.into(),
            ));
        }

        // first try to lookup relation in cache
        if let Some(_nblocks) = self.get_cached_rel_size(&tag, version) {
            return Ok(true);
        }
        // then check if the database was already initialized.
        // get_rel_exists can be called before dbdir is created.
        let buf = version.get(self, DBDIR_KEY, ctx).await?;
        let dbdirs = DbDirectory::des(&buf)?.dbdirs;
        if !dbdirs.contains_key(&(tag.spcnode, tag.dbnode)) {
            return Ok(false);
        }

        self.get_rel_exists_in_reldir_v2(tag, version, ctx).await
    }

    async fn list_rels_v2(
        &self,
        spcnode: Oid,
        dbnode: Oid,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<HashSet<RelTag>, PageReconstructError> {
        let key_range = rel_tag_sparse_key_range(spcnode, dbnode);
        // Ingest reads the state just before its record, like `DatadirModification::get`.
        let lsn = match version {
            Version::LsnRange(lsns) => lsns.effective_lsn,
            Version::Modified(modification) => modification.lsn_before_record(),
        };
        let results = self.scan(KeySpace::single(key_range), lsn, ctx).await?;
        let mut rels = HashSet::new();
        for (key, val) in results {
            let val = RelDirExists::decode(&val?).map_err(|_| {
                PageReconstructError::Other(anyhow::anyhow!(
                    "invalid reldir key: decode failed, {}",
                    key
                ))
            })?;
            if key.field6 != 1 {
                return Err(PageReconstructError::Other(anyhow::anyhow!(
                    "invalid reldir key: field6 != 1, {}",
                    key
                )));
            }
            if key.field2 != spcnode {
                return Err(PageReconstructError::Other(anyhow::anyhow!(
                    "invalid reldir key: field2 != spcnode, {}",
                    key
                )));
            }
            if key.field3 != dbnode {
                return Err(PageReconstructError::Other(anyhow::anyhow!(
                    "invalid reldir key: field3 != dbnode, {}",
                    key
                )));
            }
            let tag = RelTag {
                spcnode,
                dbnode,
                relnode: key.field4,
                forknum: key.field5,
            };
            if val == RelDirExists::Removed {
                debug_assert!(!rels.contains(&tag), "removed reltag in v2");
                continue;
            }
            let did_not_contain = rels.insert(tag);
            debug_assert!(did_not_contain, "duplicate reltag in v2");
        }
        // Overlay this modification's uncommitted markers; the scan above
        // reads the Timeline and cannot see them.
        if let Version::Modified(modification) = version {
            for (tag, marker) in modification.pending_reldir_markers(spcnode, dbnode) {
                match marker {
                    RelDirExists::Exists => {
                        rels.insert(tag);
                    }
                    RelDirExists::Removed => {
                        rels.remove(&tag);
                    }
                }
            }
        }
        Ok(rels)
    }

    /// Get a list of all existing relations in given tablespace and database.
    ///
    /// Only shard 0 has a full view of the relations. Other shards only know about relations that
    /// the shard stores pages for.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub(crate) async fn list_rels(
        &self,
        spcnode: Oid,
        dbnode: Oid,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<HashSet<RelTag>, PageReconstructError> {
        self.list_rels_v2(spcnode, dbnode, version, ctx).await
    }

    /// Get the whole SLRU segment
    pub(crate) async fn get_slru_segment(
        &self,
        kind: SlruKind,
        segno: u32,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        let n_blocks = self
            .get_slru_segment_size(kind, segno, Version::at(lsn), ctx)
            .await?;

        let keyspace = KeySpace::single(
            slru_block_to_key(kind, segno, 0)..slru_block_to_key(kind, segno, n_blocks),
        );

        let batches = keyspace.partition(
            self.conf.max_get_vectored_keys.get() as u64 * BLCKSZ as u64,
            BLCKSZ as u64,
        );

        let mut segment = BytesMut::with_capacity(n_blocks as usize * BLCKSZ as usize);
        for batch in batches.parts {
            let query = VersionedKeySpaceQuery::uniform(batch, lsn);
            let blocks = self.get_vectored(query, ctx).await?;

            for (_key, block) in blocks {
                let block = block?;
                segment.extend_from_slice(&block[..BLCKSZ as usize]);
            }
        }

        Ok(segment.freeze())
    }

    /// Get size of an SLRU segment
    pub(crate) async fn get_slru_segment_size(
        &self,
        kind: SlruKind,
        segno: u32,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<BlockNumber, PageReconstructError> {
        let key = slru_segment_size_to_key(kind, segno);
        let mut buf = version.get(self, key, ctx).await?;
        Ok(buf.get_u32_le())
    }

    /// Does the slru segment exist?
    pub(crate) async fn get_slru_segment_exists(
        &self,
        kind: SlruKind,
        segno: u32,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<bool, PageReconstructError> {
        // fetch directory listing
        let key = slru_dir_to_key(kind);
        let buf = version.get(self, key, ctx).await?;

        let dir = SlruSegmentDirectory::des(&buf)?;
        Ok(dir.segments.contains(&segno))
    }

    /// Locate LSN, such that all transactions that committed before
    /// 'search_timestamp' are visible, but nothing newer is.
    ///
    /// This is not exact. Commit timestamps are not guaranteed to be ordered,
    /// so it's not well defined which LSN you get if there were multiple commits
    /// "in flight" at that point in time.
    ///
    pub(crate) async fn find_lsn_for_timestamp(
        &self,
        search_timestamp: TimestampTz,
        cancel: &CancellationToken,
        ctx: &RequestContext,
    ) -> Result<LsnForTimestamp, PageReconstructError> {
        pausable_failpoint!("find-lsn-for-timestamp-pausable");

        let gc_cutoff_lsn_guard = self.get_applied_gc_cutoff_lsn();
        let gc_cutoff_planned = {
            let gc_info = self.gc_info.read().unwrap();
            info!(cutoffs=?gc_info.cutoffs, applied_cutoff=%*gc_cutoff_lsn_guard, "starting find_lsn_for_timestamp");
            gc_info.min_cutoff()
        };
        // Usually the planned cutoff is newer than the cutoff of the last gc run,
        // but let's be defensive.
        let gc_cutoff = gc_cutoff_planned.max(*gc_cutoff_lsn_guard);
        // We use this method to figure out the branching LSN for the new branch, but the
        // GC cutoff could be before the branching point and we cannot create a new branch
        // with LSN < `ancestor_lsn`. Thus, pick the maximum of these two to be
        // on the safe side.
        let min_lsn = std::cmp::max(gc_cutoff, self.get_ancestor_lsn());
        let max_lsn = self.get_last_record_lsn();

        // LSNs are always 8-byte aligned. low/mid/high represent the
        // LSN divided by 8.
        let mut low = min_lsn.0 / 8;
        let mut high = max_lsn.0 / 8 + 1;

        let mut found_smaller = false;
        let mut found_larger = false;

        while low < high {
            if cancel.is_cancelled() {
                return Err(PageReconstructError::Cancelled);
            }
            // cannot overflow, high and low are both smaller than u64::MAX / 2
            let mid = (high + low) / 2;

            let cmp = match self
                .is_latest_commit_timestamp_ge_than(
                    search_timestamp,
                    Lsn(mid * 8),
                    &mut found_smaller,
                    &mut found_larger,
                    ctx,
                )
                .await
            {
                Ok(res) => res,
                Err(PageReconstructError::MissingKey(e)) => {
                    warn!(
                        "Missing key while find_lsn_for_timestamp. Either we might have already garbage-collected that data or the key is really missing. Last error: {:#}",
                        e
                    );
                    // Return that we didn't find any requests smaller than the LSN, and logging the error.
                    return Ok(LsnForTimestamp::Past(min_lsn));
                }
                Err(e) => return Err(e),
            };

            if cmp {
                high = mid;
            } else {
                low = mid + 1;
            }
        }

        // If `found_smaller == true`, `low = t + 1` where `t` is the target LSN,
        // so the LSN of the last commit record before or at `search_timestamp`.
        // Remove one from `low` to get `t`.
        //
        // FIXME: it would be better to get the LSN of the previous commit.
        // Otherwise, if you restore to the returned LSN, the database will
        // include physical changes from later commits that will be marked
        // as aborted, and will need to be vacuumed away.
        let commit_lsn = Lsn((low - 1) * 8);
        Ok(lsn_for_timestamp_result(
            found_smaller,
            found_larger,
            commit_lsn,
            min_lsn,
        ))
    }

    /// Subroutine of find_lsn_for_timestamp(). Returns true, if there are any
    /// commits that committed after 'search_timestamp', at LSN 'probe_lsn'.
    ///
    /// Additionally, sets 'found_smaller'/'found_Larger, if encounters any commits
    /// with a smaller/larger timestamp.
    ///
    pub(crate) async fn is_latest_commit_timestamp_ge_than(
        &self,
        search_timestamp: TimestampTz,
        probe_lsn: Lsn,
        found_smaller: &mut bool,
        found_larger: &mut bool,
        ctx: &RequestContext,
    ) -> Result<bool, PageReconstructError> {
        self.map_all_timestamps(probe_lsn, ctx, |timestamp| {
            if timestamp >= search_timestamp {
                *found_larger = true;
                return ControlFlow::Break(true);
            } else {
                *found_smaller = true;
            }
            ControlFlow::Continue(())
        })
        .await
    }

    /// Obtain the timestamp for the given lsn.
    ///
    /// If the lsn has no timestamps (e.g. no commits), returns None.
    pub(crate) async fn get_timestamp_for_lsn(
        &self,
        probe_lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Option<TimestampTz>, PageReconstructError> {
        let mut max: Option<TimestampTz> = None;
        self.map_all_timestamps::<()>(probe_lsn, ctx, |timestamp| {
            if let Some(max_prev) = max {
                max = Some(max_prev.max(timestamp));
            } else {
                max = Some(timestamp);
            }
            ControlFlow::Continue(())
        })
        .await?;

        Ok(max)
    }

    /// Runs the given function on all the timestamps for a given lsn
    ///
    /// The return value is either given by the closure, or set to the `Default`
    /// impl's output.
    async fn map_all_timestamps<T: Default>(
        &self,
        probe_lsn: Lsn,
        ctx: &RequestContext,
        mut f: impl FnMut(TimestampTz) -> ControlFlow<T>,
    ) -> Result<T, PageReconstructError> {
        // Highest segno first, which is newest first except across an xid
        // wraparound (segno wraps at 4096 with the xid space), where it
        // inverts. A too-late probe stops at the first segment holding a
        // commit after the search timestamp: when few segments do (a recent
        // timestamp, e.g. branch-by-time) that is the newest one and HashSet
        // order would visit ~half the clog to find it; when many do (a cutoff
        // a day back) any order finds one within a couple of segments and the
        // sort is neutral — measured: unchanged Perform counts for the cutoff
        // shape. Order never changes the result: see `lsn_for_timestamp_result`.
        let mut segments: Vec<u32> = self
            .list_slru_segments(SlruKind::Clog, Version::at(probe_lsn), ctx)
            .await?
            .into_iter()
            .collect();
        segments.sort_unstable_by(|a, b| b.cmp(a));
        let step = self.conf.max_get_vectored_keys.get() as u32;
        for segno in segments {
            let nblocks = self
                .get_slru_segment_size(SlruKind::Clog, segno, Version::at(probe_lsn), ctx)
                .await?;
            let mut blk_hi = nblocks;
            while blk_hi > 0 {
                let blk_lo = blk_hi.saturating_sub(step);
                for trailer in self
                    .clog_trailers(segno, blk_lo..blk_hi, probe_lsn, ctx)
                    .await?
                {
                    // Newest block first, so a block that settles the probe
                    // short-circuits past an older block's error, as before.
                    let Some(timestamp) = trailer? else { continue };
                    match f(timestamp) {
                        ControlFlow::Break(b) => return Ok(b),
                        ControlFlow::Continue(()) => (),
                    }
                }
                blk_hi = blk_lo;
            }
        }
        Ok(Default::default())
    }

    /// Trailer timestamps of the clog blocks in `blocks` (one segment) as of
    /// `probe_lsn`, newest block first: `None` for a block without a trailer,
    /// `Err` for one that could not be read. Errors are returned per block
    /// rather than propagated so the caller's newest-first walk can stop on a
    /// newer block before it reaches them.
    ///
    /// Read off the top of each block's chain ([`clog_trailer_from_rows`],
    /// [`CLOG_TRAILER_ROWS`] rows per block) instead of reconstructing the
    /// page. Blocks the prefix does not settle are reconstructed by one
    /// `get_vectored` over the span from the lowest to the highest of them:
    /// one store round trip, at the cost of rebuilding any settled block in
    /// between — never more than the old all-blocks read. That is the old
    /// path, so its errors and its skipping of row-less blocks are unchanged.
    async fn clog_trailers(
        &self,
        segno: u32,
        blocks: std::ops::Range<u32>,
        probe_lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Vec<Result<Option<TimestampTz>, PageReconstructError>>, PageReconstructError> {
        let rows_by_block = match self.page_writer.as_ref() {
            Some(pw) => pw
                .scan_slru_range(
                    SlruKind::Clog,
                    segno,
                    blocks.clone(),
                    probe_lsn,
                    crate::page_store::SlruScan::NewestUpTo(CLOG_TRAILER_ROWS),
                )
                .await
                .map_err(PageReconstructError::Other)?,
            None => HashMap::new(),
        };
        // Indexed by `blknum - blocks.start`; `None` = not settled by the rows.
        let mut trailers: Vec<Option<Result<Option<TimestampTz>, PageReconstructError>>> =
            Vec::with_capacity(blocks.len());
        let mut unsettled: Option<std::ops::Range<u32>> = None;
        for blknum in blocks.clone() {
            let rows = rows_by_block.get(&blknum).map_or(&[][..], Vec::as_slice);
            trailers.push(match clog_trailer_from_rows(rows) {
                Ok(ClogTrailer::Known(ts)) => Some(Ok(ts)),
                Ok(ClogTrailer::Unknown) => {
                    unsettled = Some(match unsettled {
                        None => blknum..blknum + 1,
                        Some(r) => r.start..blknum + 1,
                    });
                    None
                }
                Err(e) => Some(Err(PageReconstructError::Other(e))),
            });
        }
        if let Some(span) = unsettled {
            let keyspace = KeySpace::single(
                slru_block_to_key(SlruKind::Clog, segno, span.start)
                    ..slru_block_to_key(SlruKind::Clog, segno, span.end),
            );
            let query = VersionedKeySpaceQuery::uniform(keyspace, probe_lsn);
            for (key, page) in self.get_vectored(query, ctx).await? {
                let (_, _, blknum) = key.to_slru_block().map_err(PageReconstructError::Other)?;
                let slot = &mut trailers[(blknum - blocks.start) as usize];
                if slot.is_none() {
                    *slot = Some(page.map(|p| clog_trailer(&p)));
                }
            }
        }
        // A block `get_vectored` did not return has no version at or below
        // `probe_lsn`; it was skipped before and is skipped now.
        Ok(trailers
            .into_iter()
            .rev()
            .map(|t| t.unwrap_or(Ok(None)))
            .collect())
    }

    pub(crate) async fn get_slru_keyspace(
        &self,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<KeySpace, PageReconstructError> {
        let mut accum = KeySpaceAccum::new();

        for kind in SlruKind::iter() {
            let mut segments: Vec<u32> = self
                .list_slru_segments(kind, version, ctx)
                .await?
                .into_iter()
                .collect();
            segments.sort_unstable();

            for seg in segments {
                let block_count = self.get_slru_segment_size(kind, seg, version, ctx).await?;

                accum.add_range(
                    slru_block_to_key(kind, seg, 0)..slru_block_to_key(kind, seg, block_count),
                );
            }
        }

        Ok(accum.to_keyspace())
    }

    /// Get a list of SLRU segments
    pub(crate) async fn list_slru_segments(
        &self,
        kind: SlruKind,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<HashSet<u32>, PageReconstructError> {
        // fetch directory entry
        let key = slru_dir_to_key(kind);

        let buf = version.get(self, key, ctx).await?;
        Ok(SlruSegmentDirectory::des(&buf)?.segments)
    }

    pub(crate) async fn get_relmap_file(
        &self,
        spcnode: Oid,
        dbnode: Oid,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        let key = relmap_file_key(spcnode, dbnode);

        let buf = version.get(self, key, ctx).await?;
        Ok(buf)
    }

    pub(crate) async fn list_dbdirs(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<HashMap<(Oid, Oid), bool>, PageReconstructError> {
        // fetch directory entry
        let buf = self.get(DBDIR_KEY, lsn, ctx).await?;

        Ok(DbDirectory::des(&buf)?.dbdirs)
    }

    pub(crate) async fn get_twophase_file(
        &self,
        xid: u64,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        let key = twophase_file_key(xid);
        let buf = self.get(key, lsn, ctx).await?;
        Ok(buf)
    }

    pub(crate) async fn list_twophase_files(
        &self,
        version: Version<'_>,
        ctx: &RequestContext,
    ) -> Result<HashSet<u64>, PageReconstructError> {
        // fetch directory entry
        let buf = version.get(self, TWOPHASEDIR_KEY, ctx).await?;

        if self.pg_version.has_64bit_twophase() {
            Ok(TwoPhaseDirectoryV17::des(&buf)?.xids)
        } else {
            Ok(TwoPhaseDirectory::des(&buf)?
                .xids
                .iter()
                .map(|x| u64::from(*x))
                .collect())
        }
    }

    pub(crate) async fn get_control_file(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        self.get(CONTROLFILE_KEY, lsn, ctx).await
    }

    pub(crate) async fn get_checkpoint(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        self.get(CHECKPOINT_KEY, lsn, ctx).await
    }

    async fn list_aux_files_v2(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<HashMap<String, Bytes>, PageReconstructError> {
        let kv = self
            .scan(KeySpace::single(Key::metadata_aux_key_range()), lsn, ctx)
            .await?;
        let mut result = HashMap::new();
        let mut sz = 0;
        for (_, v) in kv {
            let v = v?;
            let v = aux_file::decode_file_value_bytes(&v)
                .context("value decode")
                .map_err(PageReconstructError::Other)?;
            for (fname, content) in v {
                sz += fname.len();
                sz += content.len();
                result.insert(fname, content);
            }
        }
        self.aux_file_size_estimator.on_initial(sz);
        Ok(result)
    }

    pub(crate) async fn trigger_aux_file_size_computation(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<(), PageReconstructError> {
        self.list_aux_files_v2(lsn, ctx).await?;
        Ok(())
    }

    pub(crate) async fn list_aux_files(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<HashMap<String, Bytes>, PageReconstructError> {
        self.list_aux_files_v2(lsn, ctx).await
    }

    pub(crate) async fn get_replorigins(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<HashMap<RepOriginId, Lsn>, PageReconstructError> {
        let kv = self
            .scan(KeySpace::single(repl_origin_key_range()), lsn, ctx)
            .await?;
        let mut result = HashMap::new();
        for (k, v) in kv {
            let v = v?;
            if v.is_empty() {
                // This is a tombstone -- we can skip it.
                // Originally, the replorigin code uses `Lsn::INVALID` to represent a tombstone. However, as it part of
                // the sparse keyspace and the sparse keyspace uses an empty image to universally represent a tombstone,
                // we also need to consider that. Such tombstones might be written on the detach ancestor code path to
                // avoid the value going into the child branch. (See [`crate::page_server::timeline::detach_ancestor::generate_tombstone_image_layer`] for more details.)
                continue;
            }
            let origin_id = k.field6 as RepOriginId;
            let origin_lsn = Lsn::des(&v)
                .with_context(|| format!("decode replorigin value for {origin_id}: {v:?}"))?;
            if origin_lsn != Lsn::INVALID {
                result.insert(origin_id, origin_lsn);
            }
        }
        Ok(result)
    }

    /// Does the same as get_current_logical_size but counted on demand.
    /// Used to initialize the logical size tracking on startup.
    ///
    /// Only relation blocks are counted currently. That excludes metadata,
    /// SLRUs, twophase files etc.
    ///
    /// # Cancel-Safety
    ///
    /// This method is cancellation-safe.
    pub(crate) async fn get_current_logical_size_non_incremental(
        &self,
        lsn: Lsn,
        ctx: &RequestContext,
    ) -> Result<u64, CalculateLogicalSizeError> {
        debug_assert_current_span_has_branch_id();

        fail::fail_point!("skip-logical-size-calculation", |_| { Ok(0) });

        // Fetch list of database dirs and iterate them
        let buf = self.get(DBDIR_KEY, lsn, ctx).await?;
        let dbdir = DbDirectory::des(&buf)?;

        let mut total_size: u64 = 0;
        let mut dbdir_cnt = 0;
        let mut rel_cnt = 0;

        for &(spcnode, dbnode) in dbdir.dbdirs.keys() {
            dbdir_cnt += 1;
            for rel in self
                .list_rels(spcnode, dbnode, Version::at(lsn), ctx)
                .await?
            {
                rel_cnt += 1;
                if self.cancel.is_cancelled() {
                    return Err(CalculateLogicalSizeError::Cancelled);
                }
                let relsize_key = rel_size_to_key(rel);
                let mut buf = self.get(relsize_key, lsn, ctx).await?;
                let relsize = buf.get_u32_le();

                total_size += relsize as u64;
            }
        }

        self.db_rel_count
            .store(Some(Arc::new((dbdir_cnt, rel_cnt))));

        Ok(total_size * BLCKSZ as u64)
    }

    /// Get cached size of relation. There are two caches: one for primary updates, it captures the latest state of
    /// of the timeline and snapshot cache, which key includes LSN and so can be used by replicas to get relation size
    /// at the particular LSN (snapshot).
    pub fn get_cached_rel_size(&self, tag: &RelTag, version: Version<'_>) -> Option<BlockNumber> {
        let lsn = version.get_lsn();
        {
            let is_ingest = matches!(version, Version::Modified(_));
            let rel_size_cache = self.rel_size_latest_cache.read().unwrap();
            if let Some((cached_lsn, nblocks)) = rel_size_cache.get(tag) {
                if latest_rel_size_cache_hit(
                    lsn,
                    *cached_lsn,
                    self.rel_size_cache_horizon,
                    is_ingest,
                ) {
                    RELSIZE_LATEST_CACHE_HITS.inc();
                    return Some(*nblocks);
                }
                RELSIZE_CACHE_MISSES_OLD.inc();
            }
        }
        {
            let mut rel_size_cache = self.rel_size_snapshot_cache.lock().unwrap();
            if let Some(nblock) = rel_size_cache.get(&(lsn, *tag)) {
                RELSIZE_SNAPSHOT_CACHE_HITS.inc();
                return Some(*nblock);
            }
        }
        if version.is_latest() {
            RELSIZE_LATEST_CACHE_MISSES.inc();
        } else {
            RELSIZE_SNAPSHOT_CACHE_MISSES.inc();
        }
        None
    }

    /// Update cached relation size if there is no more recent update
    pub fn update_cached_rel_size(&self, tag: RelTag, version: Version<'_>, nblocks: BlockNumber) {
        let lsn = version.get_lsn();
        if version.is_latest() {
            let mut rel_size_cache = self.rel_size_latest_cache.write().unwrap();
            match rel_size_cache.entry(tag) {
                hash_map::Entry::Occupied(mut entry) => {
                    let cached_lsn = entry.get_mut();
                    if lsn >= cached_lsn.0 {
                        *cached_lsn = (lsn, nblocks);
                    }
                }
                hash_map::Entry::Vacant(entry) => {
                    entry.insert((lsn, nblocks));
                    RELSIZE_LATEST_CACHE_ENTRIES.inc();
                }
            }
        } else {
            let mut rel_size_cache = self.rel_size_snapshot_cache.lock().unwrap();
            if rel_size_cache.capacity() != 0 {
                rel_size_cache.insert((lsn, tag), nblocks);
                RELSIZE_SNAPSHOT_CACHE_ENTRIES.set(rel_size_cache.len() as u64);
            }
        }
    }

    /// Store cached relation size. Forward-only: an entry never moves to
    /// an older LSN.
    ///
    /// All callers are ingest-side. During a walreceiver RECONNECT the
    /// stream re-applies `(consistent_lsn, wal_end]` over a LIVE
    /// pageserver, and the per-record seeding in `get_rel_size_in_reldir`
    /// used to rewind the shared entry to the replay position — an
    /// external `nblocks` read arriving mid-replay then satisfied
    /// `request_lsn >= cached_lsn` against the rewound entry and was
    /// served a size from the middle of the window (#240 mode E: a
    /// recycled compute pinned 60 of 80 blocks for its lifetime).
    /// Dropping the backdated store is safe for the replayer itself: its
    /// reads at the replay position miss the (newer) entry and fall
    /// through to an LSN-correct YB read, and the #22 zero-fill hazard
    /// needs the cache to not be BELOW the replay position, which
    /// forward-only preserves.
    pub fn set_cached_rel_size(&self, tag: RelTag, lsn: Lsn, nblocks: BlockNumber) {
        let mut rel_size_cache = self.rel_size_latest_cache.write().unwrap();
        match rel_size_cache.entry(tag) {
            hash_map::Entry::Occupied(mut entry) => {
                if lsn >= entry.get().0 {
                    *entry.get_mut() = (lsn, nblocks);
                }
            }
            hash_map::Entry::Vacant(entry) => {
                entry.insert((lsn, nblocks));
                RELSIZE_LATEST_CACHE_ENTRIES.inc();
            }
        }
    }

    /// Remove cached relation size
    pub fn remove_cached_rel_size(&self, tag: &RelTag) {
        let mut rel_size_cache = self.rel_size_latest_cache.write().unwrap();
        if rel_size_cache.remove(tag).is_some() {
            RELSIZE_LATEST_CACHE_ENTRIES.dec();
        }
    }
}

/// Whether a `rel_size_latest_cache` entry `(cached_lsn, nblocks)` may
/// serve a read at `request_lsn`.
///
/// The latest cache answers any `request_lsn >= cached_lsn` on the
/// premise that ingest updates the entry on every size change, so a
/// cached value is the relation's current size. That premise breaks in
/// the attach replay window: after a restart the walreceiver re-ingests
/// `(consistent_lsn, horizon]` while `last_record_lsn` already sits at
/// `horizon`, so replay-seeded entries (stamped mid-window) describe a
/// past size, yet `request_lsn >= cached_lsn` accepts them for reads at
/// the horizon — a basebackup taken in that window shipped a relation
/// truncated at the replay frontier (#240). External readers
/// (`Version::LsnRange`) therefore never get entries stamped below
/// `horizon`; they fall through to the YB read, which is durably
/// correct at any LSN.
///
/// Ingest reads (`Version::Modified`, `is_ingest`) are exempt: the
/// replaying walreceiver is the one WRITER of those mid-window entries
/// and reads them at its own replay position, where they are exact —
/// the #22 replay semantics (pre-record state + same-pass effects via
/// the cache) depend on that, and gating them would add a YB point
/// read per replayed record for no correctness gain.
#[inline]
fn latest_rel_size_cache_hit(
    request_lsn: Lsn,
    cached_lsn: Lsn,
    horizon: Lsn,
    is_ingest: bool,
) -> bool {
    request_lsn >= cached_lsn && (is_ingest || cached_lsn >= horizon)
}

/// DatadirModification represents an operation to ingest an atomic set of
/// updates to the repository.
///
/// It is created by the 'begin_record' function. It is called for each WAL
/// record, so that all the modifications by a one WAL record appear atomic.
pub struct DatadirModification<'a> {
    /// The timeline this modification applies to. You can access this to
    /// read the state, but note that any pending updates are *not* reflected
    /// in the state in 'tline' yet.
    pub tline: &'a Timeline,

    /// Current LSN of the modification
    lsn: Lsn,

    // The modifications are not applied directly to the underlying key-value store.
    // The put-functions add the modifications here, and they are flushed to the
    // underlying key-value store by the 'finish' function.
    pending_lsns: Vec<Lsn>,
    pending_nblocks: i64,

    /// Metadata writes, indexed by key so that they can be read from not-yet-committed modifications
    /// while ingesting subsequent records. See [`Self::is_data_key`] for the definition of 'metadata'.
    pending_metadata_pages: HashMap<CompactKey, Vec<(Lsn, usize, Value)>>,

    /// Data writes, ready to be flushed into an ephemeral layer. See [`Self::is_data_key`] for
    /// which keys are stored here.
    pending_data_batch: Option<SerializedValueBatch>,

    /// For special "directory" keys that store key-value maps, track the size of the map
    /// if it was updated in this modification.
    pending_directory_entries: Vec<(DirectoryKind, MetricsUpdate)>,

    /// Drop/shrink GC event candidates recorded by this
    /// modification's rel drops / truncations, handed to the
    /// `PageWriter`'s candidate buffer on commit. Empty unless the
    /// writer collects GC candidates (`enable_gc`).
    pending_gc_drop_shrink_events: Vec<crate::page_store::PendingGcRow>,

    /// An **approximation** of how many metadata bytes will be written to the EphemeralFile.
    pending_metadata_bytes: usize,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MetricsUpdate {
    /// Set the metrics to this value
    Set(u64),
    /// Increment the metrics by this value
    Add(u64),
    /// Decrement the metrics by this value
    Sub(u64),
}

impl DatadirModification<'_> {
    // When a DatadirModification is committed, we do a monolithic serialization of all its contents.  WAL records can
    // contain multiple pages, so the pageserver's record-based batch size isn't sufficient to bound this allocation: we
    // additionally specify a limit on how much payload a DatadirModification may contain before it should be committed.
    pub(crate) const MAX_PENDING_BYTES: usize = 8 * 1024 * 1024;

    /// Get the current lsn
    pub(crate) fn get_lsn(&self) -> Lsn {
        self.lsn
    }

    pub(crate) fn approx_pending_bytes(&self) -> usize {
        self.pending_data_batch
            .as_ref()
            .map_or(0, |b| b.buffer_size())
            + self.pending_metadata_bytes
    }

    /// Every sparse-reldir key present for a database, `Removed` tombstones
    /// included, as `(RelTag, marker)`.
    ///
    /// `list_rels` deliberately hides `Removed`, which is right for callers
    /// asking "what exists" but wrong for the reset at CREATE DATABASE, which
    /// has to tombstone the survivors of a previous database on the same oid.
    async fn all_reldir_markers(
        &self,
        spcnode: Oid,
        dbnode: Oid,
        ctx: &RequestContext,
    ) -> Result<Vec<(RelTag, RelDirExists)>, PageReconstructError> {
        let range = rel_tag_sparse_key_range(spcnode, dbnode);
        let results = self
            .tline
            .scan(KeySpace::single(range), self.lsn_before_record(), ctx)
            .await?;
        let mut out = Vec::new();
        for (key, val) in results {
            let Ok(marker) = RelDirExists::decode(&val?) else {
                continue;
            };
            out.push((
                RelTag {
                    spcnode,
                    dbnode,
                    relnode: key.field4,
                    forknum: key.field5,
                },
                marker,
            ));
        }
        // Pending markers from this modification shadow the scan.
        for (tag, marker) in self.pending_reldir_markers(spcnode, dbnode) {
            out.retain(|(t, _)| *t != tag);
            out.push((tag, marker));
        }
        Ok(out)
    }

    /// Sparse-reldir markers written by *this* modification and not yet
    /// committed, for one database.
    ///
    /// `list_rels_v2` scans the Timeline, which cannot see pending writes.
    /// The v1 directory read went through [`Version::get`] → [`Self::get`]
    /// and so picked them up for free; this restores that. Without it a
    /// batch containing `XLOG_SMGR_CREATE` followed by `XLOG_DBASE_DROP`
    /// on the same database loses the new relation: `drop_dbdir` reads at
    /// `Version::Modified` and would not see it, so its size never leaves
    /// `pending_nblocks`.
    pub(crate) fn pending_reldir_markers(
        &self,
        spcnode: Oid,
        dbnode: Oid,
    ) -> Vec<(RelTag, RelDirExists)> {
        let range = rel_tag_sparse_key_range(spcnode, dbnode);
        let mut out = Vec::new();
        for (compact, values) in &self.pending_metadata_pages {
            let key = Key::from_compact(*compact);
            if key < range.start || key >= range.end {
                continue;
            }
            let Some((_, _, Value::Image(img))) = values.last() else {
                continue;
            };
            let Ok(marker) = RelDirExists::decode(img) else {
                continue;
            };
            out.push((
                RelTag {
                    spcnode,
                    dbnode,
                    relnode: key.field4,
                    forknum: key.field5,
                },
                marker,
            ));
        }
        out
    }

    pub(crate) fn has_dirty_data(&self) -> bool {
        self.pending_data_batch
            .as_ref()
            .is_some_and(|b| b.has_data())
    }

    /// Returns statistics about the currently pending modifications.
    pub(crate) fn stats(&self) -> DatadirModificationStats {
        let mut stats = DatadirModificationStats::default();
        for (_, _, value) in self.pending_metadata_pages.values().flatten() {
            match value {
                Value::Image(_) => stats.metadata_images += 1,
                Value::WalRecord(r) if r.will_init() => stats.metadata_images += 1,
                Value::WalRecord(_) => stats.metadata_deltas += 1,
            }
        }
        for valuemeta in self.pending_data_batch.iter().flat_map(|b| &b.metadata) {
            match valuemeta {
                ValueMeta::Serialized(s) if s.will_init => stats.data_images += 1,
                ValueMeta::Serialized(_) => stats.data_deltas += 1,
                ValueMeta::Observed(_) => {}
            }
        }
        stats
    }

    /// Set the current lsn
    pub(crate) fn set_lsn(&mut self, lsn: Lsn) -> Result<(), WalIngestError> {
        ensure_walingest!(
            lsn >= self.lsn,
            "setting an older lsn {} than {} is not allowed",
            lsn,
            self.lsn
        );

        if lsn > self.lsn {
            self.pending_lsns.push(self.lsn);
            self.lsn = lsn;
        }
        Ok(())
    }

    /// In this context, 'metadata' means keys that are only read by the pageserver internally, and 'data' means
    /// keys that represent literal blocks that postgres can read.  So data includes relation blocks and
    /// SLRU blocks, which are read directly by postgres, and everything else is considered metadata.
    ///
    /// The distinction is important because data keys are handled on a fast path where dirty writes are
    /// not readable until this modification is committed, whereas metadata keys are visible for read
    /// via [`Self::get`] as soon as their record has been ingested.
    fn is_data_key(key: &Key) -> bool {
        key.is_block_key()
    }

    /// Initialize a completely new repository.
    ///
    /// This inserts the directory metadata entries that are assumed to
    /// always exist.
    pub fn init_empty(&mut self) -> anyhow::Result<()> {
        let buf = DbDirectory::ser(&DbDirectory {
            dbdirs: HashMap::new(),
        })?;
        self.pending_directory_entries
            .push((DirectoryKind::Db, MetricsUpdate::Set(0)));
        self.put(DBDIR_KEY, Value::Image(buf.into()));

        let buf = if self.tline.pg_version.has_64bit_twophase() {
            TwoPhaseDirectoryV17::ser(&TwoPhaseDirectoryV17 {
                xids: HashSet::new(),
            })
        } else {
            TwoPhaseDirectory::ser(&TwoPhaseDirectory {
                xids: HashSet::new(),
            })
        }?;
        self.pending_directory_entries
            .push((DirectoryKind::TwoPhase, MetricsUpdate::Set(0)));
        self.put(TWOPHASEDIR_KEY, Value::Image(buf.into()));

        let buf: Bytes = SlruSegmentDirectory::ser(&SlruSegmentDirectory::default())?.into();
        let empty_dir = Value::Image(buf);

        // Initialize SLRUs.
        self.put(slru_dir_to_key(SlruKind::Clog), empty_dir.clone());
        self.pending_directory_entries.push((
            DirectoryKind::SlruSegment(SlruKind::Clog),
            MetricsUpdate::Set(0),
        ));
        self.put(
            slru_dir_to_key(SlruKind::MultiXactMembers),
            empty_dir.clone(),
        );
        self.pending_directory_entries.push((
            DirectoryKind::SlruSegment(SlruKind::Clog),
            MetricsUpdate::Set(0),
        ));
        self.put(slru_dir_to_key(SlruKind::MultiXactOffsets), empty_dir);
        self.pending_directory_entries.push((
            DirectoryKind::SlruSegment(SlruKind::MultiXactOffsets),
            MetricsUpdate::Set(0),
        ));

        Ok(())
    }

    #[cfg(test)]
    pub fn init_empty_test_timeline(&mut self) -> anyhow::Result<()> {
        self.init_empty()?;
        self.put_control_file(bytes::Bytes::from_static(
            b"control_file contents do not matter",
        ))
        .context("put_control_file")?;
        self.put_checkpoint(bytes::Bytes::from_static(
            b"checkpoint_file contents do not matter",
        ))
        .context("put_checkpoint_file")?;
        Ok(())
    }

    /// Creates a relation if it is not already present.
    /// Returns the current size of the relation
    pub(crate) async fn create_relation_if_required(
        &mut self,
        rel: RelTag,
        ctx: &RequestContext,
    ) -> Result<u32, WalIngestError> {
        // Get current size and put rel creation if rel doesn't exist
        //
        // NOTE: we check the cache first even though get_rel_exists and get_rel_size would
        //       check the cache too. This is because eagerly checking the cache results in
        //       less work overall and 10% better performance. It's more work on cache miss
        //       but cache miss is rare.
        if let Some(nblocks) = self
            .tline
            .get_cached_rel_size(&rel, Version::Modified(self))
        {
            return Ok(nblocks);
        }
        if !self
            .tline
            .get_rel_exists(rel, Version::Modified(self), ctx)
            .await?
        {
            // create it with 0 size initially, the logic below will extend it
            self.put_rel_creation(rel, 0, ctx).await?;
            Ok(0)
        } else {
            Ok(self
                .tline
                .get_rel_size(rel, Version::Modified(self), ctx)
                .await?)
        }
    }

    /// Given a block number for a relation (which represents a newly written block),
    /// and the previous block count of the relation, find the gaps
    /// that were created by the newly written block if any.
    fn find_gaps(rel: RelTag, blkno: u32, previous_nblocks: u32) -> Option<KeySpace> {
        let mut key = rel_block_to_key(rel, blkno);
        let mut gap_accum = None;

        for gap_blkno in previous_nblocks..blkno {
            key.field6 = gap_blkno;

            gap_accum
                .get_or_insert_with(KeySpaceAccum::new)
                .add_key(key);
        }

        gap_accum.map(|accum| accum.to_keyspace())
    }

    pub async fn ingest_batch(
        &mut self,
        mut batch: SerializedValueBatch,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        let mut gaps_at_lsns = Vec::default();

        for meta in batch.metadata.iter() {
            let key = Key::from_compact(meta.key());
            let (rel, blkno) = key
                .to_rel_block()
                .map_err(|_| WalIngestErrorKind::InvalidKey(key, meta.lsn()))?;
            let new_nblocks = blkno + 1;

            let old_nblocks = self.create_relation_if_required(rel, ctx).await?;
            if new_nblocks > old_nblocks {
                self.put_rel_extend(rel, new_nblocks, ctx).await?;
            }

            if let Some(gaps) = Self::find_gaps(rel, blkno, old_nblocks) {
                gaps_at_lsns.push((gaps, meta.lsn()));
            }
        }

        if !gaps_at_lsns.is_empty() {
            batch.zero_gaps(gaps_at_lsns);
        }

        match self.pending_data_batch.as_mut() {
            Some(pending_batch) => {
                pending_batch.extend(batch);
            }
            None if batch.has_data() => {
                self.pending_data_batch = Some(batch);
            }
            None => {
                // Nothing to initialize the batch with
            }
        }

        Ok(())
    }

    /// Put a new page version that can be constructed from a WAL record
    ///
    /// NOTE: this will *not* implicitly extend the relation, if the page is beyond the
    /// current end-of-file. It's up to the caller to check that the relation size
    /// matches the blocks inserted!
    pub fn put_rel_wal_record(
        &mut self,
        rel: RelTag,
        blknum: BlockNumber,
        rec: NeonWalRecord,
    ) -> Result<(), WalIngestError> {
        ensure_walingest!(rel.relnode != 0, RelationError::InvalidRelnode);
        self.put(rel_block_to_key(rel, blknum), Value::WalRecord(rec));
        Ok(())
    }

    // Same, but for an SLRU.
    pub fn put_slru_wal_record(
        &mut self,
        kind: SlruKind,
        segno: u32,
        blknum: BlockNumber,
        rec: NeonWalRecord,
    ) -> Result<(), WalIngestError> {
        self.put(
            slru_block_to_key(kind, segno, blknum),
            Value::WalRecord(rec),
        );
        Ok(())
    }

    /// Like put_wal_record, but with ready-made image of the page.
    pub fn put_rel_page_image(
        &mut self,
        rel: RelTag,
        blknum: BlockNumber,
        img: Bytes,
    ) -> Result<(), WalIngestError> {
        ensure_walingest!(rel.relnode != 0, RelationError::InvalidRelnode);
        let key = rel_block_to_key(rel, blknum);
        if !key.is_valid_key_on_write_path() {
            Err(WalIngestErrorKind::InvalidKey(key, self.lsn))?;
        }
        self.put(rel_block_to_key(rel, blknum), Value::Image(img));
        Ok(())
    }

    pub fn put_slru_page_image(
        &mut self,
        kind: SlruKind,
        segno: u32,
        blknum: BlockNumber,
        img: Bytes,
    ) -> Result<(), WalIngestError> {
        let key = slru_block_to_key(kind, segno, blknum);
        if !key.is_valid_key_on_write_path() {
            Err(WalIngestErrorKind::InvalidKey(key, self.lsn))?;
        }
        self.put(key, Value::Image(img));
        Ok(())
    }

    pub(crate) fn put_rel_page_image_zero(
        &mut self,
        rel: RelTag,
        blknum: BlockNumber,
    ) -> Result<(), WalIngestError> {
        ensure_walingest!(rel.relnode != 0, RelationError::InvalidRelnode);
        let key = rel_block_to_key(rel, blknum);
        if !key.is_valid_key_on_write_path() {
            Err(WalIngestErrorKind::InvalidKey(key, self.lsn))?;
        }

        let batch = self
            .pending_data_batch
            .get_or_insert_with(SerializedValueBatch::default);

        batch.put(key.to_compact(), Value::Image(ZERO_PAGE.clone()), self.lsn);

        Ok(())
    }

    pub(crate) fn put_slru_page_image_zero(
        &mut self,
        kind: SlruKind,
        segno: u32,
        blknum: BlockNumber,
    ) -> Result<(), WalIngestError> {
        let key = slru_block_to_key(kind, segno, blknum);
        if !key.is_valid_key_on_write_path() {
            Err(WalIngestErrorKind::InvalidKey(key, self.lsn))?;
        }

        let batch = self
            .pending_data_batch
            .get_or_insert_with(SerializedValueBatch::default);

        batch.put(key.to_compact(), Value::Image(ZERO_PAGE.clone()), self.lsn);

        Ok(())
    }

    /// Store a relmapper file (pg_filenode.map) in the repository
    pub async fn put_relmap_file(
        &mut self,
        spcnode: Oid,
        dbnode: Oid,
        img: Bytes,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        // Add it to the directory (if it doesn't exist already)
        let buf = self.get(DBDIR_KEY, ctx).await?;
        let mut dbdir = DbDirectory::des(&buf)?;

        let r = dbdir.dbdirs.insert((spcnode, dbnode), true);
        if r.is_none() || r == Some(false) {
            // The dbdir entry didn't exist, or it contained a
            // 'false'. The 'insert' call already updated it with
            // 'true', now write the updated 'dbdirs' map back.
            let buf = DbDirectory::ser(&dbdir)?;
            self.put(DBDIR_KEY, Value::Image(buf.into()));
        }
        if r.is_none() {
            self.pending_directory_entries
                .push((DirectoryKind::RelV2, MetricsUpdate::Set(0)));

            // A fresh dbdir entry means this database is new — but Postgres
            // reuses OIDs, so the sparse-reldir range may still hold markers
            // from a previous database with the same oid. Their removal is
            // queued asynchronously at DROP DATABASE and gated on the GC
            // cutoff, so they can easily still be here. v1 got this reset
            // for free: it wrote an *empty* RelDirectory, which replaced the
            // old set wholesale. Per-relation keys have no such single point,
            // so tombstone the survivors explicitly — otherwise
            // `get_rel_exists` resurrects relations that do not exist and
            // basebackup emits them.
            for (tag, marker) in self.all_reldir_markers(spcnode, dbnode, ctx).await? {
                if marker == RelDirExists::Exists {
                    self.put(
                        rel_tag_sparse_key(tag.spcnode, tag.dbnode, tag.relnode, tag.forknum),
                        Value::Image(RelDirExists::Removed.encode()),
                    );
                }
            }
        }

        self.put(relmap_file_key(spcnode, dbnode), Value::Image(img));
        Ok(())
    }

    pub async fn put_twophase_file(
        &mut self,
        xid: u64,
        img: Bytes,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        // Add it to the directory entry
        let dirbuf = self.get(TWOPHASEDIR_KEY, ctx).await?;
        let newdirbuf = if self.tline.pg_version.has_64bit_twophase() {
            let mut dir = TwoPhaseDirectoryV17::des(&dirbuf)?;
            if !dir.xids.insert(xid) {
                Err(WalIngestErrorKind::FileAlreadyExists(xid))?;
            }
            self.pending_directory_entries.push((
                DirectoryKind::TwoPhase,
                MetricsUpdate::Set(dir.xids.len() as u64),
            ));
            Bytes::from(TwoPhaseDirectoryV17::ser(&dir)?)
        } else {
            let xid = xid as u32;
            let mut dir = TwoPhaseDirectory::des(&dirbuf)?;
            if !dir.xids.insert(xid) {
                Err(WalIngestErrorKind::FileAlreadyExists(xid.into()))?;
            }
            self.pending_directory_entries.push((
                DirectoryKind::TwoPhase,
                MetricsUpdate::Set(dir.xids.len() as u64),
            ));
            Bytes::from(TwoPhaseDirectory::ser(&dir)?)
        };
        self.put(TWOPHASEDIR_KEY, Value::Image(newdirbuf));

        self.put(twophase_file_key(xid), Value::Image(img));
        Ok(())
    }

    pub async fn set_replorigin(
        &mut self,
        origin_id: RepOriginId,
        origin_lsn: Lsn,
    ) -> Result<(), WalIngestError> {
        let key = repl_origin_key(origin_id);
        self.put(key, Value::Image(origin_lsn.ser().unwrap().into()));
        Ok(())
    }

    pub async fn drop_replorigin(&mut self, origin_id: RepOriginId) -> Result<(), WalIngestError> {
        self.set_replorigin(origin_id, Lsn::INVALID).await
    }

    pub fn put_control_file(&mut self, img: Bytes) -> Result<(), WalIngestError> {
        self.put(CONTROLFILE_KEY, Value::Image(img));
        Ok(())
    }

    pub fn put_checkpoint(&mut self, img: Bytes) -> Result<(), WalIngestError> {
        self.put(CHECKPOINT_KEY, Value::Image(img));
        Ok(())
    }

    /// Whether GC is on (`enable_gc`) for the timeline being modified —
    /// the layering bridge to the writer's frozen master switch. Gates
    /// drop/shrink event recording.
    fn gc_enabled(&self) -> bool {
        self.tline.gc_enabled()
    }

    /// Record a drop (`GcKind::Drop*`) / shrink (`GcKind::ShrinkPages`) GC
    /// event naming the object `key` at the modification's current LSN —
    /// the drop/truncation record being ingested. Buffered on the
    /// modification and handed to the writer at commit (WAL-driven,
    /// durable); not sampled (drop/shrink are one-shot events, unlike
    /// per-base trims). No-op unless GC is on (`enable_gc`).
    fn record_gc_drop_shrink_event(
        &mut self,
        key: pageserver_api::key::Key,
        gc_kind: crate::page_store::GcKind,
        relsize: Option<u32>,
    ) {
        if !self.gc_enabled() {
            return;
        }
        let row = crate::page_store::PendingGcRow {
            key,
            gc_kind,
            due_lsn: self.lsn,
            relsize,
        };
        // The drain refuses a row whose key shape doesn't match its kind, so a
        // mispaired emitter leaks silently. Fail in CI instead.
        debug_assert!(
            crate::page_server::timeline::Timeline::classify_candidate(&row).is_ok(),
            "emitter enqueued a candidate the drain will refuse: {row:?}"
        );
        self.pending_gc_drop_shrink_events.push(row);
    }

    pub async fn drop_dbdir(
        &mut self,
        spcnode: Oid,
        dbnode: Oid,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        let total_blocks = self
            .tline
            .get_db_size(spcnode, dbnode, Version::Modified(self), ctx)
            .await?;

        // One GC candidate for the whole database, keyed by its relmap key:
        // the drain sweeps the `(spcnode, dbnode)` range of `pages` and of the
        // db's two `meta` prefixes. A range covers what a list cannot — the
        // sparse-reldir tombstones earlier DROP TABLEs left, which would
        // otherwise leak forever — and keeps the enumeration and its thousands
        // of INSERTs off the ingest path applying this record.
        self.record_gc_drop_shrink_event(
            relmap_file_key(spcnode, dbnode),
            crate::page_store::GcKind::DropDb,
            None,
        );

        // Remove entry from dbdir
        let buf = self.get(DBDIR_KEY, ctx).await?;
        let mut dir = DbDirectory::des(&buf)?;
        if dir.dbdirs.remove(&(spcnode, dbnode)).is_some() {
            let buf = DbDirectory::ser(&dir)?;
            self.pending_directory_entries.push((
                DirectoryKind::Db,
                MetricsUpdate::Set(dir.dbdirs.len() as u64),
            ));
            self.put(DBDIR_KEY, Value::Image(buf.into()));
        } else {
            warn!(
                "dropped dbdir for spcnode {} dbnode {} did not exist in db directory",
                spcnode, dbnode
            );
        }

        // Update logical database size.
        self.pending_nblocks -= total_blocks as i64;

        Ok(())
    }

    /// Mark a relation as existing in the reldir keyspace.
    ///
    /// Write-only and infallible: one idempotent upsert, no read.
    fn put_rel_creation_v2(&mut self, rel: RelTag, dbdir_exists: bool) {
        let sparse_rel_dir_key =
            rel_tag_sparse_key(rel.spcnode, rel.dbnode, rel.relnode, rel.forknum);

        // Write the marker unconditionally — no read first.
        //
        // This used to `sparse_get` the key to learn whether the relation
        // already existed. With one key per relation that read is a
        // guaranteed meta-cache miss (the key does not exist yet) and so a
        // YB round trip *per relation created*, where v1 re-read one hot
        // directory key that the cache and the pending modification served.
        // On a create-heavy workload it put ingest far enough behind that
        // `oid-overflow` timed out waiting for WAL to land.
        //
        // It is not needed. The write is an idempotent upsert of the same
        // `Exists` image, and every caller has already established the
        // relation is absent — `create_relation_if_required` does its own
        // `get_rel_exists` immediately before, and the bootstrap-import and
        // dbase-create paths are populating a fresh database.
        self.put(
            sparse_rel_dir_key,
            Value::Image(RelDirExists::Exists.encode()),
        );
        if !dbdir_exists {
            self.pending_directory_entries
                .push((DirectoryKind::RelV2, MetricsUpdate::Set(0)));
        }
        // Approximate: without the read we cannot tell a fresh create from a
        // replayed one, so a replay below the durable horizon can double
        // count. This feeds `TimelineInfo.directory_entries_counts` only —
        // nothing keys off it — and an exact count is not worth a round trip
        // per relation.
        self.pending_directory_entries
            .push((DirectoryKind::RelV2, MetricsUpdate::Add(1)));
    }

    /// Create a relation fork.
    ///
    /// 'nblocks' is the initial size.
    pub async fn put_rel_creation(
        &mut self,
        rel: RelTag,
        nblocks: BlockNumber,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        if rel.relnode == 0 {
            Err(WalIngestErrorKind::LogicalError(anyhow::anyhow!(
                "invalid relnode"
            )))?;
        }
        // It's possible that this is the first rel for this db in this
        // tablespace.  Create the reldir entry for it if so.
        let mut dbdir = DbDirectory::des(&self.get(DBDIR_KEY, ctx).await?)?;

        let dbdir_exists =
            if let hash_map::Entry::Vacant(e) = dbdir.dbdirs.entry((rel.spcnode, rel.dbnode)) {
                // Didn't exist. Update dbdir
                e.insert(false);
                let buf = DbDirectory::ser(&dbdir)?;
                self.pending_directory_entries.push((
                    DirectoryKind::Db,
                    MetricsUpdate::Set(dbdir.dbdirs.len() as u64),
                ));
                self.put(DBDIR_KEY, Value::Image(buf.into()));
                false
            } else {
                true
            };

        self.put_rel_creation_v2(rel, dbdir_exists);

        // Put size
        let size_key = rel_size_to_key(rel);
        let buf = nblocks.to_le_bytes();
        self.put(size_key, Value::Image(Bytes::from(buf.to_vec())));

        self.pending_nblocks += nblocks as i64;

        // Update relation size cache
        self.tline.set_cached_rel_size(rel, self.lsn, nblocks);

        // Even if nblocks > 0, we don't insert any actual blocks here. That's up to the
        // caller.
        Ok(())
    }

    /// Truncate relation
    pub async fn put_rel_truncation(
        &mut self,
        rel: RelTag,
        nblocks: BlockNumber,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        ensure_walingest!(rel.relnode != 0, RelationError::InvalidRelnode);
        if self
            .tline
            .get_rel_exists(rel, Version::Modified(self), ctx)
            .await?
        {
            let size_key = rel_size_to_key(rel);
            // Fetch the old size first
            let old_size = self.get(size_key, ctx).await?.get_u32_le();

            // Update the entry with the new size.
            let buf = nblocks.to_le_bytes();
            self.put(size_key, Value::Image(Bytes::from(buf.to_vec())));

            // Update relation size cache
            self.tline.set_cached_rel_size(rel, self.lsn, nblocks);

            // Update logical database size.
            self.pending_nblocks -= old_size as i64 - nblocks as i64;

            // GC shrink event: the tail at/past `nblocks` becomes
            // collectable once the cutoff passes this truncation.
            self.record_gc_drop_shrink_event(
                rel_size_to_key(rel),
                crate::page_store::GcKind::ShrinkPages,
                Some(nblocks),
            );
        }
        Ok(())
    }

    /// Extend relation
    /// If new size is smaller, do nothing.
    ///
    /// See [`rel_extend_plan`] for the persist/cache decision.
    pub async fn put_rel_extend(
        &mut self,
        rel: RelTag,
        nblocks: BlockNumber,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        ensure_walingest!(rel.relnode != 0, RelationError::InvalidRelnode);

        let size_key = rel_size_to_key(rel);
        // #36: a torn-recovery reldir can list an FSM/VM fork whose rel-size
        // key is absent at this LSN (the directory write landed but the first
        // size write did not), and recovery can then replay an extension of
        // that fork. FSM/VM are advisory (rebuilt by VACUUM), so treat a
        // missing size as 0 rather than letting `MissingKey` wedge the
        // walreceiver on the extend. Same spirit as the get_rel_size / drop
        // tolerances; a missing MAIN-fork size still surfaces as an error.
        let old_size = match self.get(size_key, ctx).await {
            Ok(mut buf) => buf.get_u32_le(),
            Err(PageReconstructError::MissingKey(_))
                if rel.forknum == FSM_FORKNUM || rel.forknum == VISIBILITYMAP_FORKNUM =>
            {
                0
            }
            Err(e) => return Err(e.into()),
        };

        let plan = rel_extend_plan(old_size, nblocks);
        if let Some(new_size) = plan.write_size {
            let buf = new_size.to_le_bytes();
            self.put(size_key, Value::Image(Bytes::from(buf.to_vec())));
            self.pending_nblocks += new_size as i64 - old_size as i64;
        }
        self.tline
            .set_cached_rel_size(rel, self.lsn, plan.cache_size);
        Ok(())
    }

    async fn put_rel_drop_v2(
        &mut self,
        drop_relations: HashMap<(u32, u32), Vec<RelTag>>,
        ctx: &RequestContext,
    ) -> Result<BTreeSet<RelTag>, WalIngestError> {
        let mut dropped_rels = BTreeSet::new();
        for ((spc_node, db_node), rel_tags) in drop_relations {
            for rel_tag in rel_tags {
                let key = rel_tag_sparse_key(spc_node, db_node, rel_tag.relnode, rel_tag.forknum);
                let val = RelDirExists::decode_option(self.sparse_get(key, ctx).await?)
                    .map_err(|_| WalIngestErrorKind::InvalidKey(key, self.lsn))?;
                if val == RelDirExists::Exists {
                    dropped_rels.insert(rel_tag);
                    self.pending_directory_entries
                        .push((DirectoryKind::RelV2, MetricsUpdate::Sub(1)));
                    // put tombstone
                    self.put(key, Value::Image(RelDirExists::Removed.encode()));

                    // Size bookkeeping. This lived only in the v1 drop path
                    // while v1 was the source of truth and v2 was written for
                    // comparison; with v2 as the only path it has to happen
                    // here or the logical size drifts and a dropped rel keeps
                    // a stale cached size — which `create_relation_if_required`
                    // consults *before* `get_rel_exists`, so it would hand back
                    // a block count for a relation that no longer exists.
                    let size_key = rel_size_to_key(rel_tag);
                    // #36: a torn-recovery reldir can list an FSM/VM fork whose
                    // rel-size key is absent at this LSN (the exists marker
                    // landed but the first size write did not). FSM/VM are
                    // advisory (rebuilt by VACUUM), so treat a missing size as 0
                    // rather than letting `MissingKey` wedge the walreceiver on
                    // the drop. A missing MAIN-fork size still surfaces as an
                    // error.
                    let old_size = match self.get(size_key, ctx).await {
                        Ok(mut buf) => buf.get_u32_le(),
                        Err(PageReconstructError::MissingKey(_))
                            if rel_tag.forknum == FSM_FORKNUM
                                || rel_tag.forknum == VISIBILITYMAP_FORKNUM =>
                        {
                            0
                        }
                        Err(e) => return Err(e.into()),
                    };
                    self.pending_nblocks -= old_size as i64;
                    self.tline.remove_cached_rel_size(&rel_tag);
                }
            }
        }
        Ok(dropped_rels)
    }

    /// Drop some relations
    pub(crate) async fn put_rel_drops(
        &mut self,
        drop_relations: HashMap<(u32, u32), Vec<RelTag>>,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        let dropped_rels = self.put_rel_drop_v2(drop_relations, ctx).await?;
        // One GC candidate per relation, not per fork. The drop's own
        // granularity is the relfilenode — `ingest_xlog_xact_record` expands
        // each xnode to every forknum, and `put_rel_drop_v2` narrows that to
        // the forks whose marker existed — so a candidate keyed by
        // `(spcnode, dbnode, relnode)` names exactly what was dropped, and the
        // drain sweeps the forks as one contiguous range per bucket.
        //
        // Collapsed through a set rather than by skipping adjacent repeats:
        // `RelTag`'s ordering puts `forknum` last only because of a hand-written
        // `Ord`, and its own comment invites deriving one instead — which would
        // order by the struct's first field, `forknum`, and scatter the forks.
        // The key carries the main fork's `forknum`; the act does not read it.
        let relations: std::collections::BTreeSet<(Oid, Oid, Oid)> = dropped_rels
            .iter()
            .map(|rel| (rel.spcnode, rel.dbnode, rel.relnode))
            .collect();
        for (spcnode, dbnode, relnode) in relations {
            self.record_gc_drop_shrink_event(
                rel_size_to_key(RelTag {
                    forknum: MAIN_FORKNUM,
                    spcnode,
                    dbnode,
                    relnode,
                }),
                crate::page_store::GcKind::DropRel,
                None,
            );
        }
        Ok(())
    }

    pub async fn put_slru_segment_creation(
        &mut self,
        kind: SlruKind,
        segno: u32,
        nblocks: BlockNumber,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        // Add it to the directory entry
        let dir_key = slru_dir_to_key(kind);
        let buf = self.get(dir_key, ctx).await?;
        let mut dir = SlruSegmentDirectory::des(&buf)?;

        if !dir.segments.insert(segno) {
            Err(WalIngestErrorKind::SlruAlreadyExists(kind, segno))?;
        }
        self.pending_directory_entries.push((
            DirectoryKind::SlruSegment(kind),
            MetricsUpdate::Set(dir.segments.len() as u64),
        ));
        self.put(
            dir_key,
            Value::Image(Bytes::from(SlruSegmentDirectory::ser(&dir)?)),
        );

        // Put size
        let size_key = slru_segment_size_to_key(kind, segno);
        let buf = nblocks.to_le_bytes();
        self.put(size_key, Value::Image(Bytes::from(buf.to_vec())));

        // even if nblocks > 0, we don't insert any actual blocks here

        Ok(())
    }

    /// Extend SLRU segment
    pub fn put_slru_extend(
        &mut self,
        kind: SlruKind,
        segno: u32,
        nblocks: BlockNumber,
    ) -> Result<(), WalIngestError> {
        // Put size
        let size_key = slru_segment_size_to_key(kind, segno);
        let buf = nblocks.to_le_bytes();
        self.put(size_key, Value::Image(Bytes::from(buf.to_vec())));
        Ok(())
    }

    /// This method is used for marking truncated SLRU files
    pub async fn drop_slru_segment(
        &mut self,
        kind: SlruKind,
        segno: u32,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        // Remove it from the directory entry
        let dir_key = slru_dir_to_key(kind);
        let buf = self.get(dir_key, ctx).await?;
        let mut dir = SlruSegmentDirectory::des(&buf)?;

        if !dir.segments.remove(&segno) {
            warn!("slru segment {:?}/{} does not exist", kind, segno);
        }
        self.pending_directory_entries.push((
            DirectoryKind::SlruSegment(kind),
            MetricsUpdate::Set(dir.segments.len() as u64),
        ));
        self.put(
            dir_key,
            Value::Image(Bytes::from(SlruSegmentDirectory::ser(&dir)?)),
        );

        // GC drop event: reclaim the segment's `slru_pages` rows + size
        // sentinel once the cutoff passes this truncation.
        // `<= due_lsn` keeps a wraparound-recreated segno alive. The key
        // is the segment's size sentinel (SLRU keyspace).
        self.record_gc_drop_shrink_event(
            slru_segment_size_to_key(kind, segno),
            crate::page_store::GcKind::DropSlru,
            None,
        );

        Ok(())
    }

    /// Drop a relmapper file (pg_filenode.map)
    pub fn drop_relmap_file(&mut self, _spcnode: Oid, _dbnode: Oid) -> Result<(), WalIngestError> {
        // TODO
        Ok(())
    }

    /// This method is used for marking truncated SLRU files
    pub async fn drop_twophase_file(
        &mut self,
        xid: u64,
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        // Remove it from the directory entry
        let buf = self.get(TWOPHASEDIR_KEY, ctx).await?;
        let newdirbuf = if self.tline.pg_version.has_64bit_twophase() {
            let mut dir = TwoPhaseDirectoryV17::des(&buf)?;

            if !dir.xids.remove(&xid) {
                warn!("twophase file for xid {} does not exist", xid);
            }
            self.pending_directory_entries.push((
                DirectoryKind::TwoPhase,
                MetricsUpdate::Set(dir.xids.len() as u64),
            ));
            Bytes::from(TwoPhaseDirectoryV17::ser(&dir)?)
        } else {
            let xid: u32 = u32::try_from(xid)
                .map_err(|e| WalIngestErrorKind::LogicalError(anyhow::Error::from(e)))?;
            let mut dir = TwoPhaseDirectory::des(&buf)?;

            if !dir.xids.remove(&xid) {
                warn!("twophase file for xid {} does not exist", xid);
            }
            self.pending_directory_entries.push((
                DirectoryKind::TwoPhase,
                MetricsUpdate::Set(dir.xids.len() as u64),
            ));
            Bytes::from(TwoPhaseDirectory::ser(&dir)?)
        };
        self.put(TWOPHASEDIR_KEY, Value::Image(newdirbuf));

        // GC drop event: reclaim the twophase file's meta row once the
        // cutoff passes this resolve.
        self.record_gc_drop_shrink_event(
            twophase_file_key(xid),
            crate::page_store::GcKind::DropMeta,
            None,
        );

        Ok(())
    }

    pub async fn put_file(
        &mut self,
        path: &str,
        content: &[u8],
        ctx: &RequestContext,
    ) -> Result<(), WalIngestError> {
        let key = aux_file::encode_aux_file_key(path);
        // retrieve the key from the engine
        let old_val = match self.get(key, ctx).await {
            Ok(val) => Some(val),
            Err(PageReconstructError::MissingKey(_)) => None,
            Err(e) => return Err(e.into()),
        };
        let files: Vec<(&str, &[u8])> = if let Some(ref old_val) = old_val {
            aux_file::decode_file_value(old_val).map_err(WalIngestErrorKind::EncodeAuxFileError)?
        } else {
            Vec::new()
        };
        let mut other_files = Vec::with_capacity(files.len());
        let mut modifying_file = None;
        for file @ (p, content) in files {
            if path == p {
                assert!(
                    modifying_file.is_none(),
                    "duplicated entries found for {path}"
                );
                modifying_file = Some(content);
            } else {
                other_files.push(file);
            }
        }
        let mut new_files = other_files;
        match (modifying_file, content.is_empty()) {
            (Some(old_content), false) => {
                self.tline
                    .aux_file_size_estimator
                    .on_update(old_content.len(), content.len());
                new_files.push((path, content));
            }
            (Some(old_content), true) => {
                self.tline
                    .aux_file_size_estimator
                    .on_remove(old_content.len());
                // not adding the file key to the final `new_files` vec.
            }
            (None, false) => {
                self.tline.aux_file_size_estimator.on_add(content.len());
                new_files.push((path, content));
            }
            // Compute may request delete of old version of pgstat AUX file if new one exceeds size limit.
            // Compute doesn't know if previous version of this file exists or not, so
            // attempt to delete non-existing file can cause this message.
            // To avoid false alarms, log it as info rather than warning.
            (None, true) if path.starts_with("pg_stat/") => {
                info!("removing non-existing pg_stat file: {}", path)
            }
            (None, true) => warn!("removing non-existing aux file: {}", path),
        }
        let new_val = aux_file::encode_file_value(&new_files)
            .map_err(WalIngestErrorKind::EncodeAuxFileError)?;
        self.put(key, Value::Image(new_val.into()));

        Ok(())
    }

    ///
    /// Flush changes accumulated so far to the underlying repository.
    ///
    /// Usually, changes made in DatadirModification are atomic, but this allows
    /// you to flush them to the underlying repository before the final `commit`.
    /// That allows to free up the memory used to hold the pending changes.
    ///
    /// Currently only used during bulk import of a data directory. In that
    /// context, breaking the atomicity is OK. If the import is interrupted, the
    /// whole import fails and the timeline will be deleted anyway.
    /// (Or to be precise, it will be left behind for debugging purposes and
    /// ignored, see <https://github.com/neondatabase/neon/pull/1809>)
    ///
    /// Note: A consequence of flushing the pending operations is that they
    /// won't be visible to subsequent operations until `commit`. The function
    /// retains all the metadata, but data pages are flushed. That's again OK
    /// for bulk import, where you are just loading data pages and won't try to
    /// modify the same pages twice.
    pub(crate) async fn flush(&mut self, ctx: &RequestContext) -> anyhow::Result<()> {
        // Unless we have accumulated a decent amount of changes, it's not worth it
        // to scan through the pending_updates list.
        let pending_nblocks = self.pending_nblocks;
        if pending_nblocks < 10000 {
            return Ok(());
        }

        let mut writer = self.tline.writer().await;

        // Flush relation and  SLRU data blocks, keep metadata.
        if let Some(batch) = self.pending_data_batch.take() {
            tracing::debug!(
                "Flushing batch with max_lsn={}. Last record LSN is {}",
                batch.max_lsn,
                self.tline.get_last_record_lsn()
            );

            // This bails out on first error without modifying pending_updates.
            // That's Ok, cf this function's doc comment.
            writer.put_batch(batch, ctx).await?;
        }

        if pending_nblocks != 0 {
            writer.update_current_logical_size(pending_nblocks * i64::from(BLCKSZ));
            self.pending_nblocks = 0;
        }

        for (kind, count) in std::mem::take(&mut self.pending_directory_entries) {
            writer.update_directory_entries_count(kind, count);
        }

        Ok(())
    }

    ///
    /// Finish this atomic update, writing all the updated keys to the
    /// underlying timeline.
    /// All the modifications in this atomic update are stamped by the specified LSN.
    ///
    pub async fn commit(&mut self, ctx: &RequestContext) -> anyhow::Result<()> {
        let mut writer = self.tline.writer().await;

        let pending_nblocks = self.pending_nblocks;
        self.pending_nblocks = 0;

        // Ordering: the items in this batch do not need to be in any global order, but values for
        // a particular Key must be in Lsn order relative to one another.  InMemoryLayer relies on
        // this to do efficient updates to its index.  See [`wal_decoder::serialized_batch`] for
        // more details.

        let metadata_batch = {
            let pending_meta = self
                .pending_metadata_pages
                .drain()
                .flat_map(|(key, values)| {
                    values
                        .into_iter()
                        .map(move |(lsn, value_size, value)| (key, lsn, value_size, value))
                })
                .collect::<Vec<_>>();

            if pending_meta.is_empty() {
                None
            } else {
                Some(SerializedValueBatch::from_values(pending_meta))
            }
        };

        let data_batch = self.pending_data_batch.take();

        let maybe_batch = match (data_batch, metadata_batch) {
            (Some(mut data), Some(metadata)) => {
                data.extend(metadata);
                Some(data)
            }
            (Some(data), None) => Some(data),
            (None, Some(metadata)) => Some(metadata),
            (None, None) => None,
        };

        if let Some(batch) = maybe_batch {
            tracing::debug!(
                "Flushing batch with max_lsn={}. Last record LSN is {}",
                batch.max_lsn,
                self.tline.get_last_record_lsn()
            );

            // This bails out on first error without modifying pending_updates.
            // That's Ok, cf this function's doc comment.
            writer.put_batch(batch, ctx).await?;
        }

        // Drop/shrink GC events ride the writer's candidate buffer,
        // durable with a window flush like ingest trim candidates: a
        // buffer lost to a crash implies consistent_lsn didn't advance,
        // so the producing record replays and re-emits (PK dedup).
        if !self.pending_gc_drop_shrink_events.is_empty() {
            if let Some(pw) = self.tline.page_writer.as_ref() {
                pw.push_gc_drop_shrink_events(&self.pending_gc_drop_shrink_events);
            }
            self.pending_gc_drop_shrink_events.clear();
        }

        self.pending_lsns.push(self.lsn);
        for pending_lsn in self.pending_lsns.drain(..) {
            // TODO(vlad): pretty sure the comment below is not valid anymore
            // and we can call finish write with the latest LSN
            //
            // Ideally, we should be able to call writer.finish_write() only once
            // with the highest LSN. However, the last_record_lsn variable in the
            // timeline keeps track of the latest LSN and the immediate previous LSN
            // so we need to record every LSN to not leave a gap between them.
            writer.finish_write(pending_lsn);
        }

        if pending_nblocks != 0 {
            writer.update_current_logical_size(pending_nblocks * i64::from(BLCKSZ));
        }

        for (kind, count) in std::mem::take(&mut self.pending_directory_entries) {
            writer.update_directory_entries_count(kind, count);
        }

        self.pending_metadata_bytes = 0;

        Ok(())
    }

    pub(crate) fn len(&self) -> usize {
        self.pending_metadata_pages.len() + self.pending_data_batch.as_ref().map_or(0, |b| b.len())
    }

    /// Read a page from the Timeline we are writing to.  For metadata pages, this passes through
    /// a cache in Self, which makes writes earlier in this modification visible to WAL records later
    /// in the modification.
    ///
    /// For data pages, reads pass directly to the owning Timeline: any ingest code which reads a data
    /// page must ensure that the pages they read are already committed in Timeline, for example
    /// DB create operations are always preceded by a call to commit().  This is special cased because
    /// it's rare: all the 'normal' WAL operations will only read metadata pages such as relation sizes,
    /// and not data pages.
    ///
    /// The committed read is strictly below this record, so a replayed record sees the state its
    /// first pass saw rather than its own or later prior-pass rows (#22, #1627).
    async fn get(&self, key: Key, ctx: &RequestContext) -> Result<Bytes, PageReconstructError> {
        if !Self::is_data_key(&key) {
            // Have we already updated the same key? Read the latest pending updated
            // version in that case.
            if let Some(values) = self.pending_metadata_pages.get(&key.to_compact()) {
                if let Some((_, _, value)) = values.last() {
                    return if let Value::Image(img) = value {
                        Ok(img.clone())
                    } else {
                        // Currently, we never need to read back a WAL record that we
                        // inserted in the same "transaction". All the metadata updates
                        // work directly with Images, and we never need to read actual
                        // data pages. We could handle this if we had to, by calling
                        // the walredo manager, but let's keep it simple for now.
                        Err(PageReconstructError::Other(anyhow::anyhow!(
                            "unexpected pending WAL record"
                        )))
                    };
                }
            }
        } else {
            // This is an expensive check, so we only do it in debug mode. If reading a data key,
            // this key should never be present in pending_data_pages. We ensure this by committing
            // modifications before ingesting DB create operations, which are the only kind that reads
            // data pages during ingest.
            if cfg!(debug_assertions) {
                assert!(
                    !self
                        .pending_data_batch
                        .as_ref()
                        .is_some_and(|b| b.updates_key(&key))
                );
            }
        }

        // Read the state just before this record, as its first run saw it, so a replay of the
        // record makes the same decisions.
        self.tline.get(key, self.lsn_before_record(), ctx).await
    }

    fn lsn_before_record(&self) -> Lsn {
        Lsn(self.lsn.0.saturating_sub(1))
    }

    /// Get a key from the sparse keyspace. Automatically converts the missing key error
    /// and the empty value into None.
    async fn sparse_get(
        &self,
        key: Key,
        ctx: &RequestContext,
    ) -> Result<Option<Bytes>, PageReconstructError> {
        let val = self.get(key, ctx).await;
        match val {
            Ok(val) if val.is_empty() => Ok(None),
            Ok(val) => Ok(Some(val)),
            Err(PageReconstructError::MissingKey(_)) => Ok(None),
            Err(e) => Err(e),
        }
    }

    #[cfg(test)]
    pub fn put_for_unit_test(&mut self, key: Key, val: Value) {
        self.put(key, val);
    }

    fn put(&mut self, key: Key, val: Value) {
        if Self::is_data_key(&key) {
            self.put_data(key.to_compact(), val)
        } else {
            self.put_metadata(key.to_compact(), val)
        }
    }

    fn put_data(&mut self, key: CompactKey, val: Value) {
        let batch = self
            .pending_data_batch
            .get_or_insert_with(SerializedValueBatch::default);
        batch.put(key, val, self.lsn);
    }

    fn put_metadata(&mut self, key: CompactKey, val: Value) {
        let values = self.pending_metadata_pages.entry(key).or_default();
        // Replace the previous value if it exists at the same lsn
        if let Some((last_lsn, last_value_ser_size, last_value)) = values.last_mut() {
            if *last_lsn == self.lsn {
                // Update the pending_metadata_bytes contribution from this entry, and update the serialized size in place
                self.pending_metadata_bytes -= *last_value_ser_size;
                *last_value_ser_size = val.serialized_size().unwrap() as usize;
                self.pending_metadata_bytes += *last_value_ser_size;

                // Use the latest value, this replaces any earlier write to the same (key,lsn), such as much
                // have been generated by synthesized zero page writes prior to the first real write to a page.
                *last_value = val;
                return;
            }
        }

        let val_serialized_size = val.serialized_size().unwrap() as usize;
        self.pending_metadata_bytes += val_serialized_size;
        values.push((self.lsn, val_serialized_size, val));

        if key == CHECKPOINT_KEY.to_compact() {
            tracing::debug!("Checkpoint key added to pending with size {val_serialized_size}");
        }
    }
}

/// Statistics for a DatadirModification.
#[derive(Default)]
pub struct DatadirModificationStats {
    pub metadata_images: u64,
    pub metadata_deltas: u64,
    pub data_images: u64,
    pub data_deltas: u64,
}

/// What `put_rel_extend` should do for a record that may extend a
/// relation: `old_size` is the LSN-versioned size-key value at the
/// record's LSN, `nblocks` the size implied by the record's block
/// number.
pub(crate) struct RelExtendPlan {
    /// `Some(new_size)` when the size key must be rewritten (genuine
    /// growth); `None` when storage already knows a size >= nblocks.
    pub(crate) write_size: Option<BlockNumber>,
    /// Value the rel-size cache must be advanced to — ALWAYS applied,
    /// even when `write_size` is `None`.
    pub(crate) cache_size: BlockNumber,
}

/// During WAL replay below YB's durable horizon, the first pass's own
/// size-key writes are still visible, so `old_size >= nblocks` holds
/// for every record and the write branch never fires. The cache must
/// advance regardless: a cache pinned at the replay-start size makes
/// every subsequent record look like an extension, and
/// `handle_rel_extend` then re-zero-fills `[pinned_size, blkno)` on
/// every record — shadowing live blocks with zero page images
/// (#22).
pub(crate) fn rel_extend_plan(old_size: BlockNumber, nblocks: BlockNumber) -> RelExtendPlan {
    RelExtendPlan {
        write_size: (nblocks > old_size).then_some(nblocks),
        cache_size: nblocks.max(old_size),
    }
}

/// This struct facilitates accessing either a committed key from the timeline at a
/// specific LSN, or the latest uncommitted key from a pending modification.
///
/// During WAL ingestion, the records from multiple LSNs may be batched in the same
/// modification before being flushed to the timeline. Hence, the routines in WalIngest
/// need to look up the keys in the modification first before looking them up in the
/// timeline to not miss the latest updates.
#[derive(Clone, Copy)]
pub enum Version<'a> {
    LsnRange(LsnRange),
    Modified(&'a DatadirModification<'a>),
}

impl Version<'_> {
    async fn get(
        &self,
        timeline: &Timeline,
        key: Key,
        ctx: &RequestContext,
    ) -> Result<Bytes, PageReconstructError> {
        match self {
            Version::LsnRange(lsns) => timeline.get(key, lsns.effective_lsn, ctx).await,
            Version::Modified(modification) => modification.get(key, ctx).await,
        }
    }

    /// Get a key from the sparse keyspace. Automatically converts the missing key error
    /// and the empty value into None.
    async fn sparse_get(
        &self,
        timeline: &Timeline,
        key: Key,
        ctx: &RequestContext,
    ) -> Result<Option<Bytes>, PageReconstructError> {
        let val = self.get(timeline, key, ctx).await;
        match val {
            Ok(val) if val.is_empty() => Ok(None),
            Ok(val) => Ok(Some(val)),
            Err(PageReconstructError::MissingKey(_)) => Ok(None),
            Err(e) => Err(e),
        }
    }

    pub fn is_latest(&self) -> bool {
        match self {
            Version::LsnRange(lsns) => lsns.is_latest(),
            Version::Modified(_) => true,
        }
    }

    pub fn get_lsn(&self) -> Lsn {
        match self {
            Version::LsnRange(lsns) => lsns.effective_lsn,
            Version::Modified(modification) => modification.lsn,
        }
    }

    pub fn at(lsn: Lsn) -> Self {
        Version::LsnRange(LsnRange {
            effective_lsn: lsn,
            request_lsn: lsn,
        })
    }
}

//--- Metadata structs stored in key-value pairs in the repository.

#[derive(Debug, Serialize, Deserialize)]
pub(crate) struct DbDirectory {
    // (spcnode, dbnode) -> (do relmapper and PG_VERSION files exist)
    pub(crate) dbdirs: HashMap<(Oid, Oid), bool>,
}

// The format of TwoPhaseDirectory changed in PostgreSQL v17, because the filenames of
// pg_twophase files was expanded from 32-bit XIDs to 64-bit XIDs.  Previously, the files
// were named like "pg_twophase/000002E5", now they're like
// "pg_twophsae/0000000A000002E4".

#[derive(Debug, Serialize, Deserialize)]
pub(crate) struct TwoPhaseDirectory {
    pub(crate) xids: HashSet<TransactionId>,
}

#[derive(Debug, Serialize, Deserialize)]
struct TwoPhaseDirectoryV17 {
    xids: HashSet<u64>,
}

#[derive(Debug, Serialize, Deserialize)]
struct RelSizeEntry {
    nblocks: u32,
}

#[derive(Debug, Serialize, Deserialize, Default)]
pub(crate) struct SlruSegmentDirectory {
    // Set of SLRU segments that exist.
    pub(crate) segments: HashSet<u32>,
}

#[derive(Copy, Clone, PartialEq, Eq, Debug, enum_map::Enum)]
#[repr(u8)]
pub(crate) enum DirectoryKind {
    Db,
    TwoPhase,
    AuxFiles,
    SlruSegment(SlruKind),
    RelV2,
}

impl DirectoryKind {
    pub(crate) const KINDS_NUM: usize = <DirectoryKind as Enum>::LENGTH;
    pub(crate) fn offset(&self) -> usize {
        self.into_usize()
    }
}

static ZERO_PAGE: Bytes = Bytes::from_static(&[0u8; BLCKSZ as usize]);

#[cfg(test)]
mod tests {
    use super::{latest_rel_size_cache_hit, rel_extend_plan};
    use utils::lsn::Lsn;

    /// Replay below the durable horizon: storage already knows a size
    /// >= nblocks. No size-key rewrite, but the cache MUST advance to
    /// > the storage value — a cache left untouched here is the #22
    /// > pin that re-zero-fills live blocks on every record.
    #[test]
    fn rel_extend_replay_advances_cache_without_write() {
        let plan = rel_extend_plan(100, 61);
        assert_eq!(plan.write_size, None);
        assert_eq!(plan.cache_size, 100);
    }

    /// Normal forward growth: write the new size and cache it.
    #[test]
    fn rel_extend_growth_writes_and_caches() {
        let plan = rel_extend_plan(60, 61);
        assert_eq!(plan.write_size, Some(61));
        assert_eq!(plan.cache_size, 61);
    }

    /// Same size: no write, cache confirmed at the current value.
    #[test]
    fn rel_extend_same_size_is_noop_write() {
        let plan = rel_extend_plan(61, 61);
        assert_eq!(plan.write_size, None);
        assert_eq!(plan.cache_size, 61);
    }

    /// #240: entries seeded by attach-replay ingest (stamped inside the
    /// replay window, i.e. below the horizon) must never serve — a
    /// basebackup at the horizon read a mid-replay size and shipped a
    /// truncated relation. Entries at/above the horizon behave as before.
    #[test]
    fn rel_size_cache_rejects_replay_window_entries() {
        let horizon = Lsn(0x6BA4278);
        let mid_replay = Lsn(0x6AED7C0);
        let reader = false;
        let ingest = true;
        // The #240 shape: replay-seeded entry, external read at the horizon.
        assert!(!latest_rel_size_cache_hit(
            horizon, mid_replay, horizon, reader
        ));
        // Same entry can't serve external mid-replay reads either.
        assert!(!latest_rel_size_cache_hit(
            mid_replay, mid_replay, horizon, reader
        ));
        // Ingest replaying at its own frontier keeps the fast path (#22
        // replay semantics track the cache).
        assert!(latest_rel_size_cache_hit(
            mid_replay, mid_replay, horizon, ingest
        ));
        // Entry stamped exactly at the horizon serves reads at/after it.
        assert!(latest_rel_size_cache_hit(horizon, horizon, horizon, reader));
        assert!(latest_rel_size_cache_hit(
            Lsn(0x6BA5000),
            horizon,
            horizon,
            reader
        ));
        // Post-catch-up ingest entry: normal fast-path behavior.
        assert!(latest_rel_size_cache_hit(
            Lsn(0x7000000),
            Lsn(0x6BB0000),
            horizon,
            reader
        ));
        // Request below the entry is always a miss (pre-existing rule),
        // for ingest too.
        assert!(!latest_rel_size_cache_hit(
            Lsn(0x6BB0000),
            Lsn(0x7000000),
            horizon,
            reader
        ));
        assert!(!latest_rel_size_cache_hit(
            Lsn(0x6BB0000),
            Lsn(0x7000000),
            horizon,
            ingest
        ));
        // Bootstrap timeline (horizon 0): unchanged semantics.
        assert!(latest_rel_size_cache_hit(Lsn(100), Lsn(50), Lsn(0), reader));
    }
}

#[cfg(test)]
mod clog_trailer_tests {
    use super::{ClogTrailer, LsnForTimestamp, clog_trailer_from_rows, lsn_for_timestamp_result};
    use crate::page_store::SlruPageRow;
    use bytes::Bytes;
    use pageserver_api::reltag::SlruKind;
    use postgres_ffi::BLCKSZ;
    use utils::bin_ser::BeSer;
    use utils::lsn::Lsn;
    use wal_decoder::models::record::NeonWalRecord;

    fn row(lsn: u64, is_image: bool, payload: Vec<u8>) -> SlruPageRow {
        SlruPageRow {
            kind: SlruKind::Clog,
            segno: 0,
            blknum: 0,
            lsn: Lsn(lsn),
            is_image,
            will_init: is_image,
            payload: Bytes::from(payload),
        }
    }
    fn commit(lsn: u64, ts: i64) -> SlruPageRow {
        let rec = NeonWalRecord::ClogSetCommitted {
            xids: vec![1],
            timestamp: ts,
        };
        row(lsn, false, rec.ser().unwrap())
    }
    fn abort(lsn: u64) -> SlruPageRow {
        row(
            lsn,
            false,
            NeonWalRecord::ClogSetAborted { xids: vec![1] }
                .ser()
                .unwrap(),
        )
    }
    fn image(lsn: u64, trailer: Option<i64>) -> SlruPageRow {
        let mut page = vec![0u8; BLCKSZ as usize];
        if let Some(ts) = trailer {
            page.extend_from_slice(&ts.to_be_bytes());
        }
        row(lsn, true, page)
    }

    #[test]
    fn newest_commit_row_is_the_trailer() {
        let rows = [commit(300, 30), commit(200, 20), commit(100, 10)];
        assert_eq!(
            clog_trailer_from_rows(&rows).unwrap(),
            ClogTrailer::Known(Some(30))
        );
    }

    #[test]
    fn aborts_on_top_are_skipped() {
        let rows = [abort(500), abort(400), commit(300, 30), commit(200, 20)];
        assert_eq!(
            clog_trailer_from_rows(&rows).unwrap(),
            ClogTrailer::Known(Some(30))
        );
    }

    #[test]
    fn image_settles_with_or_without_trailer() {
        let rows = [abort(400), image(300, Some(25)), commit(200, 20)];
        assert_eq!(
            clog_trailer_from_rows(&rows).unwrap(),
            ClogTrailer::Known(Some(25))
        );
        let rows = [image(300, None), commit(200, 20)];
        assert_eq!(
            clog_trailer_from_rows(&rows).unwrap(),
            ClogTrailer::Known(None)
        );
    }

    #[test]
    fn aborts_only_or_nothing_is_unknown() {
        let rows = [abort(500), abort(400)];
        assert_eq!(clog_trailer_from_rows(&rows).unwrap(), ClogTrailer::Unknown);
        assert_eq!(clog_trailer_from_rows(&[]).unwrap(), ClogTrailer::Unknown);
    }

    /// Segment order changes `found_smaller`: under the newest-first walk a
    /// too-late probe breaks on the first trailer >= T and may set nothing,
    /// while hash order could have seen a smaller trailer before breaking.
    /// When every probe is too-late `low` never moves, so
    /// `commit_lsn = min_lsn - 8`, and both flag outcomes must agree on
    /// `Past(min_lsn)`.
    #[test]
    fn all_probes_too_late_is_past_under_both_orderings() {
        let min_lsn = Lsn(0x1000);
        let commit_lsn = Lsn(0x1000 - 8);
        // newest-first: no smaller trailer observed before any break
        assert!(matches!(
            lsn_for_timestamp_result(false, true, commit_lsn, min_lsn),
            LsnForTimestamp::Past(l) if l == min_lsn
        ));
        // hash order: a smaller trailer observed on the way to the break
        assert!(matches!(
            lsn_for_timestamp_result(true, true, commit_lsn, min_lsn),
            LsnForTimestamp::Past(l) if l == min_lsn
        ));
    }

    #[test]
    fn some_probe_too_early_classifies_by_flags() {
        let min_lsn = Lsn(0x1000);
        let commit_lsn = Lsn(0x2000);
        assert!(matches!(
            lsn_for_timestamp_result(true, true, commit_lsn, min_lsn),
            LsnForTimestamp::Present(l) if l == commit_lsn
        ));
        assert!(matches!(
            lsn_for_timestamp_result(true, false, commit_lsn, min_lsn),
            LsnForTimestamp::Future(l) if l == commit_lsn
        ));
        assert!(matches!(
            lsn_for_timestamp_result(false, false, commit_lsn, min_lsn),
            LsnForTimestamp::NoData(l) if l == min_lsn
        ));
    }
}
