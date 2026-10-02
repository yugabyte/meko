// Summary badge row + per-address results table for the bulk invite
// tab. Rendered only after a submit — before that, the parent hides
// this component entirely so the tab reads as "fresh form" rather than
// "empty table".
//
// The table is keyed by email (not by index) because the server
// de-duplicates case-insensitively, so N inputs can return fewer than
// N rows.
import { UsersRound } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { cn } from "@/lib/utils";
import type { DatapackBulkShareResult } from "@/lib/api/mekoApi";
import {
  bulkShareStatusPresentation,
  type BulkShareSummary,
  type BulkShareStatusTone,
} from "@/features/datapacks/components/datapack-share/bulkShareEmails";

/** Tone → text-colour tokens for the Status cell. The classifier
 *  itself (bulkShareStatusPresentation) lives in bulkShareEmails.ts;
 *  this map only knows how to paint the tone tokens it returns. */
const TONE_CLASS: Record<BulkShareStatusTone, string> = {
  success: "text-emerald-700 dark:text-emerald-300",
  neutral: "text-muted-foreground",
  warning: "text-amber-700 dark:text-amber-300",
  error: "text-destructive",
};

export type BulkInviteResultsPanelProps = {
  results: DatapackBulkShareResult[];
  summary: BulkShareSummary;
};

export function BulkInviteResultsPanel({ results, summary }: BulkInviteResultsPanelProps) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="brand" className="px-2 py-0.5 text-xs font-semibold tabular-nums">
          {summary.invited} invited
        </Badge>
        <span className="text-xs text-muted-foreground">
          {summary.unchanged} unchanged · {summary.failed} failed · {summary.total} rows
        </span>
        {summary.newUsers > 0 ? (
          <span className="text-xs text-amber-700 dark:text-amber-300">
            {summary.newUsers} {summary.newUsers === 1 ? "address has" : "addresses have"} no Meko
            account yet — they must sign up to accept.
          </span>
        ) : null}
      </div>
      <div className="max-h-80 overflow-auto rounded-lg border border-border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-2/3">Email</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {results.map((r) => {
              const p = bulkShareStatusPresentation(r.status);
              // Per-row API message is still surfaced, just as a
              // hover title on the Status cell rather than a full
              // Detail column — the column was line-noise for the
              // common "Invite sent / Failed" case and is hidden on
              // mobile anyway. Operators who need to drill into a
              // failure hover the cell to read the exact message.
              const hoverDetail = r.message || p.detail;
              return (
                <TableRow key={r.email}>
                  <TableCell className="font-mono text-xs">{r.email}</TableCell>
                  <TableCell
                    className={cn("text-xs font-medium", TONE_CLASS[p.tone])}
                    title={hoverDetail}
                  >
                    {p.label}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
      <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <UsersRound className="h-3.5 w-3.5 shrink-0" aria-hidden />
        Addresses are de-duplicated case-insensitively, so this list can be shorter than what you
        submitted.
      </p>
    </div>
  );
}
