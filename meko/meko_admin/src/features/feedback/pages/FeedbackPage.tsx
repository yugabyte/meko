// Admin Feedback page — paged, date-range-filtered view of
// meko_system.feedback. Backed by GET /api/admin/feedback (proxied
// through the meko_admin node server to api_server).
//
// UX defaults: last 30 days (matches MetricsPage's window), 50 rows
// per page. Row body is truncated in the table; clicking opens a
// dialog with the full text so long feedback stays readable.

import { useState } from "react";
import { useQuery, keepPreviousData } from "@tanstack/react-query";
import { format, formatDistanceToNow, parseISO, subDays } from "date-fns";
import { ChevronLeft, ChevronRight, ExternalLink } from "lucide-react";
import { fetchAdminFeedback, type AdminFeedbackItem } from "@/lib/adminApi";
import { SingleDatePicker } from "@/features/dashboard/components/SingleDatePicker";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// Page size for the table. Kept as a constant so the pagination footer
// arithmetic and the API request stay in step; small enough to render
// quickly, large enough that most sessions never page through more
// than once or twice.
const PAGE_SIZE = 50;

// How many characters of the body to show before the "…" truncation.
// Chosen so a two-sentence report fits inline and anything longer
// prompts a click into the modal.
const BODY_PREVIEW_CHARS = 160;

/** Truncate long body text for the inline table cell; the full body
 *  is available via the row-click modal so nothing is hidden from
 *  the operator. */
function truncateBody(body: string): string {
  const clean = body.trim();
  if (clean.length <= BODY_PREVIEW_CHARS) return clean;
  return clean.slice(0, BODY_PREVIEW_CHARS).trimEnd() + "…";
}

/** Category → soft badge tone. Keeps `bug` red-ish for scanability
 *  while other categories stay neutral. */
function categoryBadgeVariant(cat: string): "default" | "secondary" | "destructive" | "outline" {
  const c = cat.toLowerCase();
  if (c === "bug") return "destructive";
  if (c === "feature_request") return "default";
  return "secondary";
}

/** Prettify the raw enum values from the DB. `feature_request` reads
 *  better as `Feature`; `Other` is already capitalised. */
function formatCategory(cat: string): string {
  if (cat === "feature_request") return "Feature";
  return cat.charAt(0).toUpperCase() + cat.slice(1);
}

export function FeedbackPage() {
  // Default window: last 30 days inclusive. Same convention MetricsPage
  // uses so the two admin pages share muscle memory.
  const [startDate, setStartDate] = useState<Date | undefined>(subDays(new Date(), 29));
  const [endDate, setEndDate] = useState<Date | undefined>(new Date());
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<AdminFeedbackItem | null>(null);

  const from = startDate ? format(startDate, "yyyy-MM-dd") : undefined;
  const to = endDate ? format(endDate, "yyyy-MM-dd") : undefined;

  const feedback = useQuery({
    queryKey: ["admin-feedback", from, to, PAGE_SIZE, offset],
    queryFn: () => fetchAdminFeedback({ from, to, limit: PAGE_SIZE, offset }),
    staleTime: 30_000,
    // Keep the previous page visible while a new page loads so the
    // table doesn't blank out on paging clicks.
    placeholderData: keepPreviousData,
  });

  const data = feedback.data;
  const total = data?.pagination.total ?? 0;
  const items = data?.feedback ?? [];
  const rangeStart = total === 0 ? 0 : offset + 1;
  const rangeEnd = Math.min(offset + items.length, total);
  const canPrev = offset > 0;
  const canNext = offset + PAGE_SIZE < total;

  const handleStartChange = (d: Date | undefined) => {
    setStartDate(d);
    setOffset(0);
  };
  const handleEndChange = (d: Date | undefined) => {
    setEndDate(d);
    setOffset(0);
  };

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Feedback</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            Product feedback submitted from the app
            {from && to ? ` · ${from} → ${to}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SingleDatePicker
            label="Start"
            value={startDate}
            onChange={handleStartChange}
            disabled={(d) => (endDate ? d > endDate : false)}
          />
          <SingleDatePicker
            label="End"
            value={endDate}
            onChange={handleEndChange}
            disabled={(d) => d > new Date() || (startDate ? d < startDate : false)}
          />
        </div>
      </div>

      {feedback.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Couldn't load feedback — {(feedback.error as Error)?.message ?? "unknown error"}.
          </AlertDescription>
        </Alert>
      )}

      <div className="rounded-md border border-border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-40">Created</TableHead>
              <TableHead className="w-28">Category</TableHead>
              <TableHead className="w-32">Source</TableHead>
              <TableHead className="w-64">User</TableHead>
              <TableHead>Feedback</TableHead>
              <TableHead className="w-24">Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {feedback.isLoading && !data ? (
              Array.from({ length: 5 }).map((_, i) => (
                <TableRow key={`skeleton-${i}`}>
                  <TableCell colSpan={6}>
                    <Skeleton className="h-6 w-full" />
                  </TableCell>
                </TableRow>
              ))
            ) : items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} className="py-10 text-center text-sm text-muted-foreground">
                  No feedback in this date range.
                </TableCell>
              </TableRow>
            ) : (
              items.map((row) => (
                <TableRow
                  key={row.id}
                  className="cursor-pointer hover:bg-accent/40"
                  onClick={() => setSelected(row)}
                >
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                    {formatDistanceToNow(parseISO(row.created_at), { addSuffix: true })}
                    <div className="text-[10px] opacity-70">
                      {format(parseISO(row.created_at), "MMM d, yyyy HH:mm")}
                    </div>
                  </TableCell>
                  <TableCell>
                    <Badge variant={categoryBadgeVariant(row.category)}>
                      {formatCategory(row.category)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs">{row.source}</TableCell>
                  <TableCell
                    className="truncate text-xs"
                    title={row.source_identifier ?? row.user_id ?? ""}
                  >
                    {row.source_identifier ?? row.user_id ?? "—"}
                  </TableCell>
                  <TableCell className="text-sm">{truncateBody(row.body)}</TableCell>
                  <TableCell>
                    <Badge variant="outline" className="text-[10px] uppercase tracking-wide">
                      {row.status}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>

      {/* Pagination footer — mirrors the "Showing X-Y of N" convention
          used elsewhere in admin. Only rendered when the query has
          returned at least once so the counts don't jump from empty
          to populated. */}
      {data ? (
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <div>
            {total === 0
              ? "No results"
              : `Showing ${rangeStart}-${rangeEnd} of ${total.toLocaleString()}`}
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={!canPrev || feedback.isFetching}
              onClick={() => setOffset((n) => Math.max(0, n - PAGE_SIZE))}
              className="gap-1"
            >
              <ChevronLeft className="h-3.5 w-3.5" aria-hidden />
              Previous
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!canNext || feedback.isFetching}
              onClick={() => setOffset((n) => n + PAGE_SIZE)}
              className="gap-1"
            >
              Next
              <ChevronRight className="h-3.5 w-3.5" aria-hidden />
            </Button>
          </div>
        </div>
      ) : null}

      <FeedbackDetailDialog
        item={selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
      />
    </div>
  );
}

/** Full-body modal shown when an admin clicks a table row. Includes
 *  the immutable audit fields (id, created_at, meko_version) so
 *  downstream investigations have everything without a DB query. */
function FeedbackDetailDialog({
  item,
  onOpenChange,
}: {
  item: AdminFeedbackItem | null;
  onOpenChange: (open: boolean) => void;
}) {
  const open = item != null;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Feedback details</DialogTitle>
          <DialogDescription>
            {item ? (
              <span className="flex flex-wrap gap-x-3 gap-y-1 text-xs">
                <span>
                  <span className="text-muted-foreground">Received</span>{" "}
                  {format(parseISO(item.created_at), "MMM d, yyyy HH:mm")}
                </span>
                <span>
                  <span className="text-muted-foreground">Category</span>{" "}
                  {formatCategory(item.category)}
                </span>
                <span>
                  <span className="text-muted-foreground">Source</span> {item.source}
                </span>
                <span>
                  <span className="text-muted-foreground">Status</span> {item.status}
                </span>
              </span>
            ) : null}
          </DialogDescription>
        </DialogHeader>

        {item ? (
          <div className="space-y-4">
            <DetailRow label="User" value={item.source_identifier ?? item.user_id ?? "—"} mono />
            {item.datapack_name || item.datapack_id ? (
              <DetailRow
                label="Datapack"
                value={
                  item.datapack_name
                    ? `${item.datapack_name}${item.datapack_id ? ` (${item.datapack_id})` : ""}`
                    : (item.datapack_id ?? "")
                }
                mono
              />
            ) : null}
            {item.context_route ? (
              <DetailRow label="From page" value={item.context_route} mono />
            ) : null}
            {item.meko_version ? (
              <DetailRow label="Meko version" value={item.meko_version} mono />
            ) : null}

            <div>
              <div className="mb-1 text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                Feedback
              </div>
              <div className="whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-sm">
                {item.body}
              </div>
            </div>
          </div>
        ) : null}

        <DialogFooter className="gap-2 sm:justify-between">
          <span className="text-[10px] text-muted-foreground">Feedback ID · {item?.id ?? ""}</span>
          <Button variant="outline" onClick={() => onOpenChange(false)} className="gap-1.5">
            Close
            <ExternalLink className="h-3.5 w-3.5 opacity-0" aria-hidden />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DetailRow({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-3">
      <span className="w-28 shrink-0 text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <span
        className={
          mono
            ? "min-w-0 flex-1 break-all font-mono text-xs"
            : "min-w-0 flex-1 break-words text-sm"
        }
      >
        {value}
      </span>
    </div>
  );
}
