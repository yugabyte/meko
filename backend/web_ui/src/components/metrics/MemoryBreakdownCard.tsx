// Memories by where they stand, three groups that add up to the total: promoted to the collective
// memory (shared with every agent through the knowledge base), kept private, and learnings still
// waiting to be kept or promoted.

import { count } from "../../lib/format";
import { pct } from "../../lib/metrics";
import { Button } from "../ui";
import { BigStat, MetricCard } from "./MetricCard";

export function MemoryBreakdownCard({
  promoted,
  kept,
  pending,
  onReview,
}: {
  promoted: number;
  kept: number;
  pending: number;
  onReview: () => void;
}) {
  const total = promoted + kept + pending;
  const title = "Memory breakdown";
  const subtitle = "Shared vs private vs pending promotion";
  if (total === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No memories yet", "Agents add them with memory_add."]} />;
  }
  const parts = [
    { label: "Promoted", n: promoted, color: "bg-info" },
    { label: "Private", n: kept, color: "bg-info-strong" },
    { label: "Pending", n: pending, color: "bg-muted-foreground" },
  ];
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="flex gap-6">
        <BigStat value={count(total)} label="Total memories" />
        <div className="w-px bg-border" />
        <BigStat value={`${pct(promoted, total)}%`} label="Shared" />
      </div>
      <div className="mt-4 flex h-2.5 overflow-hidden rounded-full bg-muted">
        {parts.map((p) => (p.n > 0 ? <div key={p.label} className={p.color} style={{ width: `${(100 * p.n) / total}%` }} /> : null))}
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
        {parts.map((p) => (
          <span key={p.label} className="flex items-center gap-1.5">
            <span className={`size-2 rounded-full ${p.color}`} /> {p.label} <b>{count(p.n)}</b>
          </span>
        ))}
        {pending > 0 && (
          <Button size="sm" variant="ghost" className="ml-auto text-primary" onClick={onReview}>
            Review pending
          </Button>
        )}
      </div>
    </MetricCard>
  );
}
