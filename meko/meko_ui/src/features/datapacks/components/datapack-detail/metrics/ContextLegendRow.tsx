// One "● Knowledge Base … 64% · 16K" line under a stacked context bar.
// Takes a ContextConcept rather than a colour class, so the dot can never
// disagree with the bar segment it labels.

import { cn } from "@/lib/utils";
import { CONTEXT_LABEL, CONTEXT_TONE, type ContextConcept } from "@/lib/contextTones";

export function ContextLegendRow({
  concept,
  label,
  value,
  pct,
}: {
  concept: ContextConcept;
  /** Overrides the concept's default label where a card names it differently. */
  label?: string;
  value: string;
  pct: number;
}) {
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      <div className={cn("h-2 w-2 shrink-0 rounded-full", CONTEXT_TONE[concept].fill)} />
      <span>{label ?? CONTEXT_LABEL[concept]}</span>
      <span className="ml-auto tabular-nums text-foreground/80">
        <span className="font-semibold">{Math.round(pct)}%</span>
        <span className="ml-1 text-muted-foreground">· {value}</span>
      </span>
    </div>
  );
}

/** The stacked bar the legend above describes. Segments are given in the same
 *  order as the legend rows, so the two read top-to-bottom together. */
export function ContextStackedBar({
  segments,
}: {
  segments: { concept: ContextConcept; pct: number }[];
}) {
  return (
    <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
      {segments.map((s) => (
        <div
          key={s.concept}
          className={CONTEXT_TONE[s.concept].fill}
          style={{ width: `${s.pct}%` }}
        />
      ))}
    </div>
  );
}
