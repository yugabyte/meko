// SharedKnowledge summary page: header (title + Ko pill) → emerald
// auto-generated description tile → 4-stat tile → shared search bar
// → tabs (Files / Promoted memories) → activity log.
//
// Search lives at the page level so it filters whichever tab is
// active. The summary endpoint backs the description + stats in one
// round trip; documents and promoted memories each pull their own
// list; activity-log is independent. Last-activity timestamp comes
// from the most recent activity-log item when available, falling
// back to the summary's last_file_uploaded_at while the log loads.

import { useEffect, useRef, useState } from "react";
import { useLocation, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { FileText, Search, Sparkles } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useCurrentUser } from "@/hooks/useCurrentUser";

import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

// Underline tab variant: TabsTrigger className that strips the default
// shadcn "blue pill" active state (bg-blue-600 + shadow + ring) and
// replaces it with a 2px emerald underline. Used once per trigger;
// extracted because copy/pasting a 200-char className across two
// triggers makes both intent and diffs noisier than they need to be.
// Pair with UNDERLINE_TAB_LIST_CLASS on the TabsList for the matching
// transparent bottom-bordered strip.
const UNDERLINE_TAB_LIST_CLASS =
  "h-auto justify-start gap-1 rounded-none border-b border-border bg-transparent p-0 text-muted-foreground";
const UNDERLINE_TAB_TRIGGER_CLASS =
  "gap-2 rounded-none border-b-2 border-transparent px-3 py-2 " +
  "data-[state=active]:border-emerald-400 data-[state=active]:bg-transparent " +
  "data-[state=active]:text-foreground data-[state=active]:shadow-none data-[state=active]:ring-0";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { CollectiveMemoryPanel } from "@/features/datapacks/components/collective-memory/CollectiveMemoryPanel";
import { KnowledgeSummaryHeader } from "@/features/datapacks/components/knowledge-documents/KnowledgeSummaryHeader";
import { KnowledgeFilesTab } from "@/features/datapacks/components/knowledge-documents/KnowledgeFilesTab";
import { KnowledgeActivityLog } from "@/features/datapacks/components/knowledge-documents/KnowledgeActivityLog";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getDatapack,
  getKnowledgeBaseActivityLog,
  getKnowledgeBaseSummary,
  listMemories,
  type KBActivityLogItem,
} from "@/lib/api/mekoApi";
import { datapackKey, kbLatestActivityKey, kbSummaryKey } from "@/features/datapacks/lib/knowledgeQueryKeys";

type Props = {
  datapackId: string;
};

export default function DatapackKnowledgeSummaryPage({ datapackId }: Props) {
  const qc = useQueryClient();

  // Cached (staleTime 30s) so navigating away and back to this datapack
  // -- or between its sub-pages -- doesn't re-show a loading state for
  // data that hasn't gone stale.
  const datapackQuery = useQuery({
    queryKey: datapackKey(datapackId),
    queryFn: ({ signal }) => getDatapack(datapackId, { signal }),
    staleTime: 30_000,
  });
  const datapack = datapackQuery.data ?? null;
  const datapackLoading = datapackQuery.isLoading;
  const datapackError = datapackQuery.isError ? "Failed to load datapack." : null;

  // Short staleTime -- this backs live-ish stats (total files, promoted
  // count) that should feel current shortly after an upload, but still
  // benefits from not re-fetching on every render/tab switch.
  const summaryQuery = useQuery({
    queryKey: kbSummaryKey(datapackId),
    queryFn: ({ signal }) => getKnowledgeBaseSummary(datapackId, { signal }),
    staleTime: 10_000,
  });
  const summary = summaryQuery.data ?? null;
  const summaryLoading = summaryQuery.isLoading;

  // Caller identity is needed to decide whether the Delete button on
  // each file row is shown. The button is visible when the caller is
  // the datapack owner / a maintainer / the file's uploader; the BE
  // re-enforces this on the request itself. Shared cache with the rest
  // of the app (sidebar, session bootstrap, …) via useCurrentUser --
  // this page no longer runs its own duplicate `/me` fetch.
  const { data: currentUser } = useCurrentUser();

  // The activity-log first page is fetched here so the header's
  // "Last activity" cell can derive its relative-time string from
  // `items[0].timestamp`. The full ActivityLog component owns its own
  // pagination state and fetches its own pages.
  const latestActivityQuery = useQuery({
    queryKey: kbLatestActivityKey(datapackId),
    // First page only — we just need the newest row's timestamp for the
    // "Last activity" header cell, no pagination from here.
    queryFn: ({ signal }) => getKnowledgeBaseActivityLog(datapackId, { limit: 1 }, { signal }),
    staleTime: 10_000,
  });
  const latestActivityAt: string | null =
    (latestActivityQuery.data?.items[0] as KBActivityLogItem | undefined)?.timestamp ?? null;

  const pendingMemoryCount = datapack?.learnings_count ?? 0;

  // Incremented after createKnowledgeBase succeeds to trigger document
  // and activity-log re-fetches without unmounting the components.
  const [refreshKey, setRefreshKey] = useState(0);
  // Small debounce on manual refresh triggers to prevent request storms
  // when multiple components (or multiple status polls) fire at once.
  const refreshTimerRef = useRef<NodeJS.Timeout | null>(null);
  const triggerRefresh = (delay = 100) => {
    if (refreshTimerRef.current) clearTimeout(refreshTimerRef.current);
    refreshTimerRef.current = setTimeout(() => {
      refreshTimerRef.current = null;
      setRefreshKey((k) => k + 1);
    }, delay);
  };

  // Shared across both tabs. Active-tab-aware filtering inside each
  // tab component (files filter by document_name, promoted memories
  // filter by content).
  const [search, setSearch] = useState("");

  // Deep-link params (e.g. from the trace drawer's "Promoted to KB"):
  // `tab` selects the active tab, `promotedMemoryId` is the entry to
  // scroll to + shimmer inside the Promoted memories tab.
  //
  // The three "flash-once" params below are captured into local state
  // via a lazy initializer -- read *once*, on this mount, never again --
  // and then stripped from the URL (see the effect further down). If we
  // instead kept reading them straight off `searchParams` on every
  // render, they'd survive in the URL/history indefinitely, so
  // navigating away and back to this exact page (browser Back, or a
  // link that lands on the same URL) would redeliver the same param and
  // re-trigger the shimmer every time instead of once. `tab` deliberately
  // isn't treated this way -- it's not one-shot, it's "which tab is
  // active", and should stay in the URL for as long as you're here.
  const [searchParams, setSearchParams] = useSearchParams();
  const location = useLocation();
  const state = location.state as {
    highlightDocId?: string;
    flashFiles?: boolean;
    promotedMemoryId?: string;
    tab?: string;
  } | null;

  // Track "consumed" flash intents at the page level. This survives
  // internal pagination and tab switching, ensuring a deep-link arrival
  // only ever flashes once regardless of how much the user navigates
  // around inside this datapack.
  const flashedIdsRef = useRef<Set<string>>(new Set());
  const [flashFilesConsumed, setFlashFilesConsumed] = useState(false);

  // Deep-link intent: capture from location.state (modern internal jump)
  // or URL params (legacy/external link).
  const highlightDocId =
    (state?.highlightDocId || searchParams.get("highlightDocId")) ?? undefined;
  const flashFiles = state?.flashFiles || searchParams.get("flashFiles") === "1";
  const promotedMemoryId =
    (state?.promotedMemoryId || searchParams.get("promotedMemoryId")) ?? "";

  useEffect(() => {
    console.log(
      "[arrival-flash] SummaryPage intent",
      JSON.stringify(
        {
          locationState: state,
          searchParams: Object.fromEntries(searchParams.entries()),
          highlightDocId: highlightDocId ?? null,
          flashFiles,
          promotedMemoryId: promotedMemoryId || null,
          alreadyFlashed: Array.from(flashedIdsRef.current),
          flashFilesConsumed,
        },
        null,
        2,
      ),
    );
  }, [state, searchParams, highlightDocId, flashFiles, promotedMemoryId, flashFilesConsumed]);

  const initialTab =
    state?.tab === "promoted-memories" ||
    searchParams.get("tab") === "promoted-memories" ||
    promotedMemoryId
      ? "promoted-memories"
      : "files";
  const [activeTab, setActiveTab] = useState(initialTab);
  // Re-apply when the deep-link changes while the page is already mounted.
  useEffect(() => {
    const nextTab =
      state?.tab === "promoted-memories" ||
      searchParams.get("tab") === "promoted-memories" ||
      promotedMemoryId
        ? "promoted-memories"
        : "files";
    if (nextTab !== activeTab) {
      setActiveTab(nextTab);
    }
  }, [state?.tab, searchParams, promotedMemoryId, activeTab]);

  // Strip the flash-once params from the URL right after capturing them
  // above -- `replace: true` so this doesn't add a history entry, and so
  // a later browser Back to "this page" lands on the already-stripped
  // URL rather than resurrecting the params that would re-fire the
  // shimmer on remount.
  useEffect(() => {
    if (
      !searchParams.has("promotedMemoryId") &&
      !searchParams.has("flashFiles") &&
      !searchParams.has("highlightDocId")
    ) {
      return;
    }
    const next = new URLSearchParams(searchParams);
    next.delete("promotedMemoryId");
    next.delete("flashFiles");
    next.delete("highlightDocId");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  // Fetch un-promoted memory count for the "X pending" badge.
  // page_size=1 minimises payload — we only need `total`. Currently
  // fire-and-forget (nothing reads the response) -- pendingMemoryCount
  // itself comes from datapack.learnings_count above; kept as a cache
  // warm-up for the Promoted memories tab's own listMemories call.
  useEffect(() => {
    const dpId = datapack?.datapack_id ?? datapackId;
    if (!dpId || !currentUser?.account_id) return;
    const ac = new AbortController();
    void listMemories(
      { datapack_id: dpId, user_id: currentUser.account_id, page: 1, page_size: 1 },
      { signal: ac.signal },
    )
      .then(() => {})
      .catch(() => {});
    return () => ac.abort();
  }, [datapack?.datapack_id, datapackId, currentUser?.account_id]);

  // Bridge `refreshKey` (bumped after createKnowledgeBase / a document
  // delete elsewhere) into cache invalidation for the three queries
  // above -- react-query then refetches them in the background
  // (isFetching, not isLoading/a full reload) since they already have
  // cached data. Guarded so mount doesn't invalidate what it just
  // populated.
  const prevRefreshKeyRef = useRef(refreshKey);
  useEffect(() => {
    if (prevRefreshKeyRef.current === refreshKey) return;
    prevRefreshKeyRef.current = refreshKey;
    void qc.invalidateQueries({ queryKey: kbSummaryKey(datapackId) });
    void qc.invalidateQueries({ queryKey: kbLatestActivityKey(datapackId) });
    void qc.invalidateQueries({ queryKey: datapackKey(datapackId) });
  }, [refreshKey, datapackId, qc]);

  const resolvedDatapackId = datapack?.datapack_id ?? datapackId;
  const displayDatapackName = datapack?.datapack_name ?? "";
  const promotedCount = summary?.promoted_memories_count ?? 0;
  // Use summary.total_files for the tab badge — KnowledgeFilesTab owns
  // its own paginated document fetch, so the parent no longer holds the
  // full document list.
  const filesCount = summary?.total_files ?? 0;

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-7xl flex-1 flex-col space-y-8 px-1 pb-12 pr-1 pt-6">
        <DatapackBreadcrumbs
          items={[
            { label: "Datapacks", href: ROUTES.datapacks },
            { label: displayDatapackName, href: datapackDetailPath(datapackId) },
            { label: "Shared Knowledge" },
          ]}
        />

        {datapackError ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-700 dark:text-amber-100">
            {datapackError}
          </div>
        ) : null}

        <KnowledgeSummaryHeader
          datapackName={displayDatapackName}
          summary={summary}
          loading={summaryLoading}
          lastActivityAt={latestActivityAt}
        />

        {resolvedDatapackId ? (
          <Tabs value={activeTab} onValueChange={setActiveTab} className="w-full">
            {/* Shared search bar above the tab strip — same query
                drives the files-tab filter and the promoted-memories
                filter. Bordered input matches the rest of the app. */}
            <div className="relative mb-3">
              <Search
                className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden
              />
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search across files, memories, and tables…"
                className="border border-border bg-input pl-9"
                aria-label="Search"
              />
            </div>

            <TabsList className={UNDERLINE_TAB_LIST_CLASS}>
              <TabsTrigger value="files" className={UNDERLINE_TAB_TRIGGER_CLASS}>
                <FileText className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
                Files
                <span className="rounded-full bg-background/40 px-1.5 py-0.5 text-[10px] tabular-nums">
                  {filesCount}
                </span>
              </TabsTrigger>
              <TabsTrigger
                value="promoted-memories"
                className={UNDERLINE_TAB_TRIGGER_CLASS}
              >
                <Sparkles className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />
                Promoted memories
                <span className="rounded-full bg-background/40 px-1.5 py-0.5 text-[10px] tabular-nums">
                  {promotedCount}
                </span>
                {pendingMemoryCount > 0 ? (
                  // Amber "X pending" badge: mem0 memories that haven't
                  // been promoted yet. Distinct color from the count
                  // badge so the eye reads "there's work to do" not
                  // "this is the total".
                  <span className="rounded-full border border-amber-500/40 bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-amber-700 dark:text-amber-200">
                    {pendingMemoryCount} pending
                  </span>
                ) : null}
              </TabsTrigger>
            </TabsList>

            <TabsContent value="files" className="mt-6 outline-none">
              <KnowledgeFilesTab
                datapackName={displayDatapackName}
                datapackId={resolvedDatapackId}
                filesSummary={summary?.files_summary ?? ""}
                heaviestSourceName={summary?.heaviest_source_name ?? null}
                embeddingModel={summary?.embedding_model ?? null}
                embeddingDimensions={summary?.embedding_dimensions ?? null}
                chunkSize={summary?.chunk_size ?? null}
                searchQuery={search}
                callerUserId={currentUser?.account_id ?? null}
                datapackGrant={datapack?.grant ?? null}
                refreshKey={refreshKey}
                onDocumentDeleted={() => {
                  // Non-optimistic: increment refreshKey, which invalidates
                  // /documents, /summary, and /activity-log's caches above.
                  triggerRefresh(150);
                }}
                onKnowledgeBaseCreated={() => {
                  triggerRefresh(100);
                }}
                flash={flashFiles && !flashFilesConsumed}
                onFlashConsumed={() => setFlashFilesConsumed(true)}
                highlightDocumentId={
                  highlightDocId && !flashedIdsRef.current.has(highlightDocId)
                    ? highlightDocId
                    : undefined
                }
                onHighlightConsumed={(id) => flashedIdsRef.current.add(id)}
              />
            </TabsContent>

            <TabsContent value="promoted-memories" className="mt-6 outline-none">
              <CollectiveMemoryPanel
                datapackName={displayDatapackName}
                datapackId={resolvedDatapackId}
                searchQuery={search}
                pendingCount={pendingMemoryCount}
                embeddingModel={summary?.embedding_model ?? null}
                embeddingDimensions={summary?.embedding_dimensions ?? null}
                aiProvider={summary?.ai_provider ?? null}
                highlightMemoryId={
                  promotedMemoryId && !flashedIdsRef.current.has(promotedMemoryId)
                    ? promotedMemoryId
                    : undefined
                }
                onHighlightConsumed={(id) => flashedIdsRef.current.add(id)}
              />
            </TabsContent>
          </Tabs>
        ) : (
          <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
            {datapackLoading ? "Loading datapack…" : "Datapack not available."}
          </div>
        )}

        {resolvedDatapackId ? (
          <KnowledgeActivityLog
            datapackId={resolvedDatapackId}
            refreshKey={refreshKey}
            pageSize={10}
          />
        ) : null}
      </main>
    </div>
  );
}
