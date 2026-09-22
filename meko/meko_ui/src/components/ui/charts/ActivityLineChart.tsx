// Multi-series line chart of daily query activity, on a day-offset x-axis so
// gaps in the data read as gaps in time rather than as evenly spaced points.
// Shared: any surface plotting an ActivityPoint[] series.

import { ACTIVITY_SERIES, seriesColor, type ActivityPoint } from "./chartSeries";

export function ActivityLineChart({
  activity,
  tall,
}: {
  activity: ActivityPoint[];
  tall?: boolean;
}) {
  const W = 360;
  const BPAD = 14;
  const H = (tall ? 160 : 80) + BPAD;
  const PAD = 4;

  const rawMax = Math.max(
    1,
    ...activity.flatMap((d) => ACTIVITY_SERIES.map((s) => d[s.key])),
  );
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const tickCount = yMax <= 10 ? 2 : yMax <= 50 ? 3 : 4;
  const yTicks = Array.from({ length: tickCount }, (_, i) => Math.round((yMax / (tickCount - 1)) * i));
  const axisFontSize = yMax <= 10 ? 11 : yMax <= 50 ? 9 : 8;
  const LPAD = yMax >= 1000 ? 36 : yMax >= 100 ? 30 : 26;

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  // Span from oldest data point; cap at 30 days; add 1-day padding on each side
  const maxDaysAgo = activity.length > 0 ? Math.max(...activity.map((d) => dayOffset(d.date))) : 30;
  const DAYS = Math.min(30, Math.max(maxDaysAgo + 1, 2));
  const oldestLabel = DAYS >= 30 ? "30d ago" : DAYS === 1 ? "Yesterday" : `${DAYS}d ago`;

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD - BPAD;
  const xAt = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;

  const isSingle = activity.length === 1;

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full text-foreground" style={{ height: H }}>
      {yTicks.map((v) => {
        const gy = yAt(v);
        return (
          <g key={v}>
            <line x1={LPAD} y1={gy} x2={W - PAD} y2={gy}
              stroke="currentColor" strokeOpacity="0.1" strokeWidth="1" />
            <text x={LPAD - 4} y={gy + axisFontSize * 0.35} textAnchor="end"
              fontSize={axisFontSize} fill="currentColor" fillOpacity="0.5">{v}</text>
          </g>
        );
      })}

      <text x={LPAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="start">{oldestLabel}</text>
      <text x={W - PAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="end">Today</text>

      {ACTIVITY_SERIES.map((s) => {
        const color = seriesColor(s);
        if (isSingle) {
          const d = activity[0];
          const v = d[s.key];
          if (v === 0) return null;
          return (
            <circle key={s.key} cx={xAt(dayOffset(d.date))} cy={yAt(v)}
              r="4" fill={color} />
          );
        }
        const pts = activity
          .map((d) => `${xAt(dayOffset(d.date)).toFixed(1)},${yAt(d[s.key]).toFixed(1)}`)
          .join(" ");
        return (
          <g key={s.key}>
            <polyline points={pts} fill="none" stroke={color}
              strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
            {activity.map((d) => {
              const v = d[s.key];
              if (v === 0) return null;
              return (
                <circle key={d.date} cx={xAt(dayOffset(d.date))} cy={yAt(v)}
                  r="3" fill={color} />
              );
            })}
          </g>
        );
      })}
    </svg>
  );
}
