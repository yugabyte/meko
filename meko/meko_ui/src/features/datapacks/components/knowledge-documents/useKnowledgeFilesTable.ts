// Shared data/polling logic behind the Files table -- fetch + filter +
// pagination + the "queued placeholder" bookkeeping that makes a
// just-uploaded file appear instantly even before dist_rag has a real
// row for it. Extracted out of KnowledgeFilesTab so any surface that
// embeds `FileUpload` next to a `FilesTable` (the Files tab, and the
// upload dialog) can wire the two together identically instead of
// re-deriving this by hand.
//
// Backed by TanStack Query rather than hand-rolled fetch/loading state:
// - Every (datapackId, page, filters) combination is its own cache entry,
//   so paging back to an already-seen page (or returning to this tab
//   after navigating away) is instant -- no spinner, no network round
//   trip -- as long as it's within `staleTime`.
// - `placeholderData: keepPreviousData` keeps the outgoing page's rows on
//   screen while the next page loads instead of blanking the table.
// - `isFetching` (true for any in-flight request, including background
//   polls and page changes) drives the RowsList overlay; `isLoading`
//   (true only when there's no cached data at all yet) drives the full
//   "Loading documents…" empty-state -- this is the same silent-vs-real
//   distinction the previous hand-rolled version tracked manually.
import { useCallback, useEffect, useRef, useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import { sanitizeFilenameForMatch } from "@/lib/knowledgeFileUploadRules";
import { getApiErrorMessage, getKnowledgeBaseDocuments } from "@/lib/api/mekoApi";
import { documentFileExtension, documentNameLastSegment } from "./knowledgeDocumentName";
import { type FileTypeFilter, type StatusFilter } from "./KnowledgeFilesTable";
import { kbFilesKeys } from "@/features/datapacks/lib/knowledgeQueryKeys";

export { kbFilesKeys };

type UseKnowledgeFilesTableArgs = {
  datapackId: string;
  /** No longer read directly (react-query keys off datapackId alone),
   *  kept so existing callers don't need to change their prop list. */
  datapackName?: string;
  /** Document-name substring filter, e.g. from a page-level search bar.
   *  Defaults to no filter for callers (like the upload dialog) that
   *  don't have their own search input. */
  searchQuery?: string;
  pageSize?: number;
  /** Bumped by the caller (e.g. after a delete or KB-create elsewhere)
   *  to force a re-fetch without unmounting anything. */
  refreshKey?: number;
  /** Deep-link arrival target (e.g. from the upload dialog's row click).
   *  If it isn't on the currently-loaded page -- wrong page, or hidden
   *  by a stale type/status filter -- this hook locates it and jumps
   *  there automatically instead of silently showing nothing. */
  highlightDocumentId?: string;
  /** Fired after the highlight row has genuinely been found and shimmered. */
  onHighlightConsumed?: (id: string) => void;
};

export function useKnowledgeFilesTable({
  datapackId,
  searchQuery = "",
  pageSize = 5,
  refreshKey,
  highlightDocumentId,
  onHighlightConsumed,
}: UseKnowledgeFilesTableArgs) {
  const qc = useQueryClient();
  const [typeFilter, setTypeFilter] = useState<FileTypeFilter>("all");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [page, setPage] = useState(1);
  const q = searchQuery.trim();

  // Optimistic "queued" rows for files just uploaded but not yet visible
  // in `documents` -- mirrors FileUpload's own queuedDocNames, which is
  // needed because dist_rag.documents has no row for a brand new source's
  // first files until its CREATE_SOURCE worker discovers them (see the
  // isNewSource comment in CreateKB). Reported by FileUpload so the table
  // shows *something* immediately instead of the file silently vanishing
  // between "uploaded" and "discovered".
  //
  // Union, never replace: FileUpload wholesale-clears its own
  // queuedDocNames the moment *any* pipelineDetails entry exists (even for
  // an unrelated file already in this KB), which would otherwise drop a
  // still-invisible name here too early. A name only leaves this set once
  // it actually shows up in `documents` below (see knownKeys) -- this
  // hook owns that decision, not FileUpload.
  // Keyed by *sanitized* name -- FileUpload can report either the raw
  // `File.name` (for a name dist_rag hasn't discovered yet) or the
  // already-sanitized backend document_name (once dist_rag has picked it
  // up), and `documents` below always has the sanitized S3-key basename.
  // Keying on whatever FileUpload happened to send would let a filename
  // with a space/parenthesis/etc. (sanitized to "_" server-side) never
  // match its own real row, leaving a permanent duplicate placeholder.
  // The raw name is kept as `displayName` purely for rendering.
  const [queuedPlaceholders, setQueuedPlaceholders] = useState<
    Map<string, { displayName: string; status: string }>
  >(new Map());
  const handleQueuedDocsChange = useCallback((docs: { name: string; status: string }[]) => {
    // If FileUpload sends an empty list, it means it has no more optimistic
    // or discovered-but-unindexed files to report. We don't clear the map
    // here, though, because of the "gap" -- dist_rag might have finished
    // a doc (so FileUpload drops it) but ListDistRagDocuments hasn't seen
    // it yet. We let the `useEffect([documents])` below be the final
    // authority on pruning.
    if (docs.length === 0) return;

    setQueuedPlaceholders((prev) => {
      let changed = false;
      const next = new Map(prev);

      for (const { name, status } of docs) {
        const key = sanitizeFilenameForMatch(name);
        const existing = next.get(key);
        // Keep whichever display name we saw first (usually the nicer,
        // unsanitized raw filename) -- once dist_rag discovers the file,
        // FileUpload starts reporting the sanitized backend document_name
        // for the same key, and swapping to that would replace "My File
        // (1).pdf" with "My_File__1_.pdf" mid-flight for no benefit.
        if (!existing) {
          next.set(key, { displayName: name, status });
          changed = true;
        } else if (existing.status !== status) {
          next.set(key, { displayName: existing.displayName, status });
          changed = true;
        }
      }

      return changed ? next : prev;
    });
  }, []);
  // Mirrors FileUpload's own pollingActive -- see onProcessingActiveChange's
  // doc comment. Belt-and-suspenders alongside queuedPlaceholders: this
  // fires reliably for the whole in-flight window (upload -> indexed),
  // whereas the placeholder set can get pruned of a file's queued name
  // before dist_rag actually finishes it, which would otherwise stop the
  // poll below too early and leave this table stale until a manual reload.
  const [uploadProcessingActive, setUploadProcessingActive] = useState(false);

  // When the caller's searchQuery changes, reset to page 1. Updating
  // state during render (the React 18 "getDerivedStateFromProps" pattern)
  // is safe here: React discards the current render and immediately
  // re-renders with page=1, so the query below always sees the correct
  // page alongside the new query.
  const prevSearchRef = useRef(searchQuery);
  if (prevSearchRef.current !== searchQuery) {
    prevSearchRef.current = searchQuery;
    if (page !== 1) setPage(1);
  }

  const hasPendingPlaceholders = queuedPlaceholders.size > 0;
  const shouldPollDocuments = hasPendingPlaceholders || uploadProcessingActive;

  const documentsQuery = useQuery({
    queryKey: kbFilesKeys.documents(datapackId, page, pageSize, q, typeFilter, statusFilter),
    queryFn: ({ signal }) =>
      getKnowledgeBaseDocuments(datapackId, {
        page,
        page_size: pageSize,
        q: q || undefined,
        type_filter: typeFilter !== "all" ? typeFilter : undefined,
        status_filter: statusFilter !== "all" ? statusFilter : undefined,
        signal,
      }),
    // A stale page is still shown instantly on revisit (e.g. Prev after
    // Next, or navigating back to this tab) while a background refetch
    // quietly confirms it's still current -- smoother than either a
    // spinner-every-time or a cache that never double-checks itself.
    staleTime: 10_000,
    placeholderData: keepPreviousData,
    // Only while something's genuinely in flight (a fresh upload still
    // discovering/indexing) -- otherwise an already-settled KB sits idle,
    // exactly like the previous hand-rolled setInterval version.
    refetchInterval: shouldPollDocuments ? 3000 : false,
  });
  const documents = documentsQuery.data?.items ?? [];
  const total = documentsQuery.data?.total ?? 0;
  const hasMore = documentsQuery.data?.has_more ?? false;
  // isLoading (react-query: isPending && isFetching) only true when there's
  // no cached data at all -- a background poll or page change instead
  // reports via isFetching below, without wiping the rows already shown.
  const loading = documentsQuery.isLoading || documentsQuery.isPlaceholderData;
  const error = documentsQuery.isError
    ? getApiErrorMessage(documentsQuery.error, "Could not load knowledge documents.")
    : null;

  // Deep-link page locator: if `highlightDocumentId` isn't on the
  // currently-loaded page, find which page it's actually on (ignoring
  // type/status filters, which may be the very reason it's hidden) and
  // jump there -- rather than silently rendering a page with no
  // highlighted row and leaving the caller's scroll-into-view a no-op.
  // Only runs once the *current* page has genuinely loaded and confirmed
  // the doc isn't on it, so the common case (already on the right page)
  // never pays for the extra lookup.
  const highlightFoundOnPage = documents.some((d) => d.id === highlightDocumentId);

  // "Flash once, ever, per arrival" -- once the target row has actually
  // been found, shown, AND finished flashing, permanently stop treating
  // `highlightDocumentId` as live. Without this it stays armed for as
  // long as the caller keeps passing the same id (which is the whole
  // page visit): the user pages away, `highlightFoundOnPage` goes false
  // again, `needsLocate` below re-fires, and the locate query yanks them
  // straight back to that row's page -- which also remounts the row and
  // replays its flash, so it reads as a permanent "snap back and reflash"
  // loop instead of a single arrival flash.
  //
  // Crucially this is driven by the *row*, via `consumeHighlight`, not by
  // `highlightFoundOnPage` going true. Retiring the id the instant the
  // row appears would clear `highlight` on that row mid-animation and
  // the flash would never become visible.
  const [highlightConsumed, setHighlightConsumed] = useState(false);
  const activeHighlightDocumentId = highlightConsumed ? undefined : highlightDocumentId;

  const consumeHighlight = useCallback(
    (id: string) => {
      console.log(
        "[arrival-flash] useKnowledgeFilesTable consume",
        JSON.stringify({ id, highlightDocumentId, matches: id === highlightDocumentId }, null, 2),
      );
      if (id !== highlightDocumentId) return;
      setHighlightConsumed(true);
      onHighlightConsumed?.(id);
    },
    [highlightDocumentId, onHighlightConsumed],
  );

  const needsLocate = Boolean(activeHighlightDocumentId) && !loading && !highlightFoundOnPage;

  useEffect(() => {
    if (!highlightDocumentId) return;
    console.log(
      "[arrival-flash] useKnowledgeFilesTable state",
      JSON.stringify(
        {
          highlightDocumentId,
          activeHighlightDocumentId,
          highlightFoundOnPage,
          highlightConsumed,
          needsLocate,
          loading,
          page,
          docIdsOnPage: documents.map((d) => d.id),
        },
        null,
        2,
      ),
    );
  }, [
    highlightDocumentId,
    activeHighlightDocumentId,
    highlightFoundOnPage,
    highlightConsumed,
    needsLocate,
    loading,
    page,
    documents,
  ]);

  const locateQuery = useQuery({
    queryKey: ["kb-documents-locate", datapackId, highlightDocumentId],
    queryFn: async ({ signal }) => {
      // Unfiltered, in the same server sort order as the real table, so
      // an absolute index here maps directly to a page under `pageSize`
      // regardless of what type/status filter is currently active.
      const LOCATE_PAGE_SIZE = 200;
      const MAX_PAGES_TO_SCAN = 10; // best-effort cap (2000 docs)
      for (let p = 1; p <= MAX_PAGES_TO_SCAN; p++) {
        const resp = await getKnowledgeBaseDocuments(datapackId, {
          page: p,
          page_size: LOCATE_PAGE_SIZE,
          signal,
        });
        const idx = resp.items.findIndex((d) => d.id === highlightDocumentId);
        if (idx >= 0) return (p - 1) * LOCATE_PAGE_SIZE + idx;
        if (!resp.has_more) break;
      }
      return null;
    },
    enabled: needsLocate,
    staleTime: Infinity,
  });
  useEffect(() => {
    if (locateQuery.data == null) return;
    const targetPage = Math.floor(locateQuery.data / pageSize) + 1;
    // Whatever filter was hiding it (if any) needs clearing too, or the
    // page we just jumped to would still come back empty.
    setTypeFilter("all");
    setStatusFilter("all");
    setPage(targetPage);
  }, [locateQuery.data, pageSize]);

  // Which file-type / status chips are worth showing at all -- e.g. no
  // point offering a ".json" chip in a datapack that has never had one.
  // Sampled from up to 200 documents (the API's page cap) ignoring the
  // current type/status filter (but honoring search) so switching a
  // filter doesn't itself shrink the chip list out from under the user.
  // A best-effort sample, not an exact distinct-values query -- fine for
  // deciding which chips are worth a click, over-provisioning past 200
  // uploaded files just means an extra chip or two might show.
  const availableFiltersQuery = useQuery({
    queryKey: kbFilesKeys.availableFilters(datapackId, q),
    queryFn: ({ signal }) =>
      getKnowledgeBaseDocuments(datapackId, { page: 1, page_size: 200, q: q || undefined, signal }),
    staleTime: 30_000,
    // Sampling failure just means chips fall back to "show everything"
    // (see FilterRow) -- not worth surfacing an error for, so no `error`
    // is read out of this query at all.
  });
  // Unioned with whatever is on the current page, because the sample is a
  // separate cache entry with its own staleTime and no refetchInterval: while
  // `documentsQuery` polls an in-flight upload, the sample stays frozen on the
  // pre-upload picture, so a file that just went PROCESSING renders that badge
  // in its row while the dropdown still offers only "Indexed" until a reload.
  // Rows in `documents` are real server rows, so a status/type taken from one
  // is guaranteed to match at least that row when picked.
  const sampledAndVisible = [...(availableFiltersQuery.data?.items ?? []), ...documents];
  const availableTypeFilters = Array.from(
    new Set(
      // `document_type` on a document row is the S3 object's MIME
      // content-type (e.g. "text/csv"), not an extension -- the type
      // filter itself matches a file-extension suffix on the name
      // server-side, so the available-chips sample has to key off the
      // same thing or every chip would filter out every document (see
      // documentFileExtension's doc comment).
      sampledAndVisible.map((d) => documentFileExtension(d.document_name)).filter(Boolean),
    ),
  );
  const availableStatusFilters = Array.from(new Set(sampledAndVisible.map((d) => d.status)));

  // Bridge the caller's external `refreshKey` (bumped after a delete or
  // KB-create elsewhere) into a cache invalidation -- react-query then
  // refetches every cached page/filter combo for this datapack, keeping
  // whatever's currently on screen visible (isFetching, not isLoading)
  // rather than a full reload. Guarded so mount doesn't invalidate a
  // cache it just populated.
  //
  // Also wipes queuedPlaceholders outright rather than waiting for the
  // documents-confirmed pruning effect below: that effect only ever
  // *removes* a name once it reappears in `documents`, so a name whose
  // only reason for being queued was a file that's now been deleted
  // would otherwise sit there showing "Queued" forever (until a full
  // page reload resets this hook's state). refreshKey only bumps on an
  // explicit user action (delete, KB-create) rather than every poll, so
  // this can't thrash a placeholder that's still genuinely in flight --
  // FileUpload's own poll (pollTick, derived from this same refetch)
  // re-reports it via handleQueuedDocsChange within the next cycle.
  const prevRefreshKeyRef = useRef(refreshKey);
  useEffect(() => {
    if (prevRefreshKeyRef.current === refreshKey) return;
    prevRefreshKeyRef.current = refreshKey;
    setQueuedPlaceholders((prev) => (prev.size === 0 ? prev : new Map()));
    void qc.invalidateQueries({ queryKey: kbFilesKeys.documentsAll(datapackId) });
    void qc.invalidateQueries({ queryKey: kbFilesKeys.availableFiltersAll(datapackId) });
  }, [refreshKey, datapackId, qc]);

  // Force a re-fetch (e.g. this table's own delete action, for a caller
  // with no external `refreshKey` to delete, like the upload dialog).
  // Also clears queuedPlaceholders -- see the refreshKey effect above,
  // which this mirrors for callers with no refreshKey to bump.
  const refetch = useCallback(() => {
    setQueuedPlaceholders((prev) => (prev.size === 0 ? prev : new Map()));
    void qc.invalidateQueries({ queryKey: kbFilesKeys.documentsAll(datapackId) });
    void qc.invalidateQueries({ queryKey: kbFilesKeys.availableFiltersAll(datapackId) });
  }, [qc, datapackId]);

  // Shared pulse for FileUpload's own status poll -- any change re-arms
  // its fetch effect (see FileUpload's pollTick doc comment), so this
  // table's own query timestamp doubles as that signal directly: no
  // separate counter needed, and it now also re-checks status on a plain
  // page/filter change, which is harmless (just an extra status read).
  const pollTick = documentsQuery.dataUpdatedAt;

  // Broadcast to sibling views of this datapack's documents -- e.g. the
  // main datapack page's preview tile, which caches a different (smaller,
  // unfiltered) page-size than this table -- so a file finishing upload
  // here (via the poll above) also shows up there without a manual
  // reload. Only fires while genuinely polling, and only on each new
  // successful fetch (not every render). Excludes this exact query's own
  // key: it's already kept fresh by its own refetchInterval above, and
  // invalidating it here too would immediately refetch it, bump
  // `dataUpdatedAt` again, and re-trigger this same effect forever.
  const lastBroadcastAtRef = useRef(documentsQuery.dataUpdatedAt);
  useEffect(() => {
    if (!shouldPollDocuments) return;
    if (documentsQuery.dataUpdatedAt === lastBroadcastAtRef.current) return;
    lastBroadcastAtRef.current = documentsQuery.dataUpdatedAt;
    const selfKey = kbFilesKeys.documents(datapackId, page, pageSize, q, typeFilter, statusFilter);
    void qc.invalidateQueries({
      queryKey: kbFilesKeys.documentsAll(datapackId),
      exact: false,
      predicate: (query) => query.queryKey.join("\u0000") !== selfKey.join("\u0000"),
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- page/pageSize/q/typeFilter/statusFilter are only read to build selfKey (which excludes this exact query, not to scope what changed); including them would re-run this effect on every filter change for no benefit beyond what dataUpdatedAt already captures.
  }, [documentsQuery.dataUpdatedAt, shouldPollDocuments, qc, datapackId]);

  // Prune graduated names out of the stored map (not just the rendered
  // list below) once `documents` confirms them -- otherwise the map only
  // ever grows, which would leave hasPendingPlaceholders (and its poll)
  // running forever after the very first queued upload. `document_name`
  // is already the sanitized S3-key basename, matching the sanitized keys
  // the map is stored under (see queuedPlaceholders above).
  useEffect(() => {
    if (queuedPlaceholders.size === 0) return;
    const known = new Set(
      documents.map((d) =>
        sanitizeFilenameForMatch(documentNameLastSegment(d.document_name).replace(/^\d+_/, "")),
      ),
    );
    setQueuedPlaceholders((prev) => {
      let changed = false;
      const next = new Map(prev);
      Array.from(prev.keys()).forEach((key) => {
        if (known.has(key)) {
          next.delete(key);
          changed = true;
        }
      });
      return changed ? next : prev;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `documents` (derived from documentsQuery.data) is a new array each fetch; keying on it directly would re-run every poll for no reason beyond what dataUpdatedAt already captures.
  }, [documentsQuery.dataUpdatedAt]);

  // Reset to page 1 when type or status filter changes. Called directly
  // in the event handler so React 18 batches both state updates into one
  // re-render.
  const handleTypeFilterChange = useCallback((next: FileTypeFilter) => {
    setTypeFilter(next);
    setPage(1);
  }, []);
  const handleStatusFilterChange = useCallback((next: StatusFilter) => {
    setStatusFilter(next);
    setPage(1);
  }, []);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const hasActiveFilter = q !== "" || typeFilter !== "all" || statusFilter !== "all";

  // Drop a placeholder the moment the real row shows up in `documents`
  // (matched the same way FileUpload matches uploads against pipelineDetails
  // -- last path segment, numeric-id prefix stripped, sanitized -- so the
  // two never show the same file twice regardless of whether the
  // placeholder's key came from a raw or already-sanitized name).
  const knownKeys = new Set(
    documents.map((d) =>
      sanitizeFilenameForMatch(documentNameLastSegment(d.document_name).replace(/^\d+_/, "")),
    ),
  );
  const pendingPlaceholders = Array.from(queuedPlaceholders.entries())
    .filter(([key]) => !knownKeys.has(key))
    .map(([, { displayName, status }]) => ({ name: displayName, status }));

  return {
    typeFilter,
    handleTypeFilterChange,
    statusFilter,
    handleStatusFilterChange,
    page,
    setPage,
    totalPages,
    hasMore,
    documents,
    total,
    loading,
    error,
    /** Same value the caller passed in, except once-and-forever cleared
     *  to `undefined` after the target row has actually been found and
     *  shown -- pass this (not the raw `highlightDocumentId` prop) to
     *  `FilesTable`'s `highlightDocumentId` so a later page change can't
     *  remount that row with `highlight` still true and replay the
     *  shimmer. See the comment above `highlightConsumed`. */
    activeHighlightDocumentId,
    pendingPlaceholders,
    hasActiveFilter,
    availableTypeFilters,
    availableStatusFilters,
    pollTick,
    /** Wire directly to `<FileUpload onQueuedDocsChange={...} />`. */
    handleQueuedDocsChange,
    /** Wire directly to `<FileUpload onProcessingActiveChange={...} />`. */
    setUploadProcessingActive,
    /** Signal that the deep-link highlight has been performed and should
     *  no longer trigger page scans or row shimmers. */
    consumeHighlight,
    /** Force a re-fetch (e.g. after this table's own delete action) when
     *  the caller has no external `refreshKey` to bump. */
    refetch,
  };
}
