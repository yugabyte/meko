// A map of things laid out by meaning, in plain SVG: a datapack's memories (each dot colored by its
// state, alike ones linked) or a search's nearest memories, knowledge-base chunks and conversation
// turns (colored by source, which the legend shows or hides). The lit ones (what a search returned)
// are bright with the rest faded. With a query, a star marks where it lands and lines run to the lit
// ones, and a dot hovered or focused shows its score above it.

import { useState } from "react";
import { SHOW_COLLECTIVE } from "../lib/features";
import type { MapPoint, MapSource, MemoryMap, MemoryState } from "../lib/types";
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

export const SOURCE_COLOR: Record<MapSource, string> = {
  memory: "text-info",
  knowledge_base: "text-success",
  conversation: "text-warning",
};

export const SOURCE_LABEL: Record<MapSource, string> = {
  memory: "Memory",
  knowledge_base: "Knowledge base",
  conversation: "Conversation",
};

// Only the points that pass keep, with the links between them renumbered.
function keep(m: MemoryMap, ok: (p: MapPoint) => boolean): MemoryMap {
  const at = new Map<number, number>();
  const points = m.points.filter((p, i) => ok(p) && (at.set(i, at.size), true));
  const links = m.links.flatMap((l) =>
    at.has(l.a) && at.has(l.b) ? [{ ...l, a: at.get(l.a)!, b: at.get(l.b)! }] : [],
  );
  return { ...m, points, links };
}

// Only the memories in states.
export const only = (m: MemoryMap, states: MemoryState[]) => keep(m, (p) => !!p.state && states.includes(p.state));

// The states drawn: collective memories only while the UI shows them.
export const SHOWN_STATES: MemoryState[] = SHOW_COLLECTIVE ? ["learning", "kept", "collective"] : ["learning", "kept"];

// A dot's color: by its source on a search map; by its state on a memory map, or one color while
// the UI shows memories as just memories.
const colorOf = (p: MapPoint) =>
  p.source ? SOURCE_COLOR[p.source] : SHOW_COLLECTIVE && p.state ? STATE_COLOR[p.state] : STATE_COLOR.learning;

export function MemoryMapView({
  map: whole,
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
  // A search map's sources, and the ones the legend has hidden.
  const sources = (Object.keys(SOURCE_LABEL) as MapSource[]).filter((s) => whole.points.some((p) => p.source === s));
  const [hidden, setHidden] = useState<Set<MapSource>>(new Set());
  const toggle = (s: MapSource) =>
    setHidden((h) => {
      const next = new Set(h);
      if (!next.delete(s)) next.add(s);
      return next;
    });
  const map = hidden.size ? keep(whole, (p) => !p.source || !hidden.has(p.source)) : whole;
  const dim = lit !== undefined && lit.size > 0;
  const on = (p: MapPoint) => !dim || lit.has(p.id);
  const r = mini ? 0.04 : 0.028;
  const picked = map.points.find((p) => p.id === selected);
  const shown = hover ?? picked;

  return (
    <div className="space-y-2">
      <svg
        viewBox="-1.12 -1.12 2.24 2.24"
        // A click paints nothing over the map: it doesn't select the drawing (which the browser fills
        // with its selection color), take focus or flash a tap highlight. Tab still focuses a dot.
        className={cx(
          "aspect-square w-full select-none rounded-lg border bg-card outline-none [-webkit-tap-highlight-color:transparent]",
          mini ? "max-w-56" : "max-w-[32rem]",
        )}
        onMouseDown={(e) => e.preventDefault()}
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
            className={cx(colorOf(p), "outline-none", onPick && "cursor-pointer")}
            opacity={on(p) ? 1 : 0.2}
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
        {picked && (
          // The picked dot's ring, sized in the map's own units: a stroke width in pixels
          // (non-scaling-stroke) isn't honored on every renderer, and 3 map units covers the whole map.
          <circle
            cx={picked.x}
            cy={picked.y}
            r={(dim && on(picked) ? r * 1.4 : r) + r * 0.6}
            fill="none"
            stroke="currentColor"
            strokeWidth={r * 0.35}
            className={colorOf(picked)}
            pointerEvents="none"
          />
        )}
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
          {sources.length > 0 ? (
            sources.map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => toggle(s)}
                aria-pressed={!hidden.has(s)}
                className={cx("flex items-center gap-1.5 hover:text-foreground", hidden.has(s) && "opacity-40")}
                title={hidden.has(s) ? "Show these" : "Hide these"}
              >
                <span className={cx("size-2 rounded-full bg-current", SOURCE_COLOR[s])} />
                {SOURCE_LABEL[s]}
              </button>
            ))
          ) : SHOW_COLLECTIVE ? (
            SHOWN_STATES.map((s) => (
              <span key={s} className="flex items-center gap-1.5">
                <span className={cx("size-2 rounded-full bg-current", STATE_COLOR[s])} />
                {STATE_LABEL[s]}
              </span>
            ))
          ) : (
            <span className="flex items-center gap-1.5">
              <span className={cx("size-2 rounded-full bg-current", STATE_COLOR.learning)} />
              Memory
            </span>
          )}
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
