// Half-circle gauge for a 0–1 ratio, with the percentage in the arc, a label
// below and an optional unit pill. Shared: any single-ratio readout.
//
// `color` is a CSS colour value rather than a class because SVG `stroke` takes
// no utility class — pass a token reference (`contextColorVar(...)`,
// `qualityColorVar(...)`), never a hex, so both modes track the theme.

import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";

export function ArcGauge({
  value,
  label,
  color,
  pill,
  tooltip,
}: {
  /** 0–1, or null when there is nothing to show yet. */
  value: number | null;
  label: string;
  color: string;
  pill?: string;
  tooltip?: string;
}) {
  const r = 32;
  const cx = 42;
  const cy = 42;
  const circumference = Math.PI * r;
  const pct = value !== null ? Math.min(1, Math.max(0, value)) : 0;
  const dashOffset = circumference * (1 - pct);

  return (
    // Fixed label/pill rows: three gauges side by side must agree on where
    // the arc sits and where the caption sits, whatever their text wraps to.
    <div className="flex flex-col items-center gap-1">
      <svg width="84" height="48" viewBox="0 0 84 48" className="text-foreground">
        <path
          d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
          fill="none"
          stroke="currentColor"
          strokeWidth="8"
          className="text-muted"
        />
        {value !== null && (
          <path
            d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
            fill="none"
            stroke={color}
            strokeWidth="8"
            strokeDasharray={circumference}
            strokeDashoffset={dashOffset}
            strokeLinecap="round"
          />
        )}
        <text
          x={cx}
          y={cy - 6}
          textAnchor="middle"
          fontSize="11"
          fontWeight="700"
          fill="currentColor"
        >
          {value !== null ? `${Math.round(value * 100)}%` : "—"}
        </text>
      </svg>
      <p className="flex min-h-[1.25rem] items-center gap-0.5 text-center text-[10px] text-muted-foreground">
        {label}
        {tooltip && (
          <TooltipProvider delayDuration={100}>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="cursor-help opacity-50 hover:opacity-100">ⓘ</span>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[240px] text-center leading-snug">
                {tooltip}
              </TooltipContent>
            </Tooltip>
          </TooltipProvider>
        )}
      </p>
      {/* Reserved even when absent, so a gauge with a pill and one without
       *  still line up along their bottom edge. */}
      <span className="flex min-h-[1.125rem] items-center rounded-full bg-muted px-1.5 text-[10px] font-semibold text-muted-foreground empty:bg-transparent">
        {pill ?? ""}
      </span>
    </div>
  );
}
