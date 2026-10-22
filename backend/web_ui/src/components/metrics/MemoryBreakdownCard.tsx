// Memories by where they stand: promoted to the collective memory (shared with every agent through
// the knowledge base), kept private, and learnings still waiting to be kept or promoted.

import { count } from "../../lib/format";
import { pct } from "../../lib/metrics";
import { Button } from "../ui";
import { BigStat, MetricCard } from "./MetricCard";

export function MemoryBreakdownCard({
  promoted,
  privateCount,
  pending,
  onReview,
}: {
  promoted: number;
  privateCount: number;
  pending: number;
  onReview: () => void;
}) {
  const total = promoted + privateCount;
  const title = "Memory breakdown";
  const subtitle = "Shared vs private vs pending promotion";
  if (total === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No memories yet", "Agents add them with memory_add."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="flex gap-6">
        <BigStat value={count(total)} label="Total memories" />
        <div className="w-px bg-border" />
        <BigStat value={`${pct(promoted, total)}%`} label="Shared" />
      </div>
      <div className="mt-4 flex h-2.5 overflow-hidden rounded-full bg-muted">
        <div className="bg-info" style={{ width: `${(100 * promoted) / total}%` }} />
        <div className="bg-info-strong" style={{ width: `${(100 * privateCount) / total}%` }} />
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
        <span className="flex items-center gap-1.5">
          <span className="size-2 rounded-full bg-info" /> Promoted <b>{count(promoted)}</b>
        </span>
        <span className="flex items-center gap-1.5">
          <span className="size-2 rounded-full bg-info-strong" /> Private <b>{count(privateCount)}</b>
        </span>
        {pending > 0 && (
          <Button size="sm" variant="ghost" className="ml-auto text-primary" onClick={onReview}>
            Review {count(pending)} pending
          </Button>
        )}
      </div>
    </MetricCard>
  );
}
