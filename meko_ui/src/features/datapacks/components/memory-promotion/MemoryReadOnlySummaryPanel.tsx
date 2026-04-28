/**
 * Read-only memories: same card chrome as Learnings summary, without promote / keep actions.
 * Preview shows top {@link MEMORY_READ_ONLY_PREVIEW_SIZE} rows + optional “View all” to the memory-summary route.
 */
import { Link } from "react-router-dom";
import { ArrowLeftRight, ChevronLeft, ChevronRight, Loader2, AlertCircle, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { datapackMemorySummaryPath } from "@/app/routes/paths";
import { MEMORY_READ_ONLY_PREVIEW_SIZE, useMemoryReadOnlyList, type MemoryReadOnlyVariant } from "./useMemoryReadOnlyList";
import { PAGE_SIZE_OPTIONS } from "./useMemoryPromotion";
import { MemoryRowBody } from "./memoryRowBody";

type Props = {
  datapackName: string;
  datapackId: string;
  variant: MemoryReadOnlyVariant;
};

export function MemoryReadOnlySummaryPanel({ datapackName, datapackId, variant }: Props) {
  const {
    isFull,
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    accountLoading,
    accountError,
    sessionSummaryReady,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
  } = useMemoryReadOnlyList({ datapackId, variant });

  const showPagination = isFull && total > 0;
  const showViewAll = !isFull && total > MEMORY_READ_ONLY_PREVIEW_SIZE;

  return (
    <div className="rounded-xl border border-border bg-[#1C2333] p-6 space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="flex min-w-0 items-center gap-4">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/15 ring-1 ring-blue-500/10">
            <ArrowLeftRight className="h-5 w-5 text-blue-200" strokeWidth={1.75} aria-hidden />
          </div>
          <div className="min-w-0">
            <h3 className="text-base font-semibold tracking-tight text-white">
              <span>Memories</span>
              {sessionSummaryReady ? (
                <span>
                  {" "}
                  · {total} {total === 1 ? "memory" : "memories"}
                </span>
              ) : (
                <span className="font-normal text-muted-foreground"> · …</span>
              )}
            </h3>
            {accountError ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                Account could not be loaded for this list. Details appear below.
              </p>
            ) : sessionSummaryReady ? (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                Recent session memories for{" "}
                <span className="font-medium text-white">{datapackName}</span>
                {isFull ? "." : ` (showing up to ${MEMORY_READ_ONLY_PREVIEW_SIZE} here).`}
              </p>
            ) : (
              <p className="mt-1 text-xs leading-relaxed text-muted-foreground">Loading memories…</p>
            )}
          </div>
        </div>
        <Button
          variant="outline"
          className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
          onClick={() => void loadMemories()}
          disabled={loading || accountLoading || !datapackId}
        >
          {loading || accountLoading ? (
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
          ) : (
            <RefreshCw className="h-4 w-4" aria-hidden />
          )}
          {loading || accountLoading ? "Loading..." : "Refresh"}
        </Button>
      </div>

      {accountError ? (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
          <AlertCircle className="h-4 w-4 shrink-0" aria-hidden />
          {accountError}
        </div>
      ) : null}

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

      {memories.length === 0 && !loading && !error && sessionSummaryReady ? (
        <p className="text-xs text-muted-foreground">No memories found for this datapack yet.</p>
      ) : null}

      {memories.length > 0 ? (
        <>
          <ul className="space-y-2">
            {memories.map((m) => (
              <li
                key={m.id}
                className="rounded-lg border border-border bg-[#0E1525] px-4 py-3"
              >
                <MemoryRowBody memory={m} />
              </li>
            ))}
          </ul>
          {showViewAll ? (
            <div className="flex justify-center pt-1">
              <Button
                variant="outline"
                className="min-w-[18rem] max-w-full border-border bg-background/80 px-6 py-2.5 text-sm sm:min-w-[22rem]"
                asChild
              >
                <Link to={datapackMemorySummaryPath(datapackName)}>View all memories</Link>
              </Button>
            </div>
          ) : null}
        </>
      ) : null}
    </div>
  );
}
