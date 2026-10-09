// Three-band donut (high / mid / low) with the total in the middle.
// Shared: any surface breaking results into quality bands.
//
// The band colours come from the success/warning/destructive tokens. The
// previous implementation wrote `oklch(var(--color-emerald-400))`, which
// double-wraps a value Tailwind already stores as `oklch(...)` — the result
// is an invalid colour, so every arc silently failed to paint.

import { qualityColorVar } from "@/lib/contextTones";

export function DonutChart({
  high,
  mid,
  low,
  total,
}: {
  high: number;
  mid: number;
  low: number;
  total: number;
}) {
  const r = 30;
  const cx = 40;
  const cy = 40;
  const circumference = 2 * Math.PI * r;

  const segments = [
    { value: high, color: qualityColorVar("high") },
    { value: mid, color: qualityColorVar("mid") },
    { value: low, color: qualityColorVar("low") },
  ];

  let offset = 0;
  const arcs = segments.map((seg) => {
    const pct = total > 0 ? seg.value / total : 0;
    const dash = pct * circumference;
    const arc = { dash, offset, color: seg.color };
    offset += dash;
    return arc;
  });

  return (
    <div className="relative flex-shrink-0">
      <svg width="80" height="80" viewBox="0 0 80 80" className="text-foreground">
        <circle
          cx={cx}
          cy={cy}
          r={r}
          fill="none"
          stroke="currentColor"
          strokeWidth="10"
          className="text-muted"
        />
        {arcs.map((arc, i) => (
          <circle
            key={i}
            cx={cx}
            cy={cy}
            r={r}
            fill="none"
            stroke={arc.color}
            strokeWidth="10"
            strokeDasharray={`${arc.dash} ${circumference - arc.dash}`}
            strokeDashoffset={circumference / 4 - arc.offset}
            style={{ transform: "rotate(-90deg)", transformOrigin: `${cx}px ${cy}px` }}
          />
        ))}
        <text
          x={cx}
          y={cy + 4}
          textAnchor="middle"
          fontSize="12"
          fontWeight="700"
          fill="currentColor"
        >
          {total.toLocaleString()}
        </text>
      </svg>
    </div>
  );
}
