// "Context Size - by token count" and "…by raw data size" — the same card twice, differing
// only in which measure it totals, so it takes the three already-computed
// parts rather than knowing about either unit.

import { MetricCard } from "./MetricCard";
import { ContextLegendRow, ContextStackedBar } from "./ContextLegendRow";

export type ContextBreakdown = {
  knowledge: number;
  memory: number;
  conversation: number;
};

function pct(part: number, total: number): number {
  return total > 0 ? (part / total) * 100 : 0;
}

export function TotalContextCard({
  title,
  subtitle,
  id,
  breakdown,
  format,
  emptyLabel,
  /** Rendered after the headline number, e.g. the "tokens" suffix. */
  unitSuffix,
}: {
  title: string;
  subtitle: string;
  id?: string;
  breakdown: ContextBreakdown;
  format: (value: number) => string;
  emptyLabel: string;
  unitSuffix?: string;
}) {
  const total = breakdown.knowledge + breakdown.memory + breakdown.conversation;

  return (
    <MetricCard title={title} subtitle={subtitle} id={id}>
      <p className="text-3xl font-bold tabular-nums text-foreground">
        {total === 0 ? "—" : format(total)}
        {total > 0 && unitSuffix && (
          <span className="ml-1 text-base font-normal text-muted-foreground">{unitSuffix}</span>
        )}
      </p>

      {total > 0 ? (
        <>
          <ContextStackedBar
            segments={[
              { concept: "knowledge", pct: pct(breakdown.knowledge, total) },
              { concept: "memory", pct: pct(breakdown.memory, total) },
              { concept: "conversation", pct: pct(breakdown.conversation, total) },
            ]}
          />
          <div className="space-y-1.5">
            <ContextLegendRow
              concept="knowledge"
              value={format(breakdown.knowledge)}
              pct={pct(breakdown.knowledge, total)}
            />
            <ContextLegendRow
              concept="memory"
              value={format(breakdown.memory)}
              pct={pct(breakdown.memory, total)}
            />
            <ContextLegendRow
              concept="conversation"
              value={format(breakdown.conversation)}
              pct={pct(breakdown.conversation, total)}
            />
          </div>
        </>
      ) : (
        <p className="text-xs text-muted-foreground">{emptyLabel}</p>
      )}
    </MetricCard>
  );
}
