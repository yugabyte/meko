// Stacked bar chart of daily query activity, one bar per day on a day-offset
// x-axis, with a hover tooltip breaking the stack down by series.
// Shared: any surface plotting an ActivityPoint[] series.

import { useState } from "react";

import { ACTIVITY_SERIES, pointTotal, seriesColor, type ActivityPoint } from "./chartSeries";

/** Widest a single day's bar may be drawn, in viewBox units. See BAR_W. */
const MAX_BAR_W = 26;

export function ActivityBarChart({
  activity,
  tall,
}: {
  activity: ActivityPoint[];
  tall?: boolean;
}) {
  const [hovered, setHovered] = useState<ActivityPoint | null>(null);
  const [tipPct, setTipPct] = useState(0);

  const W = 360;
  const BPAD = 14;
  const H = (tall ? 160 : 80) + BPAD;
  const PAD = 4;

  const rawMax = Math.max(1, ...activity.map(pointTotal));
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const tickCount = yMax <= 10 ? 2 : yMax <= 50 ? 3 : 4;
  const yTicks = Array.from({ length: tickCount }, (_, i) =>
    Math.round((yMax / (tickCount - 1)) * i),
  );
  const axisFontSize = yMax <= 10 ? 11 : yMax <= 50 ? 9 : 8;
  const LPAD = yMax >= 1000 ? 36 : yMax >= 100 ? 30 : 26;

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  const maxDaysAgo = activity.length > 0 ? Math.max(...activity.map((d) => dayOffset(d.date))) : 30;
  const DAYS = Math.min(30, Math.max(maxDaysAgo + 1, 2));
  const oldestLabel = DAYS >= 30 ? "30d ago" : DAYS === 1 ? "Yesterday" : `${DAYS}d ago`;
  // Width is the window divided by its days, so bars naturally thin out as
  // history accumulates (~10 units at 30 days). Capped because the other end
  // is ugly: a datapack with one day of data gave that day HALF the plot, a
  // slab that read as a rendering fault rather than as a single day. The cap
  // is roughly the width a two-week window produces, so the chart looks the
  // same on day one as it does once it has filled in.
  const BAR_W = Math.min(MAX_BAR_W, Math.max(6, Math.floor((W - LPAD - PAD) / DAYS) - 1));

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD - BPAD;
  const barCx = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;
  const chartBottom = PAD + plotH;

  return (
    <div className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full text-foreground" style={{ height: H }}>
        {/* Y-axis gridlines + labels */}
        {yTicks.map((v) => {
          const gy = yAt(v);
          return (
            <g key={v}>
              <line
                x1={LPAD}
                y1={gy}
                x2={W - PAD}
                y2={gy}
                stroke="currentColor"
                strokeOpacity="0.1"
                strokeWidth="1"
              />
              <text
                x={LPAD - 4}
                y={gy + axisFontSize * 0.35}
                textAnchor="end"
                fontSize={axisFontSize}
                fill="currentColor"
                fillOpacity="0.5"
              >
                {v}
              </text>
            </g>
          );
        })}

        <text
          x={LPAD}
          y={H - 3}
          fontSize="9"
          fill="currentColor"
          fillOpacity="0.4"
          textAnchor="start"
        >
          {oldestLabel}
        </text>
        <text
          x={W - PAD}
          y={H - 3}
          fontSize="9"
          fill="currentColor"
          fillOpacity="0.4"
          textAnchor="end"
        >
          Today
        </text>

        {activity.map((d) => {
          const total = pointTotal(d);
          if (total === 0) return null;
          const ago = dayOffset(d.date);
          const cx = barCx(ago);
          const x = cx - BAR_W / 2;
          const fullH = (total / yMax) * plotH;
          const segs = ACTIVITY_SERIES.map((s) => ({
            value: d[s.key],
            color: seriesColor(s),
          })).filter((s) => s.value > 0);

          let yBottom = chartBottom;
          return (
            <g
              key={d.date}
              onMouseEnter={() => {
                setHovered(d);
                setTipPct(cx / W);
              }}
              onMouseLeave={() => setHovered(null)}
              style={{ cursor: "default" }}
            >
              <rect x={x - 3} y={PAD} width={BAR_W + 6} height={plotH} fill="transparent" />
              {segs.map((seg, i) => {
                const segH = (seg.value / total) * fullH;
                const ry = yBottom - segH;
                yBottom -= segH;
                return (
                  <rect
                    key={i}
                    x={x}
                    y={ry}
                    width={BAR_W}
                    height={segH}
                    fill={seg.color}
                    rx={i === segs.length - 1 ? 1.5 : 0}
                  />
                );
              })}
            </g>
          );
        })}
      </svg>

      {/* Hover tooltip */}
      {hovered && (
        <div
          className="absolute z-10 pointer-events-none rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{
            bottom: BPAD + 4,
            left: `${Math.min(80, Math.max(10, tipPct * 100))}%`,
            transform: "translateX(-50%)",
          }}
        >
          <p className="font-medium text-foreground mb-0.5">{hovered.date}</p>
          {ACTIVITY_SERIES.map((s) =>
            hovered[s.key] > 0 ? (
              <p key={s.key} className="text-muted-foreground">
                {s.label} <span className="text-foreground tabular-nums">{hovered[s.key]}</span>
              </p>
            ) : null,
          )}
        </div>
      )}
    </div>
  );
}
