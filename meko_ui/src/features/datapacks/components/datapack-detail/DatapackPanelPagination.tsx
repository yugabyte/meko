// Shared pagination bar for datapack-detail panels (Learnings,
// Conversations, …). Renders the same row that MemoryPromotionPanel
// previously had inline:
//
//   ┌───────────────────────────────────────────────────────────────┐
//   │ Showing 1–10 of 42  |  Per page [10▾]  Prev  Page 1 of 5  Next │
//   └───────────────────────────────────────────────────────────────┘
//
// State is fully owned by the parent (so the parent can fetch on page
// change). This component is presentation-only.

import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";

export type DatapackPanelPaginationProps = {
  page: number;
  pageSize: number;
  total: number;
  totalPages: number;
  rangeStart: number;
  rangeEnd: number;
  pageSizeOptions: readonly number[];
  /** Disables Prev/Next/select while a fetch is in flight. */
  loading?: boolean;
  /** Singular/plural label for the row count text (e.g. memory/memories, session/sessions). */
  itemNoun?: { singular: string; plural: string };
  onPrev: () => void;
  onNext: () => void;
  onPageSizeChange: (size: number) => void;
};

export default function DatapackPanelPagination({
  page,
  pageSize,
  total,
  totalPages,
  rangeStart,
  rangeEnd,
  pageSizeOptions,
  loading,
  itemNoun = { singular: "item", plural: "items" },
  onPrev,
  onNext,
  onPageSizeChange,
}: DatapackPanelPaginationProps) {
  const noun = total === 1 ? itemNoun.singular : itemNoun.plural;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-2.5 text-xs text-muted-foreground">
      <div>{total === 0 ? `0 ${itemNoun.plural}` : `Showing ${rangeStart}–${rangeEnd} of ${total} ${noun}`}</div>
      <div className="flex items-center gap-3">
        <label className="flex items-center gap-2">
          <span>Per page</span>
          <select
            value={pageSize}
            onChange={(e) => onPageSizeChange(Number(e.target.value))}
            disabled={loading}
            className="rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50"
          >
            {pageSizeOptions.map((opt) => (
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
            onClick={onPrev}
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
            onClick={onNext}
            disabled={loading || page >= totalPages}
            aria-label="Next page"
          >
            Next
            <ChevronRight className="w-4 h-4" />
          </Button>
        </div>
      </div>
    </div>
  );
}
