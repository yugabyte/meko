// A map of things laid out by meaning, in plain SVG: a datapack's memories (each dot colored by its
// state, alike ones linked) or a search's nearest memories, knowledge-base chunks and conversation
// turns (colored by source, which the legend shows or hides). The lit ones (what a search returned)
// are bright with the rest faded. With a query, a star marks where it lands and lines run to the lit
// ones, and a dot hovered or focused shows its score above it. 3D adds the third main direction as
// depth: drag to turn it, and it turns slowly on its own until touched.

import { useEffect, useMemo, useRef, useState } from "react";
import { SHOW_COLLECTIVE } from "../lib/features";
import type { MapPoint, MapSource, MemoryMap, MemoryState } from "../lib/types";
import { useSaved } from "../lib/useSavedView";
import { Segmented } from "./Segmented";
import { round2 } from "./Similarity";
import { cx } from "./ui";

type Depth = "2d" | "3d";
const DEPTHS: readonly { id: Depth; label: string }[] = [
  { id: "2d", label: "2D" },
  { id: "3d", label: "3D" },
];

// Where a point is drawn: its place on screen, and in 3D how near it is (s, 1 at the center's depth).
interface Spot {
  x: number;
  y: number;
  s: number;
}

const FLAT = (x: number, y: number): Spot => ({ x, y, s: 1 });

const MIN_DEPTH = 0.05;
// The scale the turned map is drawn at, and the eye's distance: with them, a dot's s runs from about
// NEAREST_S (front) to FARTHEST_S (back), and the nearest stays inside the map.
const K = 0.6;
const EYE = 4;
const NEAREST_S = EYE / (EYE - K * Math.SQRT2);
const FARTHEST_S = EYE / (EYE + K * Math.SQRT2);

// turn places points seen from yaw and pitch, with perspective. Depth is stretched to the map's own
// spread, since the third direction varies less than the first two and would draw nearly flat, but
// never from under MIN_DEPTH: with three points or fewer there's no third direction, and its z is
// rounding noise. Then the whole is shrunk so the nearest dots stay inside the map.
function turn(m: MemoryMap, yaw: number, pitch: number): { at: Spot[]; query?: Spot } {
  const zs = [...m.points.map((p) => p.z ?? 0), m.query_z ?? 0];
  const zMax = Math.max(MIN_DEPTH, ...zs.map(Math.abs));
  const [cy, sy, cp, sp] = [Math.cos(yaw), Math.sin(yaw), Math.cos(pitch), Math.sin(pitch)];
  const at = (x: number, y: number, z: number): Spot => {
    [x, y, z] = [x * K, y * K, (z / zMax) * K];
    const x1 = x * cy + z * sy;
    const z1 = -x * sy + z * cy;
    const y2 = y * cp - z1 * sp;
    const z2 = y * sp + z1 * cp;
    const s = EYE / (EYE - z2);
    return { x: x1 * s, y: y2 * s, s };
  };
  return {
    at: m.points.map((p) => at(p.x, p.y, p.z ?? 0)),
    query: m.query && at(m.query[0], m.query[1], m.query_z ?? 0),
  };
}

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
  const [depth, setDepth] = useSaved<Depth>("meko.map.depth", ["2d", "3d"], "2d");
  const three = depth === "3d" && !mini; // a mini map has no switch, so stays flat
  const [angle, setAngle] = useState({ yaw: 0.5, pitch: -0.3 });
  // It turns on its own until the map is touched, unless the reader asked for less motion.
  const [spinning, setSpinning] = useState(() => !window.matchMedia?.("(prefers-reduced-motion: reduce)").matches);
  const drag = useRef<{ x: number; y: number; moved: boolean } | undefined>(undefined);
  useEffect(() => {
    if (!three || !spinning) return;
    let frame = requestAnimationFrame(function step() {
      setAngle((a) => ({ ...a, yaw: a.yaw + 0.004 }));
      frame = requestAnimationFrame(step);
    });
    return () => cancelAnimationFrame(frame);
  }, [three, spinning]);
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
  const view = useMemo(
    () =>
      three
        ? turn(map, angle.yaw, angle.pitch)
        : { at: map.points.map((p) => FLAT(p.x, p.y)), query: map.query && FLAT(map.query[0], map.query[1]) },
    [map, three, angle],
  );
  // Far dots first, so near ones cover them.
  const order = map.points.map((_, i) => i);
  if (three) order.sort((a, b) => view.at[a].s - view.at[b].s);
  const spot = (p: MapPoint) => view.at[map.points.indexOf(p)];
  // Nearer dots are bolder: full at the front, two thirds at the center's depth, under half at the back.
  const fade = (i: number) =>
    three ? 0.4 + (0.6 * (view.at[i].s - FARTHEST_S)) / (NEAREST_S - FARTHEST_S) : 1;
  const pickedIndex = map.points.findIndex((p) => p.id === selected);
  const picked = pickedIndex >= 0 ? map.points[pickedIndex] : undefined;
  const shown = hover ?? picked;
  const pick = (p: MapPoint) => {
    if (!drag.current?.moved) onPick?.(p);
  };

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
        onPointerDown={(e) => {
          if (!three) return;
          setSpinning(false);
          drag.current = { x: e.clientX, y: e.clientY, moved: false };
        }}
        onPointerMove={(e) => {
          const d = drag.current;
          if (!three || !d || e.buttons === 0) return;
          const [dx, dy] = [e.clientX - d.x, e.clientY - d.y];
          if (Math.abs(dx) + Math.abs(dy) > 3) d.moved = true;
          d.x = e.clientX;
          d.y = e.clientY;
          setAngle((a) => ({
            yaw: a.yaw + dx * 0.01,
            pitch: Math.min(1.4, Math.max(-1.4, a.pitch - dy * 0.01)),
          }));
        }}
        onPointerUp={() => setTimeout(() => (drag.current = undefined))}
        role="img"
        aria-label="Memory map"
      >
        {map.links.map((l, i) => {
          const a = map.points[l.a];
          const b = map.points[l.b];
          const [sa, sb] = [view.at[l.a], view.at[l.b]];
          return (
            <line
              key={i}
              x1={sa.x}
              y1={sa.y}
              x2={sb.x}
              y2={sb.y}
              className="stroke-border"
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
              opacity={on(a) && on(b) ? 0.9 : 0.25}
            />
          );
        })}
        {view.query &&
          map.points.filter((p) => dim && lit.has(p.id)).map((p) => (
            <line
              key={`q-${p.id}`}
              x1={view.query!.x}
              y1={view.query!.y}
              x2={spot(p).x}
              y2={spot(p).y}
              className="stroke-primary"
              strokeWidth={1 + 4 * Math.max(0, (p.similarity ?? 0) - 0.3)}
              vectorEffect="non-scaling-stroke"
              opacity={0.6}
            />
          ))}
        {order.map((i) => {
          const p = map.points[i];
          const at = view.at[i];
          return (
            <circle
              key={p.id}
              cx={at.x}
              cy={at.y}
              r={(on(p) && dim ? r * 1.4 : r) * at.s}
              fill="currentColor"
              className={cx(colorOf(p), "outline-none", onPick && "cursor-pointer")}
              opacity={(on(p) ? 1 : 0.2) * fade(i)}
              tabIndex={onPick ? 0 : undefined}
              onMouseEnter={() => setHover(p)}
              onMouseLeave={() => setHover(undefined)}
              onFocus={() => setHover(p)}
              onBlur={() => setHover(undefined)}
              onClick={() => pick(p)}
              onKeyDown={(e) => e.key === "Enter" && onPick?.(p)}
            >
              <title>{p.text}</title>
            </circle>
          );
        })}
        {picked && (
          // The picked dot's ring, a circle of its own sized in map units so it keeps to its dot.
          <circle
            cx={view.at[pickedIndex].x}
            cy={view.at[pickedIndex].y}
            r={((dim && on(picked) ? r * 1.4 : r) + r * 0.6) * view.at[pickedIndex].s}
            fill="none"
            stroke="currentColor"
            strokeWidth={r * 0.35 * view.at[pickedIndex].s}
            className={colorOf(picked)}
            pointerEvents="none"
          />
        )}
        {view.query && (
          <text
            x={view.query.x}
            y={view.query.y}
            textAnchor="middle"
            dominantBaseline="central"
            fontSize={mini ? 0.16 : 0.12}
            className="fill-primary"
          >
            ★
          </text>
        )}
        {hover?.similarity !== undefined && spot(hover) && (
          <Score similarity={hover.similarity} at={spot(hover)} r={r} />
        )}
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
          <div className="ml-auto flex items-center gap-2">
            {three && <span>Drag to turn</span>}
            <Segmented size="sm" options={DEPTHS} value={depth} onChange={setDepth} />
          </div>
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
function Score({ similarity, at, r }: { similarity: number; at: Spot; r: number }) {
  const w = 0.26;
  const h = 0.11;
  r *= at.s;
  const x = Math.min(Math.max(at.x, -1.1 + w / 2), 1.1 - w / 2);
  const above = at.y - r - 0.04 - h > -1.1;
  const y = above ? at.y - r - 0.04 - h : at.y + r + 0.04;
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
        {round2(similarity).toFixed(2)}
      </text>
    </g>
  );
}
