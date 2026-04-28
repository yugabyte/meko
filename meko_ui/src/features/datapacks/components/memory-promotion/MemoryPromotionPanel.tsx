// Renders agent memories with promote-to-KB actions: preview (3 rows + optional View All) or full pagination.
// UI-only; state and API calls live in useMemoryPromotion so detail and learnings routes stay in sync.

import { Link } from "react-router-dom";
import {
  ArrowLeftRight,
  Loader2,
  AlertCircle,
  CheckCircle2,
  ArrowUpToLine,
  RefreshCw,
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { datapackLearningsPath } from "@/app/routes/paths";
import {
  PAGE_SIZE_OPTIONS,
  PREVIEW_PAGE_SIZE,
  useMemoryPromotion,
  type MemoryPromotionVariant,
} from "@/features/datapacks/components/memory-promotion/useMemoryPromotion";
import { MemoryRowBody } from "@/features/datapacks/components/memory-promotion/memoryRowBody";

type Props = {
  datapackName: string;
  datapackId: string;
  /** `preview`: shows 3 unpromoted rows (fetches a larger window so slots refill after promote). `full`: paginated list. */
  variant?: MemoryPromotionVariant;
};

export default function MemoryPromotionPanel({
  datapackName,
  datapackId,
  variant = "full",
}: Props) {
  const {
    isPreview,
    loading,
    memoriesSummaryReady,
    error,
    memories,
    selected,
    page,
    pageSize,
    total,
    totalPages,
    accountLoading,
    accountError,
    promoting,
    promotingId,
    promoteError,
    lastOutcome,
    promotedMemories,
    unpromotedMemories,
    selectableIds,
    allChecked,
    someChecked,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
    toggleOne,
    toggleAll,
    promote,
    promoteOne,
    accountId,
  } = useMemoryPromotion({ datapackId, variant });

  const showPagination = !isPreview && total > 0;
  // More than PREVIEW_PAGE_SIZE memories exist server-side — link out to the full learnings page.
  const showViewAll = isPreview && total > PREVIEW_PAGE_SIZE;

  const patternCount = unpromotedMemories.length;
  /** Count + empty copy only after account is known and listMemories has settled once (avoids the frame where total is still 0 before fetch). */
  const sessionSummaryReady =
    !accountLoading && accountError == null && Boolean(accountId) && memoriesSummaryReady;

  return (
    <TooltipProvider delayDuration={200}>
      <div className="rounded-xl border border-border bg-[#1C2333] p-6 space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-4 min-w-0">
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-purple-500/15 ring-1 ring-purple-500/10">
              <ArrowLeftRight className="h-5 w-5 text-white" strokeWidth={1.75} aria-hidden />
            </div>
            <div className="min-w-0">
              <h3 className="text-base font-semibold tracking-tight text-white">
                <span>Learnings Summary</span>
                {sessionSummaryReady ? (
                  <span>
                    {" "}
                    · {total - (promotedMemories.length ?? 0)} new {total === 1 ? "memory" : "memories"}
                  </span>
                ) : (
                  <span className="font-normal text-muted-foreground"> · …</span>
                )}
              </h3>
              {sessionSummaryReady && total === 0 ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  No session memories yet for{" "}
                  <span className="font-medium text-white">{datapackName}</span>.
                </p>
              ) : sessionSummaryReady && patternCount === 0 && total > 0 ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Every memory on this view is already promoted to shared knowledge (Ko).
                  {isPreview && total > PREVIEW_PAGE_SIZE ? (
                    <> Use View All to open the full learnings summary.</>
                  ) : !isPreview ? (
                    <> Use Prev/Next or refresh to review more from this datapack.</>
                  ) : null}
                </p>
              ) : sessionSummaryReady ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Meko spotted{" "}
                  <span className="font-semibold text-white">
                    {"these"} {patternCount === 1 ? "pattern" : "patterns"}
                  </span>{" "}
                  that might be worth promoting to shared knowledge (Ko). Review each one — approved ones
                  become visible to every agent in this datapack.
                </p>
              ) : (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">Loading session summary…</p>
              )}
            </div>
          </div>
          <Button
            variant="outline"
            className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
            onClick={() => void loadMemories()}
            disabled={loading || accountLoading || !accountId}
          >
            {loading || accountLoading ? (
              <Loader2 className="w-4 h-4 animate-spin" />
            ) : (
              <RefreshCw className="w-4 h-4" />
            )}
            {loading || accountLoading ? "Loading..." : "Refresh"}
          </Button>
        </div>

        {accountError && (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {accountError}
          </div>
        )}

        {error && (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {error}
          </div>
        )}

        {memories.length > 0 && (
          <div className="flex items-center justify-between rounded-lg border border-border bg-[#0E1525] px-4 py-2.5">
            <label className="flex items-center gap-2 cursor-pointer">
              <Checkbox
                checked={allChecked ? true : someChecked ? "indeterminate" : false}
                onCheckedChange={toggleAll}
                disabled={selectableIds.length === 0}
              />
              <span className="text-xs text-muted-foreground">
                {selected.size > 0
                  ? `${selected.size} of ${selectableIds.length} selectable on this page`
                  : selectableIds.length === 0
                    ? `Nothing to promote on this page (${promotedMemories.length} already promoted)`
                    : `Select all selectable on this page (${selectableIds.length}${total > memories.length ? ` of ${total}` : ""})`}
              </span>
            </label>
            <Button
              className="gap-2 bg-purple-600 hover:bg-purple-700 text-white"
              onClick={() => void promote()}
              disabled={promoting || selected.size < 2}
            >
              {promoting ? (
                <Loader2 className="w-4 h-4 animate-spin" />
              ) : (
                <ArrowUpToLine className="w-4 h-4" />
              )}
              {promoting
                ? "Promoting..."
                : selected.size > 1
                  ? `Promote to Knowledge (${selected.size})`
                  : "Promote to Knowledge"}
            </Button>
          </div>
        )}

        {promoteError && (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {promoteError}
          </div>
        )}
        {lastOutcome && (
          <div className="flex items-center gap-2 rounded-lg border border-green-500/30 bg-green-500/10 px-4 py-2 text-xs text-green-300">
            <CheckCircle2 className="w-4 h-4 shrink-0" />
            Promoted {lastOutcome.attempted} selected · {lastOutcome.inserted} inserted · {lastOutcome.updated}{" "}
            updated
            {lastOutcome.notFound > 0 && (
              <>
                {" "}
                · {lastOutcome.notFound} skipped
              </>
            )}
          </div>
        )}

        {memories.length === 0 && !loading && !error && (
          <p className="text-xs text-muted-foreground">No memories found for this datapack yet.</p>
        )}

        {showPagination && (
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-2.5 text-xs text-muted-foreground">
            <div>
              {total === 0 ? "0 memories" : `Showing ${rangeStart}–${rangeEnd} of ${total}`}
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
                  onClick={() => goToPage(page - 1)}
                  disabled={loading || page <= 1}
                  aria-label="Previous page"
                >
                  <ChevronLeft className="w-4 h-4" />
                  Prev
                </Button>
                <span className="px-2 text-muted-foreground">
                  Page <span className="text-white">{page}</span> of {totalPages}
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  className="gap-1 px-2"
                  onClick={() => goToPage(page + 1)}
                  disabled={loading || page >= totalPages}
                  aria-label="Next page"
                >
                  Next
                  <ChevronRight className="w-4 h-4" />
                </Button>
              </div>
            </div>
          </div>
        )}

        {(memories.length > 0 || promotedMemories.length > 0) && (
          <>
            <ul className="space-y-2">
              {unpromotedMemories.map((m) => {
                const checked = selected.has(m.id);
                return (
                  <li
                    key={m.id}
                    className={cn(
                      "flex items-start gap-3 rounded-lg border bg-[#0E1525] px-4 py-3",
                      checked ? "border-purple-500/50 bg-purple-500/5" : "border-border",
                    )}
                  >
                    <Checkbox
                      checked={checked}
                      onCheckedChange={() => toggleOne(m.id)}
                      className="mt-0.5 shrink-0"
                    />
                    <div className="min-w-0 flex-1 space-y-1">
                      <MemoryRowBody memory={m} />
                    </div>
                    <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
                      <Button
                        type="button"
                        size="sm"
                        className="gap-1.5 bg-purple-600 hover:bg-purple-700 text-white"
                        onClick={() => promoteOne(m.id)}
                        disabled={promoting}
                      >
                        {promoting && promotingId === m.id ? (
                          <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0" aria-hidden />
                        ) : (
                          <ArrowUpToLine className="h-3.5 w-3.5 shrink-0" aria-hidden />
                        )}
                        Promote to Knowledge
                      </Button>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="inline-flex">
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              className="border-border bg-background/80 text-muted-foreground"
                              disabled
                            >
                              Keep as memory
                            </Button>
                          </span>
                        </TooltipTrigger>
                        <TooltipContent side="top">Coming Soon</TooltipContent>
                      </Tooltip>
                    </div>
                  </li>
                );
              })}

              {promotedMemories.length > 0 && (
                <li
                  className="pt-2 pb-1 text-[10px] uppercase tracking-wider text-muted-foreground/70"
                  aria-hidden
                >
                  Already promoted · {promotedMemories.length}
                </li>
              )}

              {promotedMemories.map((m) => (
                <li
                  key={m.id}
                  className="flex items-start gap-3 rounded-lg border border-dashed border-border/60 bg-[#0E1525]/40 px-4 py-3 opacity-50"
                  aria-label="Already promoted to knowledge base"
                >
                  <Checkbox checked={false} disabled className="mt-0.5 cursor-not-allowed" />
                  <div className="min-w-0 flex-1 space-y-1">
                    <MemoryRowBody
                      memory={m}
                      textClassName="line-through decoration-muted-foreground/40"
                    />
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
                      <span className="inline-flex items-center gap-1 rounded-full border border-green-500/30 bg-green-500/10 px-2 py-0.5 text-[10px] font-medium text-green-300">
                        <CheckCircle2 className="w-3 h-3" />
                        Promoted to KB
                      </span>
                    </div>
                  </div>
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
                  <Link to={datapackLearningsPath(datapackName)}>View all promotion suggestions</Link>
                </Button>
              </div>
            ) : null}
          </>
        )}
      </div>
    </TooltipProvider>
  );
}
