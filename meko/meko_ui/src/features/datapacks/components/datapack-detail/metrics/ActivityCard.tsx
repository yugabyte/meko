// "Context Queries" — daily retrieval activity, as a line or a stacked bar.
// The legend is driven by ACTIVITY_SERIES, so it can't name a colour the
// chart didn't draw.

import { useState } from "react";

import { cn } from "@/lib/utils";
import { CONTEXT_TONE } from "@/lib/contextTones";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { ActivityBarChart } from "@/components/ui/charts/ActivityBarChart";
import { ActivityLineChart } from "@/components/ui/charts/ActivityLineChart";
import { ACTIVITY_SERIES, type ActivityPoint } from "@/components/ui/charts/chartSeries";

type ChartType = "line" | "bar";

export function ActivityCard({
  activity,
  totalQueryCount,
  isLoading,
  title = "Query Activity",
}: {
  activity: ActivityPoint[];
  totalQueryCount: number;
  isLoading: boolean;
  title?: string;
}) {
  const [chartType, setChartType] = useState<ChartType>("bar");

  return (
    <div className="space-y-4 rounded-xl border border-border bg-card p-5">
      <div className="flex items-center justify-between">
        <div>
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            {title}
          </p>
          {!isLoading && (
            <p className="mt-0.5 text-muted-foreground">
              <span className="text-2xl font-bold tabular-nums text-foreground">
                {totalQueryCount.toLocaleString()}
              </span>
              <span className="ml-1 text-sm">queries</span>
            </p>
          )}
        </div>
        {/* ToggleGroup, not a pair of hand-rolled buttons: Radix gives the
         *  roving-focus keyboard nav and pressed state, and `size="xs"` is a
         *  28px target where the old 10px/py-0.5 pills were about 20. */}
        <ToggleGroup
          type="single"
          variant="segmented"
          size="xs"
          // Root defaults to gap-1; the segmented variant joins the items
          // itself, so the gap has to go or the hairline never meets.
          className="gap-0"
          value={chartType}
          // Radix reports "" when the pressed item is clicked again; a chart
          // always has to be drawn as something, so that is ignored.
          onValueChange={(next) => next && setChartType(next as ChartType)}
          aria-label="Chart type"
        >
          <ToggleGroupItem value="line">Line</ToggleGroupItem>
          <ToggleGroupItem value="bar">Bar</ToggleGroupItem>
        </ToggleGroup>
      </div>

      {isLoading ? (
        <div className="h-32 w-full animate-pulse rounded bg-muted" />
      ) : activity.length > 0 ? (
        <>
          {chartType === "line" ? (
            <ActivityLineChart activity={activity} tall />
          ) : (
            <ActivityBarChart activity={activity} tall />
          )}
          <div className="flex flex-wrap items-center gap-4">
            {ACTIVITY_SERIES.map((s) => (
              <div key={s.key} className="flex items-center gap-1.5">
                <div
                  className={cn("h-2 w-2 shrink-0 rounded-full", CONTEXT_TONE[s.concept].fill)}
                />
                <span className="text-[10px] text-muted-foreground">{s.label}</span>
              </div>
            ))}
          </div>
        </>
      ) : (
        <p className="text-xs text-muted-foreground">No activity recorded yet.</p>
      )}
    </div>
  );
}
