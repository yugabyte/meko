// Inline trend line for daily query totals — no axes, no labels, sized to sit
// under a stat. Shared: any compact "is this going up or down" readout.

import { contextColorVar } from "@/lib/contextTones";

import { pointTotal, type ActivityPoint } from "./chartSeries";

export function MiniSparkline({ activity }: { activity: ActivityPoint[] }) {
  if (activity.length === 0) {
    return (
      <div className="flex h-8 w-full items-center justify-center rounded bg-muted/40">
        <span className="text-[10px] text-muted-foreground/40">No data</span>
      </div>
    );
  }

  const totals = activity.map(pointTotal);
  const max = Math.max(...totals, 1);
  const w = 100 / totals.length;

  const points = totals.map((v, i) => {
    const x = i * w + w / 2;
    const y = 32 - (v / max) * 28;
    return `${x},${y}`;
  });
  const polyline = points.join(" ");
  const areaPoints = ["0,32", ...points, "100,32"].join(" ");

  // A query mixes knowledge and memory, which is what the hybrid series is —
  // so the trend of all queries carries the same hue as that series.
  const stroke = contextColorVar("learning");
  const fill = contextColorVar("learning", 0.15);

  return (
    <svg viewBox="0 0 100 32" className="h-8 w-full" preserveAspectRatio="none">
      <polygon points={areaPoints} fill={fill} />
      <polyline
        points={polyline}
        fill="none"
        stroke={stroke}
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  );
}
