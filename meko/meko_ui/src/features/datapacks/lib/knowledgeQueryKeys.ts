// Shared react-query key builders for a datapack's Knowledge Base data.
// Centralized (rather than redefined per file) so the several surfaces
// that read/write the same underlying data -- the Files tab, the upload
// dialog, the main datapack page's preview tile, the KB summary header --
// all invalidate the exact same cache entries. Without this, e.g. an
// upload finishing inside the dialog would refresh the dialog's own
// table but leave the main page's preview tile (a different page-size/
// filter combo, so a different specific key) stale until a manual reload.
export const kbFilesKeys = {
  documents: (
    datapackId: string,
    page: number,
    pageSize: number,
    q: string,
    typeFilter: string,
    statusFilter: string,
  ) => ["kb-documents", datapackId, page, pageSize, q, typeFilter, statusFilter] as const,
  // Partial key -- passing just this to invalidateQueries matches every
  // page/filter/page-size combination cached for the datapack.
  documentsAll: (datapackId: string) => ["kb-documents", datapackId] as const,
  availableFilters: (datapackId: string, q: string) =>
    ["kb-available-filters", datapackId, q] as const,
  availableFiltersAll: (datapackId: string) => ["kb-available-filters", datapackId] as const,
};

export const kbSummaryKey = (datapackId: string) => ["kb-summary", datapackId] as const;
export const kbLatestActivityKey = (datapackId: string) =>
  ["kb-activity-log-latest", datapackId] as const;
export const datapackKey = (datapackId: string) => ["datapack", datapackId] as const;
