//! Per-timeline metadata.
//!
//! Synthesized from the `amp_storage.branch_state` row (see
//! `synth_timeline_metadata_from_row`); never serialized, so it carries no version.

use postgres_ffi::PgMajorVersion;
use utils::id::BranchId;
use utils::lsn::Lsn;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TimelineMetadata {
    consistent_lsn: Lsn,
    // This is only set if we know it. We track it in memory when the page
    // server is running, but we only track the value corresponding to
    // 'last_record_lsn', not 'consistent_lsn' which can lag behind by a
    // lot. We only store it in the metadata file when we flush *all* the
    // in-memory data so that 'last_record_lsn' is the same as
    // 'consistent_lsn'.  That's OK, because after page server restart, as
    // soon as we reprocess at least one record, we will have a valid
    // 'prev_record_lsn' value in memory again. This is only really needed when
    // doing a clean shutdown, so that there is no more WAL beyond
    // 'consistent_lsn'
    prev_record_lsn: Option<Lsn>,
    ancestor_timeline: Option<BranchId>,
    ancestor_lsn: Lsn,

    // The LSN at which GC was last executed.  Synonym of [`Timeline::applied_gc_cutoff_lsn`].
    latest_gc_cutoff_lsn: Lsn,

    initdb_lsn: Lsn,
    pg_version: PgMajorVersion,
}

impl TimelineMetadata {
    pub fn new(
        consistent_lsn: Lsn,
        prev_record_lsn: Option<Lsn>,
        ancestor_timeline: Option<BranchId>,
        ancestor_lsn: Lsn,
        latest_gc_cutoff_lsn: Lsn,
        initdb_lsn: Lsn,
        pg_version: PgMajorVersion,
    ) -> Self {
        Self {
            consistent_lsn,
            prev_record_lsn,
            ancestor_timeline,
            ancestor_lsn,
            latest_gc_cutoff_lsn,
            initdb_lsn,
            pg_version,
        }
    }

    /// Durable watermark: every record at or below this is in `amp_storage.pages`
    /// with `branch_state.consistent_lsn` updated.
    pub fn consistent_lsn(&self) -> Lsn {
        self.consistent_lsn
    }

    pub fn prev_record_lsn(&self) -> Option<Lsn> {
        self.prev_record_lsn
    }

    pub fn ancestor_timeline(&self) -> Option<BranchId> {
        self.ancestor_timeline
    }

    pub fn ancestor_lsn(&self) -> Lsn {
        self.ancestor_lsn
    }

    /// When reparenting, the `ancestor_lsn` does not change.
    ///
    /// Returns true if anything was changed.
    pub fn reparent(&mut self, timeline: &BranchId) {
        assert!(self.ancestor_timeline.is_some());
        // no assertion for redoing this: it's fine, we may have to repeat this multiple times over
        self.ancestor_timeline = Some(*timeline);
    }

    /// Returns true if anything was changed
    pub fn detach_from_ancestor(&mut self, branchpoint: &(BranchId, Lsn)) {
        // Detaching from ancestor now doesn't always detach directly to the direct ancestor, but we
        // ensure the LSN is the same. So we don't check the timeline ID.
        if self.ancestor_lsn != Lsn(0) {
            assert_eq!(self.ancestor_lsn, branchpoint.1);
        }
        self.ancestor_timeline = None;
        self.ancestor_lsn = Lsn(0);
    }

    pub fn latest_gc_cutoff_lsn(&self) -> Lsn {
        self.latest_gc_cutoff_lsn
    }

    pub fn initdb_lsn(&self) -> Lsn {
        self.initdb_lsn
    }

    pub fn pg_version(&self) -> PgMajorVersion {
        self.pg_version
    }
}
