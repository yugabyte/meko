// Horizontal bars comparing input-token counts for the same review done two
// ways. Widths are relative to the largest row, so the gap is the point.

import { cn } from "@/lib/utils";

export type TokenCostBar = {
  label: string;
  tokens: number;
  /** Marks the row as the expensive baseline rather than the measured run —
   *  drives the muted fill and is the only visual difference between them. */
  isBaseline?: boolean;
  /** Appended after the number, e.g. "estimated". */
  note?: string;
};

export function TokenCostBars({ bars }: { bars: TokenCostBar[] }) {
  const max = Math.max(...bars.map((bar) => bar.tokens), 1);

  return (
    <div className="flex flex-col gap-3">
      {bars.map((bar) => (
        <div key={bar.label} className="flex flex-col gap-1.5">
          <div className="flex items-baseline justify-between gap-3 text-sm">
            <span className="text-foreground">{bar.label}</span>
            <span className="shrink-0 font-mono tabular-nums text-foreground">
              {bar.tokens.toLocaleString()}
              {bar.note && <span className="ml-1.5 font-sans text-xs text-muted-foreground">{bar.note}</span>}
            </span>
          </div>
          <div className="h-2 overflow-hidden rounded-full bg-muted">
            <div
              className={cn("h-full rounded-full", bar.isBaseline ? "bg-muted-foreground/40" : "bg-primary")}
              style={{ width: `${Math.max((bar.tokens / max) * 100, 2)}%` }}
            />
          </div>
        </div>
      ))}
    </div>
  );
}
