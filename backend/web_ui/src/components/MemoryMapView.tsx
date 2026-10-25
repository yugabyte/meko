// A datapack's memories as a map, in plain SVG: each a dot colored by its state, alike memories
// close together and linked, and the lit ones (a query's matches, or what a search returned) bright
// with the rest faded. With a query, a star marks where it lands and lines run to its matches, and a
// dot hovered or focused shows its score above it.

import { useState } from "react";
import { SHOW_COLLECTIVE } from "../lib/features";
import type { MapPoint, MemoryMap, MemoryState } from "../lib/types";
import { round2 } from "./Similarity";
import { cx } from "./ui";

export const STATE_COLOR: Record<MemoryState, string> = {
  learning: "text-info",
  kept: "text-success",
  collective: "text-primary",
};

export const STATE_LABEL: Record<MemoryState, string> = {
  learning: "Learning",
  kept: "Reviewed",
  collective: "Collective",
};

// Only the points in states, with the links between them renumbered.
export function only(m: MemoryMap, states: MemoryState[]): MemoryMap {
  const at = new Map<number, number>();
  const points = m.points.filter((p, i) => states.includes(p.state) && (at.set(i, at.size), true));
  const links = m.links.flatMap((l) =>
    at.has(l.a) && at.has(l.b) ? [{ ...l, a: at.get(l.a)!, b: at.get(l.b)! }] : [],
  );
  return { ...m, points, links };
}

// The states drawn: collective memories only while the UI shows them.
export const SHOWN_STATES: MemoryState[] = SHOW_COLLECTIVE ? ["learning", "kept", "collective"] : ["learning", "kept"];

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
        className={cx("aspect-square w-full rounded-lg border bg-card", mini ? "max-w-56" : "max-w-[32rem]")}
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
            tabIndex={onPick ? 0 : undefined}
            onMouseEnter={() => setHover(p)}
            onMouseLeave={() => setHover(undefined)}
            onFocus={() => setHover(p)}
            onBlur={() => setHover(undefined)}
            onClick={() => onPick?.(p)}
            onKeyDown={(e) => e.key === "Enter" && onPick?.(p)}
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
        {hover?.similarity !== undefined && <Score p={hover} r={r} />}
      </svg>
      {!mini && (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
          {SHOWN_STATES.map((s) => (
            <span key={s} className="flex items-center gap-1.5">
              <span className={cx("size-2 rounded-full bg-current", STATE_COLOR[s])} />
              {STATE_LABEL[s]}
            </span>
          ))}
          {map.query && <span className="text-primary">★ your question</span>}
        </div>
      )}
      {!mini && (
        <p className="min-h-10 text-sm text-muted-foreground">
          {shown ? <span className="text-foreground">{shown.text}</span> : "Hover over or tap a dot to read it."}
        </p>
      )}
    </div>
  );
}

// A dot's score to the question, in a small label above it (below it at the top edge), kept inside
// the map.
function Score({ p, r }: { p: MapPoint; r: number }) {
  const w = 0.26;
  const h = 0.11;
  const x = Math.min(Math.max(p.x, -1.1 + w / 2), 1.1 - w / 2);
  const above = p.y - r - 0.04 - h > -1.1;
  const y = above ? p.y - r - 0.04 - h : p.y + r + 0.04;
  return (
    <g pointerEvents="none">
      <rect x={x - w / 2} y={y} width={w} height={h} rx={0.025} className="fill-foreground" />
      <text
        x={x}
        y={y + h / 2}
        textAnchor="middle"
        dominantBaseline="central"
        fontSize={0.07}
        className="fill-background font-semibold"
      >
        {round2(p.similarity!).toFixed(2)}
      </text>
    </g>
  );
}
