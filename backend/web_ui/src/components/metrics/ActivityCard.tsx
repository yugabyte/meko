// Searches a day over the last 30, as bars stacked by kind or as a line of the total.

import { useState } from "react";
import { count } from "../../lib/format";
import type { DayActivity } from "../../lib/types";
import { Segmented } from "../Segmented";
import { Card } from "../ui";

const MODES = [
  { id: "line", label: "Line" },
  { id: "bar", label: "Bar" },
] as const;

const DAYS = 30;

const KINDS = [
  { key: "hybrid", label: "context_search", color: "bg-primary" },
  { key: "kb", label: "knowledgebase_search", color: "bg-success" },
  { key: "memory", label: "memory_search", color: "bg-info" },
] as const;

// The last 30 days, oldest first, with days that had no searches as zeros.
function lastDays(activity: DayActivity[]): DayActivity[] {
  const byDate = new Map(activity.map((d) => [d.date, d]));
  const out: DayActivity[] = [];
  for (let i = DAYS - 1; i >= 0; i--) {
    const date = new Date(Date.now() - i * 86_400_000).toISOString().slice(0, 10);
    out.push(byDate.get(date) ?? { date, hybrid: 0, kb: 0, memory: 0 });
  }
  return out;
}

export function ActivityCard({ activity, total }: { activity: DayActivity[]; total: number }) {
  const [mode, setMode] = useState<"bar" | "line">("bar");
  const days = lastDays(activity);
  const sum = (d: DayActivity) => d.hybrid + d.kb + d.memory;
  const peak = Math.max(1, ...days.map(sum));

  return (
    <Card className="p-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="text-xs font-semibold uppercase tracking-[0.12em] text-muted-foreground">Context queries</div>
          <div className="mt-1 text-2xl font-semibold tabular-nums">
            {count(total)} <span className="text-base font-normal text-muted-foreground">queries, last 30 days</span>
          </div>
        </div>
        <Segmented size="sm" options={MODES} value={mode} onChange={setMode} />
      </div>

      {total === 0 ? (
        <p className="py-8 text-sm text-muted-foreground">No activity recorded yet.</p>
      ) : (
        <>
          {mode === "bar" ? (
            <div className="mt-5 flex h-36 items-end gap-1">
              {days.map((d) => (
                <div
                  key={d.date}
                  title={`${d.date}: ${sum(d)} queries`}
                  className="flex h-full flex-1 flex-col-reverse overflow-hidden rounded-t"
                >
                  {KINDS.map((k) =>
                    d[k.key] > 0 ? <div key={k.key} className={k.color} style={{ height: `${(100 * d[k.key]) / peak}%` }} /> : null,
                  )}
                </div>
              ))}
            </div>
          ) : (
            <svg viewBox={`0 0 ${DAYS - 1} 100`} preserveAspectRatio="none" className="mt-5 h-36 w-full overflow-visible">
              <polyline
                fill="none"
                vectorEffect="non-scaling-stroke"
                className="stroke-primary"
                strokeWidth={2}
                points={days.map((d, i) => `${i},${100 - (100 * sum(d)) / peak}`).join(" ")}
              />
            </svg>
          )}
          <div className="mt-2 flex justify-between text-[11px] text-muted-foreground">
            <span>{days[0].date}</span>
            <span>today</span>
          </div>
          {mode === "bar" && (
            <div className="mt-3 flex flex-wrap gap-4 text-xs text-muted-foreground">
              {KINDS.map((k) => (
                <span key={k.key} className="flex items-center gap-1.5">
                  <span className={`size-2 rounded-full ${k.color}`} /> {k.label}
                </span>
              ))}
            </div>
          )}
        </>
      )}
    </Card>
  );
}
