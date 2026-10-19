// Read-only paginated Collective Memory list — the "Promoted memories"
// tab on the SharedKnowledge page (and the standalone collective-memory
// summary route).
//
// Layout:
//   1. AUTO summary banner: "N promoted from <datapack>. M candidates
//      awaiting review." with a "Review M pending" CTA on the right.
//   2. Pagination + per-page strip.
//   3. PromotedMemoryCard list — rich per-row layout (chips, body,
//      collapsible source memory, footer with index-level chips).
//
// Data sources:
//   - /promoted-memories       → list + per-row enrichment (promoted_by_email)
//   - /knowledge-bases/summary → envelope-level constants
//                                 (embedding_model / dimensions / ai_provider)
//                                 passed in as props so the panel never
//                                 has to know about the summary endpoint
//   - /datapack/<name>         → pending count (mem0 memories not yet
//                                 promoted) passed in as `pendingCount`

import { useCallback, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  AlertCircle,
  ArrowLeftRight,
  ChevronLeft,
  ChevronRight,
  Loader2,
  RefreshCw,
  Sparkles,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import {
  CONVERSATION_SOURCE,
  LEARNINGS_SOURCE,
  datapackConversationsPathWithSource,
  datapackLearningsPathFrom,
} from "@/app/routes/paths";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";
import { PAGE_SIZE_OPTIONS } from "@/features/datapacks/components/memory-promotion/useMemoryPromotion";
import {
  API_ERROR_CODE_PRE_MEM0_V2_MEMORY,
  deletePromotedMemory,
  getApiErrorMessage,
  isMekoApiRequestError,
  isPostMem0V2,
  type PromotedMemory,
} from "@/lib/api/mekoApi";
import { isDatapackGrantEligibleToShare, type DatapackShareGrant } from "@/lib/datapackShareGrants";
import { usePromotedMemoryList } from "./usePromotedMemoryList";
import { PromotedMemoryCard } from "./PromotedMemoryCard";
import { PromotedMemoryDeleteDialog } from "./PromotedMemoryDeleteDialog";

type Props = {
  datapackName: string;
  datapackId: string;
  /** Optional client-side filter applied to the currently-loaded page.
   *  When set, rows whose `data` doesn't contain the query
   *  (case-insensitive) are hidden. Pagination + total still operate on
   *  the unfiltered server result; only the visible rows shrink. */
  searchQuery?: string;
  /** mem0 memories that haven't been promoted yet — sourced from
   *  /datapack/<name>.memory_count by the parent page. We do NOT
   *  fetch this from /summary on purpose (see file header). */
  pendingCount?: number;
  /** Index-level constants for the footer chips. Sourced from
   *  /knowledge-bases/summary; pass null when the page hasn't fetched
   *  /summary (the chips degrade gracefully). */
  embeddingModel?: string | null;
  embeddingDimensions?: number | null;
  aiProvider?: string | null;
  /** Deep-link target (from the trace drawer's "Promoted to KB" link).
   *  When this id matches a loaded promoted memory, that card is scrolled
   *  into view and given a one-time shimmer sweep. */
  highlightMemoryId?: string;
  /** Fired after the highlight card has genuinely been found and shimmered. */
  onHighlightConsumed?: (id: string) => void;
  /** Caller's grant on this datapack. The per-card Delete affordance is
   *  gated to owner/maintainer (contributors/viewers see no button); the
   *  server enforces the same rule with 403 FORBIDDEN, so this is a UX
   *  gate and not a security boundary. */
  datapackGrant?: DatapackShareGrant | null;
};

export function CollectiveMemoryPanel({
  datapackName,
  datapackId,
  searchQuery = "",
  pendingCount = 0,
  embeddingModel = null,
  embeddingDimensions = null,
  aiProvider = null,
  highlightMemoryId,
  onHighlightConsumed,
  datapackGrant = null,
}: Props) {
  const navigate = useNavigate();
  const {
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    listReady,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
  } = usePromotedMemoryList({ datapackId });

  // Apply the page-level search filter to the currently-loaded page.
  // Memoized so search-typing doesn't re-filter when the upstream
  // memory list hasn't changed.
  const visibleMemories = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    if (!q) return memories;
    return memories.filter((m) => (m.data ?? "").toLowerCase().includes(q));
  }, [memories, searchQuery]);

  // Owner/maintainer (via Share helper) mirrors the server's owner-or-maintainer
  // gate on DELETE /promoted-memories/:id — do NOT switch to the Delete-named
  // helper: that one is owner-only and would silently make the client stricter
  // than the server.
  const canDelete = isDatapackGrantEligibleToShare({
    grant: datapackGrant ?? undefined,
  });

  // Single-flight delete state (mirrors MemoryReadOnlySummaryPanel).
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [pendingDeleteMemory, setPendingDeleteMemory] = useState<PromotedMemory | null>(null);

  const handleDelete = useCallback(
    async (memoryId: string) => {
      if (deletingId) return;
      setDeletingId(memoryId);
      try {
        await deletePromotedMemory(memoryId, datapackId);
        toast({ title: "Collective memory deleted" });
        setPendingDeleteMemory(null);
        await loadMemories();
      } catch (err) {
        // Race safety net — UI already hides the button for pre-cutoff rows.
        if (isMekoApiRequestError(err) && err.code === API_ERROR_CODE_PRE_MEM0_V2_MEMORY) {
          toast({ title: "Can't delete", description: err.message });
        } else {
          toast({
            title: "Couldn't delete",
            description: getApiErrorMessage(err, "Delete failed."),
          });
        }
      } finally {
        setDeletingId(null);
      }
    },
    [datapackId, deletingId, loadMemories],
  );

  // Deep-link highlight: when highlightMemoryId matches a card on the
  // currently-loaded page, scroll it into view and flash it once.
  // `active` is what makes this wait for the right page -- the target
  // may be several pages in, or filtered out by the search box.
  const targetOnPage = visibleMemories.some((m) => m.id === highlightMemoryId);
  const { ref: targetRef, flashing } = useArrivalFlash<HTMLLIElement>({
    flashKey: highlightMemoryId,
    active: targetOnPage,
    onDone: onHighlightConsumed,
    debugLabel: "PromotedMemoryCard",
  });

  const showPagination = total > 0;
  const hasPending = pendingCount > 0;

  return (
    <div className="space-y-4">
      {/* One-time sweep for a deep-linked promoted memory. White reads on
          the dark card but vanishes on light, so light mode uses a visible
          blue tint (matching the memories accent) instead. */}
      <style>{`
        @keyframes meko-promoted-shimmer {
          0%   { transform: translateX(-100%); }
          100% { transform: translateX(100%); }
        }
        @keyframes meko-promoted-glow {
          0%   { background-color: rgba(56,189,248,0.22); }
          70%  { background-color: rgba(56,189,248,0.12); }
          100% { background-color: rgba(56,189,248,0); }
        }
        .meko-promoted-shimmer {
          animation: meko-promoted-shimmer 1.2s ease-in-out forwards;
          background: linear-gradient(100deg, transparent 20%, rgba(37,99,235,0.40) 50%, transparent 80%);
        }
        .dark .meko-promoted-shimmer {
          background: linear-gradient(100deg, transparent 20%, rgba(255,255,255,0.32) 50%, transparent 80%);
        }
        .meko-promoted-glow {
          animation: meko-promoted-glow 1.6s ease-out forwards;
        }
      `}</style>
      {/* AUTO summary banner. Always rendered (even when there's
          nothing pending) so the tab keeps a stable visual anchor;
          the right-side CTA only appears when there is real work to
          review. */}
      <section className="rounded-xl border border-sky-500/25 bg-sky-500/[0.04] p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 flex-1">
            <div className="mb-1 flex items-center gap-1.5">
              <Sparkles className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />
              <span className="text-[10px] font-semibold uppercase tracking-wider text-sky-700 dark:text-sky-200/95">
                Promoted memories summary · auto
              </span>
            </div>
            <p className="text-sm leading-relaxed text-foreground/90">
              {listReady ? (
                <>
                  <span className="font-semibold text-foreground">{total}</span>{" "}
                  {total === 1 ? "memory" : "memories"} promoted from{" "}
                  <code className="rounded bg-background/40 px-1 py-0.5 text-[12px] text-foreground/90">
                    {datapackName}
                  </code>
                  .{" "}
                  {hasPending ? (
                    <>
                      <span className="text-sky-700 dark:text-sky-300">
                        {pendingCount} additional candidate
                        {pendingCount === 1 ? "" : "s"}
                      </span>{" "}
                      <span className="text-muted-foreground">awaiting review.</span>
                    </>
                  ) : (
                    <span className="text-muted-foreground">
                      No additional candidates awaiting review.
                    </span>
                  )}
                </>
              ) : (
                <span className="text-muted-foreground">Loading promoted memories…</span>
              )}
            </p>
          </div>
          {hasPending ? (
            // Amber CTA matches the "X pending" badge on the tab —
            // same color language, same affordance: this is the
            // place to act on un-promoted memories. Deep-links into
            // the Learnings page with ?from=shared-knowledge so the
            // breadcrumb trail there grows an extra hop back to
            // this Shared Knowledge view.
            <Button
              variant="outline"
              type="button"
              className="gap-2"
              onClick={() =>
                navigate(datapackLearningsPathFrom(datapackName, LEARNINGS_SOURCE.sharedKnowledge))
              }
              title="Review pending mem0 candidates"
            >
              <ArrowLeftRight className="h-4 w-4" aria-hidden />
              Review {pendingCount} pending
            </Button>
          ) : null}
        </div>
      </section>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300">
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          {error}
        </div>
      ) : null}

      {showPagination ? (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-input px-4 py-2.5 text-xs text-muted-foreground">
          <div className="flex items-center gap-3">
            <span>
              {total === 0 ? "0 memories" : `Showing ${rangeStart}–${rangeEnd} of ${total}`}
            </span>
            <Button
              variant="ghost"
              size="sm"
              className="h-7 gap-1 px-2 text-muted-foreground hover:text-foreground"
              onClick={() => void loadMemories()}
              disabled={loading || !datapackId}
              title="Refresh"
            >
              {loading ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : (
                <RefreshCw className="h-3.5 w-3.5" aria-hidden />
              )}
            </Button>
          </div>
          <div className="flex items-center gap-3">
            <label className="flex items-center gap-2">
              <span>Per page</span>
              <select
                value={pageSize}
                onChange={(e) => changePageSize(Number(e.target.value))}
                disabled={loading}
                className="rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50"
              >
                {PAGE_SIZE_OPTIONS.map((opt) => (
                  <option key={opt} value={opt}>
                    {opt}
                  </option>
                ))}
              </select>
            </label>
            <div className="flex items-center gap-1">
              <Button
                variant="outline"
                size="sm"
                className="gap-1 px-2"
                type="button"
                onClick={() => goToPage(page - 1)}
                disabled={loading || page <= 1}
                aria-label="Previous page"
              >
                <ChevronLeft className="h-4 w-4" />
                Prev
              </Button>
              <span className="px-2 text-muted-foreground">
                Page <span className="text-foreground">{page}</span> of {totalPages}
              </span>
              <Button
                variant="outline"
                size="sm"
                className="gap-1 px-2"
                type="button"
                onClick={() => goToPage(page + 1)}
                disabled={loading || page >= totalPages}
                aria-label="Next page"
              >
                Next
                <ChevronRight className="h-4 w-4" />
              </Button>
            </div>
          </div>
        </div>
      ) : null}

      {memories.length === 0 && !loading && !error && listReady ? (
        <p className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-12 text-center text-sm text-muted-foreground">
          No promoted memories yet. Promote session memories from the Memories tab to populate this
          list.
        </p>
      ) : null}

      {/* Search returned zero matches on a non-empty page — distinct
          from "no memories at all" empty state above. */}
      {memories.length > 0 && visibleMemories.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No promoted memories on this page match your search.
        </p>
      ) : null}

      {visibleMemories.length > 0 ? (
        <ul className="space-y-3">
          {visibleMemories.map((m) => {
            const conversationId = m.conversation_id ?? "";
            const traceConversationId = conversationId.replace(/-/g, "");
            // Deep-link to the originating conversation. Built here (not in
            // the card) so the dash-stripped id + datapack_id handling stays
            // in one place. The card renders it as an explicit button so the
            // rest of the card (e.g. the Source memory dropdown) stays
            // clickable instead of the whole row being one big link.
            const conversationHref = conversationId
              ? datapackConversationsPathWithSource(
                  datapackId,
                  traceConversationId,
                  CONVERSATION_SOURCE.collectiveMemorySummary,
                  { memoryId: m.id },
                )
              : undefined;
            const isTarget = !!highlightMemoryId && m.id === highlightMemoryId;
            const isShimmering = isTarget && flashing;
            return (
              <li
                key={m.id}
                ref={isTarget ? targetRef : undefined}
                className={cn(
                  "relative transition-shadow duration-300",
                  isShimmering &&
                    "z-10 overflow-hidden rounded-lg ring-2 ring-sky-500/60 dark:ring-sky-400/60",
                )}
              >
                {isShimmering ? (
                  <>
                    <span
                      aria-hidden
                      className="meko-promoted-glow pointer-events-none absolute inset-0 z-10 rounded-lg"
                    />
                    <span
                      aria-hidden
                      className="meko-promoted-shimmer pointer-events-none absolute inset-0 z-10 rounded-lg"
                    />
                  </>
                ) : null}
                <div className="rounded-lg border border-border bg-card px-4 py-3">
                  <PromotedMemoryCard
                    memory={m}
                    datapackName={datapackName}
                    conversationHref={conversationHref}
                    embeddingModel={embeddingModel}
                    embeddingDimensions={embeddingDimensions}
                    aiProvider={aiProvider}
                    canDelete={canDelete}
                    isDeleting={deletingId === m.id}
                    deleteInFlight={deletingId !== null}
                    onDeleteClick={() => setPendingDeleteMemory(m)}
                  />
                </div>
              </li>
            );
          })}
        </ul>
      ) : null}

      {/* Confirm-delete dialog. Mirrors the private-delete flow in
          MemoryReadOnlySummaryPanel: the destructive action stays open
          while the request is in flight so the user sees the spinner,
          and the memory text is shown inline so they re-read what they
          are about to erase for the whole datapack. */}
      <PromotedMemoryDeleteDialog
        memory={pendingDeleteMemory}
        deleting={deletingId !== null}
        onCancel={() => setPendingDeleteMemory(null)}
        onConfirm={(memoryId) => void handleDelete(memoryId)}
      />
    </div>
  );
}
