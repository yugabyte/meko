// A datapack's memories as a map, in plain SVG: each a dot colored by its state, alike memories
// close together and linked, and the lit ones (a query's matches, or what a search returned) bright
// with the rest faded. With a query, a star marks where it lands and lines run to its matches.

import { useState } from "react";
import type { MapPoint, MemoryMap, MemoryState } from "../lib/types";
import { cx } from "./ui";

export const STATE_COLOR: Record<MemoryState, string> = {
  learning: "text-info",
  kept: "text-success",
  collective: "text-primary",
};

export const STATE_LABEL: Record<MemoryState, string> = {
  learning: "Learning",
  kept: "Kept",
  collective: "Collective",
};

export function MemoryMapView({
  map,
  lit,
  selected,
  onPick,
  mini = false,
}: {
  map: MemoryMap;
  // The memories to light; none lit shows them all at full strength.
  lit?: Set<string>;
  selected?: string;
  onPick?: (p: MapPoint) => void;
  mini?: boolean;
}) {
  const [hover, setHover] = useState<MapPoint>();
  const dim = lit !== undefined && lit.size > 0;
  const on = (p: MapPoint) => !dim || lit.has(p.id);
  const r = mini ? 0.04 : 0.028;
  const shown = hover ?? map.points.find((p) => p.id === selected);

  return (
    <div className="space-y-2">
      <svg
        viewBox="-1.12 -1.12 2.24 2.24"
        className={cx("w-full rounded-lg border bg-card", mini ? "max-h-56" : "max-h-[32rem]")}
        role="img"
        aria-label="Memory map"
      >
        {map.links.map((l, i) => {
          const a = map.points[l.a];
          const b = map.points[l.b];
          return (
            <line
              key={i}
              x1={a.x}
              y1={a.y}
              x2={b.x}
              y2={b.y}
              className="stroke-border"
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
              opacity={on(a) && on(b) ? 0.9 : 0.25}
            />
          );
        })}
        {map.query &&
          map.points.filter((p) => dim && lit.has(p.id)).map((p) => (
            <line
              key={`q-${p.id}`}
              x1={map.query![0]}
              y1={map.query![1]}
              x2={p.x}
              y2={p.y}
              className="stroke-primary"
              strokeWidth={1 + 4 * Math.max(0, (p.similarity ?? 0) - 0.3)}
              vectorEffect="non-scaling-stroke"
              opacity={0.6}
            />
          ))}
        {map.points.map((p) => (
          <circle
            key={p.id}
            cx={p.x}
            cy={p.y}
            r={on(p) && dim ? r * 1.4 : r}
            fill="currentColor"
            className={cx(STATE_COLOR[p.state], onPick && "cursor-pointer")}
            opacity={on(p) ? 1 : 0.2}
            stroke={p.id === selected ? "currentColor" : undefined}
            strokeWidth={p.id === selected ? 3 : undefined}
            vectorEffect="non-scaling-stroke"
            onMouseEnter={() => setHover(p)}
            onMouseLeave={() => setHover(undefined)}
            onClick={() => onPick?.(p)}
          >
            <title>{p.text}</title>
          </circle>
        ))}
        {map.query && (
          <text
            x={map.query[0]}
            y={map.query[1]}
            textAnchor="middle"
            dominantBaseline="central"
            fontSize={mini ? 0.16 : 0.12}
            className="fill-primary"
          >
            ★
          </text>
        )}
      </svg>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {(Object.keys(STATE_LABEL) as MemoryState[]).map((s) => (
          <span key={s} className="flex items-center gap-1.5">
            <span className={cx("size-2 rounded-full bg-current", STATE_COLOR[s])} />
            {STATE_LABEL[s]}
          </span>
        ))}
        {map.query && <span className="text-primary">★ your question</span>}
      </div>
      {!mini && (
        <p className="min-h-10 text-sm text-muted-foreground">
          {shown ? <span className="text-foreground">{shown.text}</span> : "Hover over or tap a dot to read it."}
        </p>
      )}
    </div>
  );
}
