// Read-only paginated Collective Memory list (collective-memory-summary
// route). Same card chrome as MemoryReadOnlySummaryPanel, backed by
// GET /promoted-memories. Each row deep-links into the conversation viewer.

import { Link } from "react-router-dom";
import { AlertCircle, ChevronLeft, ChevronRight, Loader2, RefreshCw, Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  CONVERSATION_SOURCE,
  datapackConversationsPathWithSource,
} from "@/app/routes/paths";
import { PAGE_SIZE_OPTIONS } from "@/features/datapacks/components/memory-promotion/useMemoryPromotion";
import { usePromotedMemoryList } from "./usePromotedMemoryList";
import { PromotedMemoryRowBody } from "./PromotedMemoryRowBody";

type Props = {
  datapackName: string;
  datapackId: string;
};

export function CollectiveMemoryPanel({ datapackName, datapackId }: Props) {
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

  const showPagination = total > 0;

  return (
    <div className="rounded-xl border border-border bg-[#1C2333] p-6 space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="flex min-w-0 items-center gap-4">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-sky-500/15 ring-1 ring-sky-500/10">
            <Sparkles className="h-5 w-5 text-sky-300" strokeWidth={1.75} aria-hidden />
          </div>
          <div className="min-w-0">
            <h3 className="text-base font-semibold tracking-tight text-white">
              <span>Collective Memory</span>
              {listReady ? (
                <span>
                  {" "}
                  · {total} {total === 1 ? "memory" : "memories"}
                </span>
              ) : (
                <span className="font-normal text-muted-foreground"> · …</span>
              )}
            </h3>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
              Memories promoted into the tenant knowledge base for{" "}
              <span className="font-medium text-white">{datapackName}</span>.
            </p>
          </div>
        </div>
        <Button
          variant="outline"
          className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
          onClick={() => void loadMemories()}
          disabled={loading || !datapackId}
        >
          {loading ? (
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
          ) : (
            <RefreshCw className="h-4 w-4" aria-hidden />
          )}
          {loading ? "Loading..." : "Refresh"}
        </Button>
      </div>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          {error}
        </div>
      ) : null}

      {showPagination ? (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-2.5 text-xs text-muted-foreground">
          <div>{total === 0 ? "0 memories" : `Showing ${rangeStart}–${rangeEnd} of ${total}`}</div>
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
                Page <span className="text-white">{page}</span> of {totalPages}
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
        <p className="text-xs text-muted-foreground">
          No promoted memories yet. Promote session memories from the Memories tab to populate this list.
        </p>
      ) : null}

      {memories.length > 0 ? (
        <>
          <ul className="space-y-2">
            {memories.map((m) => {
              const conversationId = m.conversation_id ?? "";
              return (
                <li key={m.id}>
                  {conversationId ? (
                    <Link
                      to={datapackConversationsPathWithSource(
                        datapackName,
                        conversationId,
                        CONVERSATION_SOURCE.collectiveMemorySummary,
                        undefined,
                        // Carry datapack_id through (name collisions
                        // across share boundaries) — see paths.ts.
                        datapackId,
                      )}
                      title={`Open conversation ${conversationId}`}
                      className="block rounded-lg border border-border bg-[#0E1525] px-4 py-3 transition-colors hover:bg-[#161d2e] hover:border-border/80 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                    >
                      <PromotedMemoryRowBody memory={m} />
                    </Link>
                  ) : (
                    <div className="rounded-lg border border-border bg-[#0E1525] px-4 py-3">
                      <PromotedMemoryRowBody memory={m} />
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        </>
      ) : null}
    </div>
  );
}
