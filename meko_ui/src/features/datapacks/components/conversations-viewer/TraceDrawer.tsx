// Slide-over trace drawer: renders the windowed observation tree
// returned by GET .../messages/:message_id/trace as an indented tree
// + a small horizontal waterfall.
//
// Spans are linked by parent_observation_id. The repo guarantees one
// rooted sub-tree (the parent Conversation Add Message TOOL span);
// any orphans (parent referenced but not present in the windowed
// result set) are surfaced as additional roots so we never silently
// drop information.
//
// When the trace contains memory-related spans with output, a right
// panel is rendered alongside the tree to surface the extracted
// memory text so users can see exactly what was committed to memory.
//
// Click-to-close: ESC + backdrop click + the X button all dismiss.
// We don't trap focus aggressively — the parent thread page stays
// visible behind a translucent backdrop so users can still see
// where they are in the conversation.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertCircle,
  Bot,
  BrainCircuit,
  ChevronDown,
  ChevronRight,
  Clock,
  Cpu,
  Database,
  Loader2,
  Sparkles,
  Wrench,
  X,
  Zap,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  getDatapackMessageTrace,
  type DatapackTraceSpan,
} from "@/lib/mekoApi";
import { parseMemoryOutput } from "./parseMemoryOutput";

type Props = {
  open: boolean;
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  /** When this changes the drawer refetches. The drawer stays mounted
   *  even when closed so the slide animation works in both directions
   *  without a remount, but it skips the fetch until `open === true`. */
  messageId: string | null;
  onClose: () => void;
  /** When true, runs a shimmer sweep over the extracted-memories panel to draw
   *  the user's attention after opening via the memory badge. */
  shimmerMemories?: boolean;
  onShimmerMemoriesDone?: () => void;
};

export function TraceDrawer({
  open,
  datapackName,
  datapackId,
  conversationId,
  messageId,
  onClose,
  shimmerMemories,
  onShimmerMemoriesDone,
}: Props) {
  const [spans, setSpans] = useState<DatapackTraceSpan[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const [hoveredSpanId, setHoveredSpanId] = useState<string | null>(null);
  const [shimmering, setShimmering] = useState(false);

  // When the parent signals a shimmer, run the sweep then report done.
  useEffect(() => {
    if (!shimmerMemories) return;
    setShimmering(true);
    const t = setTimeout(() => {
      setShimmering(false);
      onShimmerMemoriesDone?.();
    }, 1600);
    return () => clearTimeout(t);
  }, [shimmerMemories, onShimmerMemoriesDone]);

  useEffect(() => {
    if (!open || !messageId) return;
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    setSpans([]);
    setCollapsed({});
    getDatapackMessageTrace(datapackName, conversationId, messageId, {
      signal: ac.signal,
      datapackId,
    })
      .then((resp) => {
        if (ac.signal.aborted) return;
        setSpans(resp.spans ?? []);
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        setError(getApiErrorMessage(err, "Failed to load trace."));
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [open, datapackName, conversationId, messageId]);

  // ESC closes.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  /** Build a parent → children index. */
  const tree = useMemo(() => buildTree(spans), [spans]);
  /** Total wall time = max end across all spans, used to scale the
   *  waterfall bars. Falls back to 1ms to avoid /0. */
  const totalMs = useMemo(() => {
    let max = 0;
    for (const s of spans) {
      const end = s.offset_ms + s.duration_ms;
      if (end > max) max = end;
    }
    return Math.max(1, max);
  }, [spans]);

  /** Top-level memory spans that have output. We exclude child spans
   *  whose parent is also a memory span to avoid showing duplicates
   *  (e.g. "Memory Add (Meko MCP)" and its child "Create a new memory
   *  in Mem0" carry the same output — we only show the parent). */
  const memorySpans = useMemo(() => {
    const memoryIds = new Set(
      spans
        .filter((s) => /memor/i.test(s.name) && s.output)
        .map((s) => s.id),
    );
    return spans.filter(
      (s) =>
        /memor/i.test(s.name) &&
        s.output &&
        (!s.parent_observation_id || !memoryIds.has(s.parent_observation_id)),
    );
  }, [spans]);
  const hasMemories = memorySpans.length > 0;

  const handleHoverSpan = useCallback(
    (id: string | null) => setHoveredSpanId(id),
    [],
  );

  return (
    <>
      <style>{`
        @keyframes memory-shimmer {
          0%   { transform: translateX(-100%); }
          100% { transform: translateX(100%); }
        }
      `}</style>

      {/* Backdrop. Click-through dismissal. */}
      <div
        className={cn(
          "fixed inset-0 z-40 bg-black/40 transition-opacity",
          open ? "opacity-100" : "pointer-events-none opacity-0",
        )}
        onClick={onClose}
        aria-hidden
      />

      {/* Drawer. Translates off-screen when closed; the parent keeps
          this mounted so the close animation runs. Widens when memory
          spans are present to accommodate the right-side panel. */}
      <aside
        className={cn(
          "fixed right-0 top-0 z-50 flex h-full w-full flex-col border-l border-border bg-[#0E1525] shadow-2xl transition-[transform,max-width] duration-200",
          hasMemories ? "max-w-[960px]" : "max-w-[640px]",
          open ? "translate-x-0" : "translate-x-full",
        )}
        aria-hidden={!open}
      >
        <header className="flex items-center justify-between gap-3 border-b border-border px-5 py-3 shrink-0">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <Zap className="h-4 w-4 text-purple-300" aria-hidden />
              <h3 className="text-sm font-medium text-white">Message trace</h3>
              {!loading && spans.length > 0 ? (
                <span className="text-[11px] text-muted-foreground">
                  {spans.length} spans · {formatDuration(totalMs)}
                </span>
              ) : null}
            </div>
            {messageId ? (
              <p
                className="mt-0.5 truncate font-mono text-[10px] text-muted-foreground"
                title={messageId}
              >
                {messageId}
              </p>
            ) : null}
          </div>
          <Button
            variant="ghost"
            size="sm"
            className="shrink-0 px-2 text-muted-foreground hover:text-foreground"
            onClick={onClose}
            aria-label="Close trace"
          >
            <X className="h-4 w-4" aria-hidden />
          </Button>
        </header>

        {/* Body: span tree (left) + optional extracted memories panel (right) */}
        <div className="flex flex-1 min-h-0 divide-x divide-border">
          {/* ── Left: span tree ── */}
          <div className="flex-1 min-w-0 overflow-y-auto px-5 py-4">
            {loading ? (
              <div className="flex items-center justify-center py-8 text-sm text-muted-foreground">
                <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
                Loading trace…
              </div>
            ) : error ? (
              <div className="flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">
                <AlertCircle className="h-4 w-4 shrink-0 mt-0.5" aria-hidden />
                <span>{error}</span>
              </div>
            ) : spans.length === 0 ? (
              <p className="text-center text-sm text-muted-foreground">
                No trace spans recorded for this message.
              </p>
            ) : (
              <ul className="space-y-1">
                {tree.roots.map((rootId) => (
                  <SpanTreeNode
                    key={rootId}
                    spanId={rootId}
                    depth={0}
                    byId={tree.byId}
                    childrenOf={tree.childrenOf}
                    totalMs={totalMs}
                    collapsed={collapsed}
                    onToggle={(id) =>
                      setCollapsed((prev) => ({ ...prev, [id]: !prev[id] }))
                    }
                    hoveredSpanId={hoveredSpanId}
                    onHoverSpan={handleHoverSpan}
                  />
                ))}
              </ul>
            )}
          </div>

          {/* ── Right: extracted memories ── shown only when present */}
          {hasMemories ? (
            <div className="relative w-[320px] shrink-0 overflow-hidden px-4 py-4">
              {/* Diagonal shimmer sweep: bottom-left → top-right */}
              {shimmering ? (
                <div
                  aria-hidden
                  className="pointer-events-none absolute inset-0 z-10"
                  style={{
                    animation: "memory-shimmer 1.6s ease-in-out forwards",
                    background:
                      "linear-gradient(105deg, transparent 30%, rgba(255,255,255,0.18) 50%, rgba(255,255,255,0.08) 58%, transparent 70%)",
                    width: "200%",
                    height: "100%",
                    top: "0",
                    left: "0",
                  }}
                />
              ) : null}
              <div className="overflow-y-auto h-full">
                <div className="mb-3 flex items-center gap-2">
                  <BrainCircuit className="h-4 w-4 text-blue-300" aria-hidden />
                  <h4 className="text-[13px] font-medium text-white">
                    Extracted memories
                  </h4>
                  <span className="ml-auto rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] text-blue-300">
                    {memorySpans.length}
                  </span>
                </div>
                <ul className="space-y-2">
                  {memorySpans.map((s) => (
                    <MemoryCard
                      key={s.id}
                      span={s}
                      highlighted={hoveredSpanId === s.id}
                      onHoverSpan={handleHoverSpan}
                    />
                  ))}
                </ul>
              </div>
            </div>
          ) : null}
        </div>
      </aside>
    </>
  );
}

// ---------------------------------------------------------------------------
// MemoryCard: renders one extracted memory from a memory-related span.
// The `output` blob can be a JSON string (various Mem0 response shapes)
// or plain text; we try to parse it and pull out human-readable strings.
// ---------------------------------------------------------------------------

type MemoryCardProps = {
  span: DatapackTraceSpan;
  highlighted: boolean;
  onHoverSpan: (id: string | null) => void;
};

function MemoryCard({ span, highlighted, onHoverSpan }: MemoryCardProps) {
  const memories = useMemo(
    () => parseMemoryOutput(span.output ?? ""),
    [span.output],
  );

  if (!memories.length) return null;

  return (
    <li
      className={cn(
        "rounded-lg border px-3 py-2.5 transition-colors duration-100 cursor-default",
        highlighted
          ? "border-blue-400/60 bg-blue-500/15 ring-1 ring-blue-400/30"
          : "border-blue-500/20 bg-blue-500/5 hover:border-blue-400/40 hover:bg-blue-500/10",
      )}
      onMouseEnter={() => onHoverSpan(span.id)}
      onMouseLeave={() => onHoverSpan(null)}
    >
      <div className="mb-1.5 flex items-center gap-1.5">
        <Sparkles className="h-3 w-3 shrink-0 text-blue-300" aria-hidden />
        <span className="truncate text-[11px] text-blue-200/70" title={span.name}>
          {span.name}
        </span>
        <span className="ml-auto shrink-0 tabular-nums text-[10px] text-muted-foreground">
          {formatDuration(span.duration_ms)}
        </span>
      </div>
      <ul className="space-y-1.5">
        {memories.map((text, i) => (
          <li
            // eslint-disable-next-line react/no-array-index-key
            key={i}
            className="text-[12.5px] leading-relaxed text-foreground/85"
          >
            {memories.length > 1 ? (
              <span className="mr-1 font-mono text-[10px] text-muted-foreground">
                {i + 1}.
              </span>
            ) : null}
            {text}
          </li>
        ))}
      </ul>
    </li>
  );
}

// ---------------------------------------------------------------------------
// One indented row in the tree. Renders the span name + type chip, the
// duration, and a horizontal waterfall bar showing where this span
// lives within the message window.
// ---------------------------------------------------------------------------

type NodeProps = {
  spanId: string;
  depth: number;
  byId: Record<string, DatapackTraceSpan>;
  childrenOf: Record<string, string[]>;
  totalMs: number;
  collapsed: Record<string, boolean>;
  onToggle: (id: string) => void;
  hoveredSpanId: string | null;
  onHoverSpan: (id: string | null) => void;
};

function SpanTreeNode({
  spanId,
  depth,
  byId,
  childrenOf,
  totalMs,
  collapsed,
  onToggle,
  hoveredSpanId,
  onHoverSpan,
}: NodeProps) {
  const span = byId[spanId];
  if (!span) return null;
  const kids = childrenOf[spanId] ?? [];
  const hasKids = kids.length > 0;
  const isCollapsed = !!collapsed[spanId];
  const isHovered = hoveredSpanId === spanId;
  const isMemorySpan = /memor/i.test(span.name);

  // Waterfall sizing — left offset and width are percentages of the
  // total message wall time.
  const leftPct = (span.offset_ms / totalMs) * 100;
  const widthPct = Math.max(0.5, (span.duration_ms / totalMs) * 100);

  const accent = spanAccent(span);

  return (
    <li>
      <div
        className={cn(
          "group flex items-center gap-2 rounded-md px-1.5 py-1 transition-colors duration-100",
          isHovered && isMemorySpan
            ? "bg-blue-500/15 ring-1 ring-inset ring-blue-400/30"
            : "hover:bg-[#141C2E]",
        )}
        style={{ paddingLeft: `${depth * 14 + 6}px` }}
        onMouseEnter={() => isMemorySpan && onHoverSpan(spanId)}
        onMouseLeave={() => isMemorySpan && onHoverSpan(null)}
      >
        <button
          type="button"
          onClick={() => hasKids && onToggle(spanId)}
          className={cn(
            "flex h-4 w-4 shrink-0 items-center justify-center rounded text-muted-foreground",
            hasKids ? "hover:text-foreground" : "opacity-0",
          )}
          aria-label={isCollapsed ? "Expand" : "Collapse"}
          tabIndex={hasKids ? 0 : -1}
        >
          {isCollapsed ? (
            <ChevronRight className="h-3 w-3" aria-hidden />
          ) : (
            <ChevronDown className="h-3 w-3" aria-hidden />
          )}
        </button>

        <span className={cn("inline-flex h-4 w-4 shrink-0 items-center justify-center", accent.iconColor)}>
          {accent.icon}
        </span>

        <span className="min-w-0 flex-1 truncate text-[13px] text-foreground/90" title={span.name}>
          {span.name}
        </span>

        {span.model ? (
          <code className="hidden truncate font-mono text-[10px] text-muted-foreground sm:inline">
            {span.model}
          </code>
        ) : null}

        <span className="shrink-0 tabular-nums text-[11px] text-muted-foreground">
          {formatDuration(span.duration_ms)}
        </span>
      </div>

      {/* Waterfall bar: renders directly under the row, indented to
          match. Uses left/width % so resizing the drawer reflows
          smoothly. */}
      <div
        className="relative mb-1 h-1 rounded-full bg-[#11192A]"
        style={{ marginLeft: `${depth * 14 + 32}px`, marginRight: "12px" }}
      >
        <div
          className={cn("absolute h-1 rounded-full", accent.barColor)}
          style={{ left: `${leftPct}%`, width: `${widthPct}%` }}
          title={`${formatDuration(span.offset_ms)} offset · ${formatDuration(span.duration_ms)} duration`}
        />
      </div>

      {hasKids && !isCollapsed ? (
        <ul>
          {kids.map((childId) => (
            <SpanTreeNode
              key={childId}
              spanId={childId}
              depth={depth + 1}
              byId={byId}
              childrenOf={childrenOf}
              totalMs={totalMs}
              collapsed={collapsed}
              onToggle={onToggle}
              hoveredSpanId={hoveredSpanId}
              onHoverSpan={onHoverSpan}
            />
          ))}
        </ul>
      ) : null}
    </li>
  );
}

// ---------------------------------------------------------------------------
// Tree builder: turn a flat list of spans into a parent→children index.
//
// Roots are spans whose parent_observation_id is empty OR whose parent
// isn't present in the result set (windowed query may legitimately
// drop the parent). Within a parent, children are ordered by offset
// then duration so the longer sibling sorts first when offsets tie.
// ---------------------------------------------------------------------------

function buildTree(spans: DatapackTraceSpan[]): {
  byId: Record<string, DatapackTraceSpan>;
  childrenOf: Record<string, string[]>;
  roots: string[];
} {
  const byId: Record<string, DatapackTraceSpan> = {};
  for (const s of spans) byId[s.id] = s;

  const childrenOf: Record<string, string[]> = {};
  const roots: string[] = [];
  for (const s of spans) {
    const parent = s.parent_observation_id;
    if (parent && byId[parent]) {
      (childrenOf[parent] = childrenOf[parent] ?? []).push(s.id);
    } else {
      roots.push(s.id);
    }
  }

  const sortByOffset = (a: string, b: string) => {
    const sa = byId[a];
    const sb = byId[b];
    if (sa.offset_ms !== sb.offset_ms) return sa.offset_ms - sb.offset_ms;
    return sb.duration_ms - sa.duration_ms;
  };
  for (const k of Object.keys(childrenOf)) childrenOf[k].sort(sortByOffset);
  roots.sort(sortByOffset);

  return { byId, childrenOf, roots };
}

// ---------------------------------------------------------------------------
// Visual mapping per span type. Picks an icon + color so the tree is
// scannable: tools = wrench/emerald, generations = cpu/amber, etc.
// ---------------------------------------------------------------------------

type SpanAccent = {
  icon: React.ReactNode;
  iconColor: string;
  barColor: string;
};

function spanAccent(span: DatapackTraceSpan): SpanAccent {
  const t = (span.type || "").toUpperCase();
  if (t === "GENERATION" || t === "LLM") {
    return {
      icon: <Cpu className="h-3 w-3" aria-hidden />,
      iconColor: "text-amber-300",
      barColor: "bg-amber-400/70",
    };
  }
  if (t === "TOOL") {
    // Distinguish memory tools so the common "memory.search" / "memory.add"
    // pattern lights up blue at a glance.
    if (/memor/i.test(span.name)) {
      return {
        icon: <Sparkles className="h-3 w-3" aria-hidden />,
        iconColor: "text-blue-300",
        barColor: "bg-blue-400/70",
      };
    }
    return {
      icon: <Wrench className="h-3 w-3" aria-hidden />,
      iconColor: "text-emerald-300",
      barColor: "bg-emerald-400/70",
    };
  }
  if (t === "RETRIEVAL" || t === "EMBEDDING") {
    return {
      icon: <Database className="h-3 w-3" aria-hidden />,
      iconColor: "text-cyan-300",
      barColor: "bg-cyan-400/70",
    };
  }
  if (t === "AGENT" || t === "SPAN") {
    return {
      icon: <Bot className="h-3 w-3" aria-hidden />,
      iconColor: "text-purple-300",
      barColor: "bg-purple-400/70",
    };
  }
  return {
    icon: <Clock className="h-3 w-3" aria-hidden />,
    iconColor: "text-foreground/60",
    barColor: "bg-foreground/40",
  };
}

function formatDuration(ms: number): string {
  if (ms < 0) return "INVALID";
  if (ms < 1) return `${ms}ms`;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`;
  const mins = Math.floor(ms / 60_000);
  const secs = Math.round((ms % 60_000) / 1000);
  const hours = Math.floor(mins / 60);
  const remainingMins = mins % 60;
  if (hours > 0) return `${hours}h ${remainingMins}m ${secs}s`;
  if (remainingMins > 0) return `${remainingMins}m ${secs}s`;
  return `${secs}s`;
}
