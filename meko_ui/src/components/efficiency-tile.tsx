// efficiency-tile.tsx — presentation-only tile for the "Meko efficiency"
// dashboard (Token Burn today; Context Quality / Infra Efficiency later).
// Renders an icon + title, one big headline number, a subtitle, and a list of
// labelled horizontal bars. It holds no data logic: callers pass pre-computed
// bar values (0..100 fill) and pre-formatted value labels, plus an accent.

import type { ReactNode } from "react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

// Accent themes keep the tiles visually distinct while sharing one component.
// Each maps to the icon chip background and the bar fill.
export type EfficiencyTileAccent = "amber" | "violet" | "emerald";

const ACCENT_STYLES: Record<EfficiencyTileAccent, { chip: string; bar: string }> = {
  amber: { chip: "bg-amber-500/15 text-amber-500", bar: "bg-amber-500" },
  violet: { chip: "bg-violet-500/15 text-violet-400", bar: "bg-violet-400" },
  emerald: { chip: "bg-emerald-500/15 text-emerald-400", bar: "bg-emerald-400" },
};

export type EfficiencyTileBar = {
  /** Left-aligned label, e.g. "vs Opus". */
  label: string;
  /** Bar fill as a percentage of the track (0..100). */
  value: number;
  /** Right-aligned value text, e.g. "$149.60" or "72". */
  valueLabel: string;
};

export type EfficiencyTileProps = {
  icon?: ReactNode;
  title: string;
  /** Optional pill next to the title, e.g. "live". */
  badge?: string;
  /** Big headline number/phrase, e.g. "38% saved". */
  headline: string;
  /** One line under the headline, e.g. "vs all-frontier baseline". */
  subtitle?: string;
  bars?: EfficiencyTileBar[];
  accent?: EfficiencyTileAccent;
  /** Small muted note under the bars, e.g. a caveat or "shared with: 1 member". */
  footer?: ReactNode;
  loading?: boolean;
  error?: string | null;
  /** Shown when there is no data yet (e.g. zero requests). */
  emptyLabel?: string;
  isEmpty?: boolean;
  className?: string;
};

export function EfficiencyTile({
  icon,
  title,
  badge,
  headline,
  subtitle,
  bars = [],
  accent = "amber",
  footer,
  loading = false,
  error = null,
  emptyLabel = "No data yet",
  isEmpty = false,
  className,
}: EfficiencyTileProps) {
  const styles = ACCENT_STYLES[accent];

  return (
    <Card className={cn("flex flex-col gap-4 p-5", className)}>
      <div className="flex items-center gap-2">
        {icon ? (
          <span
            className={cn(
              "flex h-7 w-7 shrink-0 items-center justify-center rounded-md",
              styles.chip,
            )}
            aria-hidden
          >
            {icon}
          </span>
        ) : null}
        <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {title}
        </span>
        {badge ? (
          <span className="rounded-full border border-emerald-500/40 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-emerald-500">
            {badge}
          </span>
        ) : null}
      </div>

      {loading ? (
        <TileSkeleton />
      ) : error ? (
        <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
      ) : isEmpty ? (
        <p className="py-2 text-sm text-muted-foreground">{emptyLabel}</p>
      ) : (
        <>
          <div>
            <div className="text-3xl font-semibold leading-tight text-foreground">{headline}</div>
            {subtitle ? <p className="mt-1 text-sm text-muted-foreground">{subtitle}</p> : null}
          </div>

          {bars.length > 0 ? (
            <div className="flex flex-col gap-2.5">
              {bars.map((bar) => (
                <div key={bar.label} className="flex flex-col gap-1">
                  <div className="flex items-baseline justify-between text-xs">
                    <span className="text-muted-foreground">{bar.label}</span>
                    <span className="font-medium text-foreground">{bar.valueLabel}</span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-foreground/10">
                    <div
                      className={cn("h-full rounded-full", styles.bar)}
                      style={{ width: `${Math.max(0, Math.min(100, bar.value))}%` }}
                    />
                  </div>
                </div>
              ))}
            </div>
          ) : null}

          {footer ? <div className="mt-auto text-xs text-muted-foreground">{footer}</div> : null}
        </>
      )}
    </Card>
  );
}

function TileSkeleton() {
  return (
    <div className="flex flex-col gap-4">
      <div className="h-8 w-32 animate-pulse rounded bg-foreground/10" />
      <div className="flex flex-col gap-2.5">
        {[0, 1, 2].map((i) => (
          <div key={i} className="h-1.5 w-full animate-pulse rounded-full bg-foreground/10" />
        ))}
      </div>
    </div>
  );
}
