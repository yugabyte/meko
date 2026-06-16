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

import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { FileText, Search, Sparkles } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

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
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { CollectiveMemoryPanel } from "@/features/datapacks/components/collective-memory/CollectiveMemoryPanel";
import { KnowledgeSummaryHeader } from "@/features/datapacks/components/knowledge-documents/KnowledgeSummaryHeader";
import { KnowledgeFilesTab } from "@/features/datapacks/components/knowledge-documents/KnowledgeFilesTab";
import { KnowledgeActivityLog } from "@/features/datapacks/components/knowledge-documents/KnowledgeActivityLog";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  getApiErrorMessage,
  getCurrentUser,
  getDatapack,
  getKnowledgeBaseActivityLog,
  getKnowledgeBaseSummary,
  listMemories,
  type CurrentUser,
  type Datapack,
  type KBActivityLogItem,
  type KnowledgeBaseSummary,
} from "@/lib/api/mekoApi";

type Props = {
  datapackId: string;
};

export default function DatapackKnowledgeSummaryPage({ datapackId }: Props) {
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [datapackLoading, setDatapackLoading] = useState(true);
  const [datapackError, setDatapackError] = useState<string | null>(null);

  const [summary, setSummary] = useState<KnowledgeBaseSummary | null>(null);
  const [summaryLoading, setSummaryLoading] = useState(true);

  // Caller identity is needed to decide whether the Delete button on
  // each file row is shown. The button is visible when the caller is
  // the datapack owner / a maintainer / the file's uploader; the BE
  // re-enforces this on the request itself. Failing the fetch is
  // non-fatal — we just hide Delete entirely for safety.
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);

  // The activity-log first page is fetched here so the header's
  // "Last activity" cell can derive its relative-time string from
  // `items[0].timestamp`. The full ActivityLog component owns its own
  // pagination state and fetches its own pages.
  const [latestActivityAt, setLatestActivityAt] = useState<string | null>(null);

  const [pendingMemoryCount, setPendingMemoryCount] = useState(0);

  // Incremented after createKnowledgeBase succeeds to trigger document
  // and activity-log re-fetches without unmounting the components.
  const [refreshKey, setRefreshKey] = useState(0);

  // Shared across both tabs. Active-tab-aware filtering inside each
  // tab component (files filter by document_name, promoted memories
  // filter by content).
  const [search, setSearch] = useState("");

  // Deep-link params (e.g. from the trace drawer's "Promoted to KB"):
  // `tab` selects the active tab, `promotedMemoryId` is the entry to
  // scroll to + shimmer inside the Promoted memories tab.
  const [searchParams] = useSearchParams();
  const promotedMemoryId = searchParams.get("promotedMemoryId") ?? "";
  const initialTab =
    searchParams.get("tab") === "promoted-memories" || promotedMemoryId
      ? "promoted-memories"
      : "files";
  const [activeTab, setActiveTab] = useState(initialTab);
  // Re-apply when the deep-link changes while the page is already mounted.
  useEffect(() => {
    if (searchParams.get("tab") === "promoted-memories" || promotedMemoryId) {
      setActiveTab("promoted-memories");
    }
  }, [searchParams, promotedMemoryId]);

  // Caller identity — used by KnowledgeFilesTab for per-row Delete
  // visibility. Lives on the page (not deeper) so the children get a
  // stable prop and we don't refetch on tab switch.
  useEffect(() => {
    const ac = new AbortController();
    void getCurrentUser({ signal: ac.signal })
      .then((u) => {
        if (!ac.signal.aborted) setCurrentUser(u);
      })
      .catch(() => {
        // Silent: Delete just stays hidden if we can't resolve the
        // caller. The session-bootstrap path also fetches this, so
        // a transient failure here is recoverable on next navigation.
      });
    return () => ac.abort();
  }, []);

  // Fetch un-promoted memory count for the "X pending" badge.
  // page_size=1 minimises payload — we only need `total`.
  useEffect(() => {
    const dpId = datapack?.datapack_id ?? datapackId;
    if (!dpId || !currentUser?.account_id) return;
    const ac = new AbortController();
    void listMemories(
      { datapack_id: dpId, user_id: currentUser.account_id, page: 1, page_size: 1 },
      { signal: ac.signal },
    )
      .then((r) => { })
      .catch(() => { });
    return () => ac.abort();
  }, [datapack?.datapack_id, datapackId, currentUser?.account_id]);

  // Resolve the datapack once to confirm existence + get its id (needed
  // by the Promoted memories tab below).
  useEffect(() => {
    let cancelled = false;
    setDatapackLoading(true);
    setDatapackError(null);
    void getDatapack(datapackId, {})
      .then((d) => {
        setPendingMemoryCount(d.learnings_count ?? 0);
        if (!cancelled) setDatapack(d);
      })
      .catch((e) => {
        if (!cancelled) {
          setDatapack(null);
          setDatapackError(getApiErrorMessage(e, "Failed to load datapack."));
        }
      })
      .finally(() => {
        if (!cancelled) setDatapackLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [datapackId]);

  useEffect(() => {
    const ac = new AbortController();
    setSummaryLoading(true);
    void getKnowledgeBaseSummary(datapackId, { signal: ac.signal })
      .then((s) => {
        if (ac.signal.aborted) return;
        setSummary(s);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        if ((e as { name?: string })?.name === "CanceledError") return;
        setSummary(null);
      })
      .finally(() => {
        if (!ac.signal.aborted) setSummaryLoading(false);
      });
    return () => ac.abort();
  }, [datapackId]);

  // First-page activity-log fetch JUST for the "Last activity" header
  // cell. Best-effort: we don't surface errors here because the full
  // ActivityLog component does its own fetch + shows its own banner.
  useEffect(() => {
    const ac = new AbortController();
    void getKnowledgeBaseActivityLog(
      datapackId,
      // First page only — we just need the newest row's timestamp
      // for the "Last activity" header cell, no pagination from here.
      { limit: 1 },
      { signal: ac.signal },
    )
      .then((resp) => {
        if (ac.signal.aborted) return;
        const first: KBActivityLogItem | undefined = resp.items[0];
        setLatestActivityAt(first?.timestamp ?? null);
      })
      .catch(() => {
        // Silent; the header falls back to summary.last_file_uploaded_at.
      });
    return () => ac.abort();
  }, [datapackId]);

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
                <Sparkles className="h-3.5 w-3.5 text-purple-700 dark:text-purple-300" aria-hidden />
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
                  // Non-optimistic: increment refreshKey to re-fetch
                  // /documents and /activity-log, then also refresh /summary.
                  setRefreshKey((k) => k + 1);
                  setSummaryLoading(true);
                  void getKnowledgeBaseSummary(datapackId, {})
                    .then((s) => setSummary(s))
                    .catch(() => { })
                    .finally(() => setSummaryLoading(false));
                }}
                onKnowledgeBaseCreated={() => setRefreshKey((k) => k + 1)}
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
                highlightMemoryId={promotedMemoryId || undefined}
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
