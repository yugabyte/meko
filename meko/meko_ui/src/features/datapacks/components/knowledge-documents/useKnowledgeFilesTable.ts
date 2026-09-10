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
import { isKbDocumentInFlight } from "./knowledgeDocumentStatus";
import { type FileTypeFilter, type StatusFilter } from "./KnowledgeFilesTable";
import { kbFilesKeys } from "@/features/datapacks/lib/knowledgeQueryKeys";

export { kbFilesKeys };

/** Poll cadence while the pipeline still owns a document. Fast enough that a
 *  status change reads as immediate; react-query holds the timer only while
 *  the window is focused. */
const DOCUMENTS_POLL_MS = 3000;

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
  const uploadInFlight = hasPendingPlaceholders || uploadProcessingActive;

  // Whether any document the table knows about is still owned by the
  // pipeline. Set after the queries below, and read back through a ref by
  // their `refetchInterval` callbacks, which run long after this render.
  const unsettledRef = useRef(false);
  // Used to be armed by this session's own upload alone, so a table watching
  // someone else's upload froze on its last-fetched status -- under the
  // Queued filter, a row stuck in a queue it already left.
  const pollWhileUnsettled = () =>
    uploadInFlight || unsettledRef.current ? DOCUMENTS_POLL_MS : false;

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
    // Only while something's genuinely in flight -- an already-settled KB
    // sits idle. Caveat: a legacy row permanently stuck in one of
    // isKbDocumentInFlight's statuses (e.g. an extension nothing could
    // index) never settles, so this polls for as long as the tab is open,
    // not only while a real upload is resolving.
    refetchInterval: pollWhileUnsettled,
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

  // A status filter (e.g. QUEUED) can empty out as polling moves rows away,
  // leaving the table blank with no path back but clearing it manually.
  // Rebaselined on filter change (below), so an already-empty filter is left
  // alone -- only one that had matches and lost them all gets reset.
  const lastNonZeroTotalRef = useRef(0);
  const lastStatusFilterRef = useRef(statusFilter);
  useEffect(() => {
    // A failed fetch reports total 0 with loading false (isLoading and
    // isPlaceholderData are both false once a query settles into error), so
    // it must be excluded here too -- otherwise an error on screen gets
    // read as "the filter's rows all moved elsewhere" and silently reset.
    if (loading || documentsQuery.isError) return;
    if (lastStatusFilterRef.current !== statusFilter) {
      // The filter itself just changed -- start tracking fresh rather than
      // judging the new filter by the old one's history, or switching to an
      // already-empty filter would immediately bounce back to "all".
      lastStatusFilterRef.current = statusFilter;
      lastNonZeroTotalRef.current = total;
      return;
    }
    if (statusFilter !== "all" && total === 0 && lastNonZeroTotalRef.current > 0) {
      setStatusFilter("all");
      setPage(1);
      return;
    }
    lastNonZeroTotalRef.current = total;
  }, [statusFilter, total, loading, documentsQuery.isError]);

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

  // An *unfiltered* view (up to 200, the API's page cap), honoring only the
  // search box -- needed because the type/status filter can hide the rows
  // that (1) decide which file-type chips to offer and (2) retire
  // placeholder rows below. Statuses are NOT derived from this: they change
  // within seconds, so the dropdown offers the full fixed list (FilterRow).
  const unfilteredQuery = useQuery({
    queryKey: kbFilesKeys.availableFilters(datapackId, q),
    queryFn: ({ signal }) =>
      getKnowledgeBaseDocuments(datapackId, { page: 1, page_size: 200, q: q || undefined, signal }),
    staleTime: 10_000,
    refetchInterval: pollWhileUnsettled,
    // Sampling failure just means chips fall back to "show everything"
    // (see FilterRow) -- not worth surfacing an error for, so no `error`
    // is read out of this query at all.
  });
  const unfilteredDocuments = unfilteredQuery.data?.items ?? [];

  // Both lists: the page can hold a row the sample missed, and the sample
  // holds rows a status filter hid from the page -- including the file that
  // just left the queue, why the poll must keep running after the page empties.
  const unsettledDocuments =
    documents.some((d) => isKbDocumentInFlight(d.status)) ||
    unfilteredDocuments.some((d) => isKbDocumentInFlight(d.status));
  unsettledRef.current = unsettledDocuments;
  const shouldPollDocuments = uploadInFlight || unsettledDocuments;

  // Placeholder rows for files uploaded but not yet listed by
  // ListDistRagDocuments. Dropped once the real document exists, matched
  // the same way FileUpload matches uploads against pipelineDetails.
  // Matched against the *unfiltered* list, not the current page: under a
  // status filter the page can't contain the row, and keying off it would
  // strand the placeholder on its last known status forever.
  const knownKeys = new Set(
    [...documents, ...unfilteredDocuments].map((d) =>
      sanitizeFilenameForMatch(documentNameLastSegment(d.document_name).replace(/^\d+_/, "")),
    ),
  );
  const unfilteredPlaceholders = Array.from(queuedPlaceholders.entries())
    .filter(([key]) => !knownKeys.has(key))
    .map(([, { displayName, status }]) => ({ name: displayName, status }));
  // The server applies type_filter/status_filter to `documents`; placeholders
  // never went through it, so they have to be filtered here or a QUEUED
  // upload keeps showing under "Indexed".
  const pendingPlaceholders = unfilteredPlaceholders.filter(
    ({ name, status }) =>
      (statusFilter === "all" || status === statusFilter) &&
      (typeFilter === "all" || documentFileExtension(name) === typeFilter),
  );

  const availableTypeFilters = Array.from(
    new Set(
      // `document_type` on a document row is the S3 object's MIME
      // content-type (e.g. "text/csv"), not an extension -- the type
      // filter itself matches a file-extension suffix on the name
      // server-side, so the available-chips sample has to key off the
      // same thing or every chip would filter out every document (see
      // documentFileExtension's doc comment). Unioned with `documents`
      // (the current page) for the same reason as unsettledDocuments above:
      // the current page can hold a row the 200-document sample missed, so
      // a type present only on that row would otherwise offer no chip.
      [
        ...documents.map((d) => documentFileExtension(d.document_name)),
        ...unfilteredDocuments.map((d) => documentFileExtension(d.document_name)),
        // Placeholders too: a file uploaded seconds ago has no document row
        // to sample yet, and its extension is exactly the one just added.
        ...unfilteredPlaceholders.map((p) => documentFileExtension(p.name)),
      ].filter(Boolean),
    ),
  );
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

  // Prune graduated names out of the stored map, not just the rendered list
  // -- otherwise it only grows, leaving hasPendingPlaceholders' poll running
  // forever. Uses the same filter-blind `knownKeys` so a status filter can't
  // keep a name in the map by hiding its row.
  useEffect(() => {
    if (queuedPlaceholders.size === 0) return;
    const known = knownKeys;
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
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `knownKeys` is a new Set every render; the two dataUpdatedAt stamps are the only things that change what's in it, so keying on them runs this exactly once per fetch instead of once per render.
  }, [documentsQuery.dataUpdatedAt, unfilteredQuery.dataUpdatedAt]);

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
