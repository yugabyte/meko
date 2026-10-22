// Two numbers side by side with an optional footnote: the knowledge base's chunks and files, or
// conversations and the memories extracted from them.

import type { ReactNode } from "react";
import { BigStat, MetricCard } from "./MetricCard";

export function StatPairCard({
  title,
  subtitle,
  stats,
  footnote,
}: {
  title: string;
  subtitle: string;
  stats: [string, string][]; // [value, label]
  footnote?: ReactNode;
}) {
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="flex gap-6">
        {stats.map(([value, label], i) => (
          <div key={label} className="flex gap-6">
            {i > 0 && <div className="w-px bg-border" />}
            <BigStat value={value} label={label} />
          </div>
        ))}
      </div>
      {footnote && <div className="mt-4 text-xs text-muted-foreground">{footnote}</div>}
    </MetricCard>
  );
}
