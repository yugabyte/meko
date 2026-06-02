// Renders agent memories with promote-to-KB actions (full paginated list).
// UI-only; state and API calls live in useMemoryPromotion.

import { useCallback, useEffect, useState } from "react";
import {
  ArrowLeftRight,
  Loader2,
  AlertCircle,
  CheckCircle2,
  ArrowUpToLine,
  RefreshCw,
  ChevronLeft,
  ChevronRight,
  Search,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import {
  PAGE_SIZE_OPTIONS,
  useMemoryPromotion,
} from "@/features/datapacks/components/memory-promotion/useMemoryPromotion";
import { MemoryRowBody } from "@/features/datapacks/components/memory-promotion/memoryRowBody";

type Props = {
  datapackName: string;
  datapackId: string;
  isPromotionAllowed: boolean;
  /** Fired after a successful promote with the count of just-promoted memories.
   *  Parent typically refetches the datapack so the learnings count in the
   *  pipeline ribbon refreshes. The toast itself is handled here. */
  onPromoted?: (count: number) => void;
};

export default function MemoryPromotionPanel({
  datapackName,
  datapackId,
  isPromotionAllowed,
  onPromoted,
}: Props) {
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedSearch(search);
    }, 300);
    return () => clearTimeout(timer);
  }, [search]);
  const handlePromoted = useCallback(
    (count: number) => {
      toast({
        title:
          count === 1
            ? "1 memory promoted to knowledge"
            : `${count} memories promoted to knowledge`,
      });
      onPromoted?.(count);
    },
    [onPromoted],
  );
  const {
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
    keep,
    keepOne,
    accountId,
  } = useMemoryPromotion({ datapackId, onPromoted: handlePromoted, q: debouncedSearch });

  const showPagination = total > 0;

  const patternCount = unpromotedMemories.length;
  /** Count + empty copy only after account is known and listMemories has settled once (avoids the frame where total is still 0 before fetch). */
  const sessionSummaryReady =
    !accountLoading && accountError == null && Boolean(accountId) && memoriesSummaryReady;

  return (
    <TooltipProvider delayDuration={200}>
      <div className="rounded-xl border border-border bg-card p-6 space-y-4">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-4 min-w-0">
            <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-purple-500/15 ring-1 ring-purple-500/10">
              {/* Mirror the `text-blue-700 dark:text-blue-200` convention
                  used by `<MemoryReadOnlySummaryPanel>`'s header icon —
                  hard-coded `text-white` rendered as greyed-out white on
                  the light purple tint in light mode. */}
              <ArrowLeftRight className="h-5 w-5 text-purple-700 dark:text-purple-200" strokeWidth={1.75} aria-hidden />
            </div>
            <div className="min-w-0">
              <h3 className="text-base font-semibold tracking-tight text-foreground">
                <span>Learnings Summary</span>
                {sessionSummaryReady ? (
                  <span>
                    {" "}
                    · {total} {total === 1 ? "memory" : "memories"}
                  </span>
                ) : (
                  <span className="font-normal text-muted-foreground"> · …</span>
                )}
              </h3>
              {sessionSummaryReady && total === 0 ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  No session memories yet for{" "}
                  <span className="font-medium text-foreground">{datapackName}</span>.
                </p>
              ) : sessionSummaryReady ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Meko spotted{" "}
                  <span className="font-semibold text-foreground">
                    {"these"} {patternCount === 1 ? "pattern" : "patterns"}
                  </span>{" "}
                  that might be worth promoting to shared knowledge (Ko). Review each one — approved ones
                  become visible to every agent in this datapack.
                </p>
              ) : accountError ? (
                <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
                  Account could not be loaded for promotions. Details appear below.
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
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {accountError}
          </div>
        )}

        {error && (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {error}
          </div>
        )}

        {memories.length > 0 && (
          <div className="flex items-center justify-between rounded-lg border border-border bg-card px-4 py-2.5">
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
                    ? "Nothing to promote on this page"
                    : `Select all selectable on this page (${selectableIds.length}${total > memories.length ? ` of ${total}` : ""})`}
              </span>
            </label>
            {<Button
              className="gap-2 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
              onClick={() => void promote()}
              disabled={promoting || selected.size < 2 || !isPromotionAllowed}
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
            </Button>}
            <Button
              variant="outline"
              className="gap-2"
              onClick={() => void keep()}
              disabled={promoting || selected.size < 2}
            >
              {selected.size > 1
                ? `Keep as memory (${selected.size})`
                : "Keep as memory"}
            </Button>
          </div>
        )}

        <div className="relative">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search learnings…"
            className="border border-border bg-background pl-9"
            aria-label="Search learnings"
          />
        </div>

        {promoteError && (
          <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-700 dark:text-red-300">
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
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-card px-4 py-2.5 text-xs text-muted-foreground">
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
                  Page <span className="text-foreground">{page}</span> of {totalPages}
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

        {memories.length > 0 && (
          <>
            <ul className="space-y-2">
              {unpromotedMemories.map((m) => {
                const checked = selected.has(m.id);
                // When the user has multi-selected (2+), suppress per-row
                // promote so an accidental click can't promote just one row
                // while their intent is the bulk action. The top "Promote to
                // Knowledge (N)" button is the only path in that mode.
                const multiSelectActive = selected.size >= 2;
                const rowPromoteDisabled =
                  promoting || !isPromotionAllowed || multiSelectActive;
                return (
                  <li
                    key={m.id}
                    // `bg-card` to match the row styling on
                    // `<MemoryReadOnlySummaryPanel>` (Memories Summary)
                    // — both pages should share the same row-tile surface
                    // so navigating between them doesn't feel like
                    // switching designs.
                    className={cn(
                      "flex items-start gap-3 rounded-lg border bg-card px-4 py-3",
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
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="inline-flex">
                            <Button
                              type="button"
                              size="sm"
                              className="gap-1.5 bg-purple-100 hover:bg-purple-200 text-purple-700 dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white"
                              onClick={() => promoteOne(m.id)}
                              disabled={rowPromoteDisabled}
                            >
                              {promoting && promotingId === m.id ? (
                                <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0" aria-hidden />
                              ) : (
                                <ArrowUpToLine className="h-3.5 w-3.5 shrink-0" aria-hidden />
                              )}
                              Promote to Knowledge
                            </Button>
                          </span>
                        </TooltipTrigger>
                        {!isPromotionAllowed ? (
                          <TooltipContent side="top">You don't have permission to promote to knowledge</TooltipContent>
                        ) : multiSelectActive ? (
                          <TooltipContent side="top">
                            Multiple rows selected — use the “Promote to Knowledge ({selected.size})” button above.
                          </TooltipContent>
                        ) : null}
                      </Tooltip>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="inline-flex">
                            <Button
                              type="button"
                              variant="outline"
                              size="sm"
                              className="border-border bg-background/80"
                              onClick={() => keepOne(m.id)}
                              disabled={promoting || multiSelectActive}
                            >
                              Keep as memory
                            </Button>
                          </span>
                        </TooltipTrigger>
                        {multiSelectActive ? (
                          <TooltipContent side="top">
                            Multiple rows selected — use the “Keep as memory ({selected.size})” button above.
                          </TooltipContent>
                        ) : (
                          <TooltipContent side="top">
                            Mark this memory as kept; it drops off the Learnings list but stays in Memories.
                          </TooltipContent>
                        )}
                      </Tooltip>
                    </div>
                  </li>
                );
              })}

            </ul>
          </>
        )}
      </div>
    </TooltipProvider>
  );
}
