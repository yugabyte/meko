// A total split three ways (knowledge base, memory, conversations): the total, a stacked bar, and a
// row per part with its share.

import { pct, type Split } from "../../lib/metrics";
import { BigStat, MetricCard } from "./MetricCard";

const PARTS = [
  { key: "knowledge", label: "Knowledge base", color: "bg-success" },
  { key: "memory", label: "Memory", color: "bg-info" },
  { key: "conversation", label: "Conversations", color: "bg-muted-foreground" },
] as const;

export function SplitCard({
  title,
  subtitle,
  split,
  format,
  unit,
}: {
  title: string;
  subtitle: string;
  split: Split;
  format: (n: number) => string;
  unit?: string;
}) {
  if (split.total === 0) {
    return <MetricCard title={title} subtitle={subtitle} empty={["Nothing stored yet", "Add memories or files to see their footprint."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <BigStat value={format(split.total)} unit={unit} />
      <div className="mt-4 flex h-2.5 overflow-hidden rounded-full bg-muted">
        {PARTS.map((p) =>
          split[p.key] > 0 ? (
            <div key={p.key} className={p.color} style={{ width: `${(100 * split[p.key]) / split.total}%` }} />
          ) : null,
        )}
      </div>
      <div className="mt-4 space-y-1.5 text-sm">
        {PARTS.map((p) => (
          <div key={p.key} className="flex items-center gap-2">
            <span className={`size-2 rounded-full ${p.color}`} />
            <span className="text-muted-foreground">{p.label}</span>
            <span className="ml-auto tabular-nums">
              <b>{pct(split[p.key], split.total)}%</b>
              <span className="text-muted-foreground"> · {format(split[p.key])}</span>
            </span>
          </div>
        ))}
      </div>
    </MetricCard>
  );
}
