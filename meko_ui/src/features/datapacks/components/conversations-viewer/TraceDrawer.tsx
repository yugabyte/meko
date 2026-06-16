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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  AlertCircle,
  BookUp,
  Bot,
  Brain,
  BrainCircuit,
  SquareArrowOutUpRight,
  ChevronDown,
  ChevronRight,
  Clock,
  Code2,
  Cpu,
  Database,
  ExternalLink,
  Loader2,
  Wrench,
  X,
  Zap,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { HoverInfoBadge } from "@/components/ui/hover-info-badge";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  getDatapackMessageTrace,
  type DatapackTraceSpan,
} from "@/lib/api/mekoApi";
import { Link } from "react-router-dom";
import { ROUTES, datapackKnowledgePath } from "@/app/routes/paths";
import { parseMemoryOutput } from "./parseMemoryOutput";
import { beautifyStatement } from "./sqlBeautify";
import { MemoryIcon } from "@/components/conversation-telemetry";

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
  /** mem0 row id from ?memoryId= deep-link — highlights the matching card. */
  highlightMemoryId?: string;
  /** Langfuse observation id of a span to focus when the drawer opens
   *  (set when "View Trace" is clicked on a Decision Trace step). The
   *  drawer expands the span's ancestors, scrolls it into view, and
   *  flashes it so the user lands on the query/hits for that step. */
  focusSpanId?: string;
};

/** Which side of the drawer triggered the current hover.
 *  `"tree"` → cursor is on a span row (left); auto-scroll the memory card list.
 *  `"memory"` → cursor is on a memory card (right); auto-scroll the span tree.
 *  We never auto-scroll the side the cursor is already on — that would
 *  shift another row under the pointer and start a runaway hover loop.
 */
type HoverSource = "tree" | "memory" | null;

export function TraceDrawer({
  open,
  datapackName,
  datapackId,
  conversationId,
  messageId,
  onClose,
  shimmerMemories,
  onShimmerMemoriesDone,
  highlightMemoryId,
  focusSpanId,
}: Props) {
  const [spans, setSpans] = useState<DatapackTraceSpan[]>([]);
  // `langfuse_project_id` rides along on the same trace response — the
  // drawer pulls it itself so callers don't have to plumb it through.
  // Drives the "View Detailed Trace" deep-link in the footer.
  const [langfuseProjectId, setLangfuseProjectId] = useState<string>("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  // Tracks which span is hovered AND which side the cursor is on.
  // We use the source to only auto-scroll the *opposite* side; scrolling
  // the side that has the cursor would slide a different row under the
  // pointer and trigger a runaway hover/scroll loop.
  const [hover, setHover] = useState<{
    id: string | null;
    source: HoverSource;
  }>({ id: null, source: null });
  const hoveredSpanId = hover.id;
  const hoverSource = hover.source;
  const [shimmering, setShimmering] = useState(false);
  // Tracks the message whose memories panel we've already swept, so the
  // sweep fires only ONCE per trace. The "view trace" affordance on a
  // memory bubble always asks for a shimmer, so without this guard the
  // panel re-sweeps every time the same trace is reopened.
  const shimmeredMessageIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (!open || !messageId) return;
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    setSpans([]);
    setLangfuseProjectId("");
    getDatapackMessageTrace(datapackName, conversationId, messageId, {
      signal: ac.signal,
      datapackId,
    })
      .then((resp) => {
        if (ac.signal.aborted) return;
        setSpans(resp.spans ?? []);
        setLangfuseProjectId(resp.langfuse_project_id ?? "");
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

  /** Shallow traces (at most one nested level under any root) start with every
   *  parent collapsed so the drawer stays compact; deeper trees default to expanded. */
  useEffect(() => {
    if (!spans.length) {
      setCollapsed({});
      return;
    }
    const { childrenOf, roots } = buildTree(spans);
    setCollapsed(defaultCollapsedForShallowTree(childrenOf, roots));
  }, [spans]);

  // Focus a specific span (from "View Trace" on a Decision Trace step):
  // expand its ancestors so it's visible, then flash it. Declared AFTER
  // the default-collapse effect so its ancestor-expansion wins (both run
  // on `spans` change; React fires effects top-to-bottom).
  const [flashSpanId, setFlashSpanId] = useState<string | null>(null);
  useEffect(() => {
    if (!open || !focusSpanId || spans.length === 0) return;
    const byId = Object.fromEntries(spans.map((s) => [s.id, s]));
    if (!byId[focusSpanId]) return; // step span not in this message's window
    setCollapsed((prev) => {
      const next = { ...prev };
      let parentId = byId[focusSpanId]?.parent_observation_id;
      while (parentId && byId[parentId]) {
        next[parentId] = false;
        parentId = byId[parentId].parent_observation_id;
      }
      return next;
    });
    setFlashSpanId(focusSpanId);
    const t = window.setTimeout(() => setFlashSpanId(null), 2800);
    return () => window.clearTimeout(t);
  }, [open, focusSpanId, spans]);

  /** Top-level memory spans that have output.
   *
   * Rules:
   * 1) Exclude child spans whose parent is also memory-related
   *    (e.g. parent "Memory Add" + child "Create a new memory in Mem0").
   * 2) Exclude later spans when their parsed memory text is already shown
   *    by an earlier span (common for "Updating Existing Memories").
   */
  const memorySpans = useMemo(() => {
    const memoryIds = new Set(
      spans.filter((s) => /memor/i.test(s.name) && s.output).map((s) => s.id),
    );
    const topLevelMemorySpans = spans.filter(
      (s) =>
        /memor/i.test(s.name) &&
        s.output &&
        (!s.parent_observation_id || !memoryIds.has(s.parent_observation_id)),
    );

    const seenMemoryTexts = new Set<string>();
    const latestFirst: DatapackTraceSpan[] = [];
    // Walk from newest -> oldest so duplicate memory text keeps the latest span.
    for (let i = topLevelMemorySpans.length - 1; i >= 0; i -= 1) {
      const span = topLevelMemorySpans[i];
      const parsed = parseMemoryOutput(span.output ?? "");
      const normalizedTexts = parsed.map(normalizeMemoryTextForDedupe).filter(Boolean);

      // If this span contributes only memory text already represented by a newer
      // span, skip it so we show the latest memory state.
      if (normalizedTexts.length > 0 && normalizedTexts.every((t) => seenMemoryTexts.has(t))) {
        continue;
      }

      normalizedTexts.forEach((t) => seenMemoryTexts.add(t));
      latestFirst.push(span);
    }
    // Restore chronological ordering for rendering.
    return latestFirst.reverse();
  }, [spans]);
  /**
   * Flatten memory span outputs into one renderable memory item each.
   * This keeps the right panel at one-card-per-memory instead of
   * grouping multiple extracted memories into a single card.
   */
  const memoryItems = useMemo(
    () =>
      memorySpans.flatMap((span) =>
        // Prefer backend-expanded memory items (exact counts from DB/graph).
        // Fallback to parsed output when older API responses don't include them.
        span.memory_items && span.memory_items.length > 0
          ? span.memory_items.map((item, index) => ({
              id: `${span.id}:${index}:${item.memory_id ?? "na"}`,
              span,
              memoryId: item.memory_id,
              text: item.text,
              promoted: item.promoted ?? false,
              entityNames: item.entity_names ?? [],
              relationNames: item.relation_names ?? [],
              signals: {
                vector: item.vector ?? 0,
                entities: item.entities ?? 0,
                relations: item.relations ?? 0,
              },
            }))
          : parseMemoryOutput(span.output ?? "").map((text, index) => ({
              id: `${span.id}:${index}`,
              span,
              memoryId: undefined,
              text,
              promoted: false,
              entityNames: [],
              relationNames: [],
              signals: { vector: 0, entities: 0, relations: 0 },
            })),
      ),
    [memorySpans],
  );
  const hasMemories = memoryItems.length > 0;
  const normalMemoryItems = memoryItems.filter((item) => !item.promoted);
  const promotedMemoryItems = memoryItems.filter((item) => item.promoted);

  // When the parent signals a shimmer, run the sweep then report done — but
  // only the first time for a given message (reopening the same trace just
  // acknowledges the request). The sweep is gated on `hasMemories` so it
  // doesn't start until the memories panel is actually mounted: with a
  // `?memoryId=` deep-link the trace can finish loading after the click, and
  // starting the fixed-duration sweep before the panel exists would let the
  // animation elapse off-screen (the bug where no white shimmer showed).
  useEffect(() => {
    if (!shimmerMemories) return;
    if (shimmeredMessageIdRef.current === messageId) {
      onShimmerMemoriesDone?.();
      return;
    }
    if (!hasMemories) return;
    shimmeredMessageIdRef.current = messageId ?? null;
    setShimmering(true);
    const t = setTimeout(() => {
      setShimmering(false);
      onShimmerMemoriesDone?.();
    }, 1600);
    return () => clearTimeout(t);
  }, [shimmerMemories, onShimmerMemoriesDone, messageId, hasMemories]);

  // "View execution trace" (a step's span focus) flashes ONLY the span row
  // in the left tree — it must not flash the memory cards on the right.
  // Memory-card flashing is reserved for the memory-targeted entry points
  // (the `?memoryId=` deep-link and the memory bubble's "View Trace", which
  // drive `flashMemoryId` / the panel shimmer).
  const treeFlashSpanId = flashSpanId;

  // Deep-link (`?memoryId=`) memory flash — fire ONCE per target.
  //
  // `highlightMemoryId` is set by the parent ONLY for the deep-link reveal;
  // manual opens (badge click, "View Trace") pass it as undefined so they
  // never flash a card. We convert it into a transient `flashMemoryId` set
  // once (guarded by a ref keyed on the id) and cleared after the animation.
  // When it goes falsy (a manual open after a deep-link) we cancel any
  // in-progress flash so a manual click is strictly the white sweep.
  const [flashMemoryId, setFlashMemoryId] = useState<string | null>(null);
  const flashedMemoryIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (!highlightMemoryId) {
      setFlashMemoryId(null);
      flashedMemoryIdRef.current = null;
      return;
    }
    if (!open || memoryItems.length === 0) return;
    if (flashedMemoryIdRef.current === highlightMemoryId) return;
    if (!memoryItems.some((it) => it.memoryId === highlightMemoryId)) return;
    flashedMemoryIdRef.current = highlightMemoryId;
    setFlashMemoryId(highlightMemoryId);
    const t = window.setTimeout(() => setFlashMemoryId(null), 2800);
    return () => window.clearTimeout(t);
  }, [open, highlightMemoryId, memoryItems]);

  const handleHoverSpan = useCallback(
    (id: string | null, source: HoverSource) =>
      setHover({ id, source: id === null ? null : source }),
    [],
  );
  const activePathSpanIds = useMemo(
    () => collectPathSpanIDs(hoveredSpanId, tree.byId, tree.childrenOf),
    [hoveredSpanId, tree.byId, tree.childrenOf],
  );

  return (
    <>
      <style>{`
        @keyframes memory-shimmer {
          0%   { transform: translateX(-100%); }
          100% { transform: translateX(100%); }
        }
        /* Diagonal sweep over the memories panel. White reads on the dark
           drawer but vanishes on a light background, so light mode uses a
           visible blue tint (matching the memories accent) instead. */
        .meko-memory-shimmer {
          animation: memory-shimmer 1.6s ease-in-out forwards;
          background: linear-gradient(105deg, transparent 30%, rgba(37,99,235,0.22) 50%, rgba(37,99,235,0.10) 58%, transparent 70%);
        }
        .dark .meko-memory-shimmer {
          background: linear-gradient(105deg, transparent 30%, rgba(255,255,255,0.18) 50%, rgba(255,255,255,0.08) 58%, transparent 70%);
        }
        @keyframes meko-memory-flash {
          0%, 100% {
            border-color: rgb(251 191 36 / 0.75);
            background-color: rgb(245 158 11 / 0.18);
            box-shadow: 0 0 0 1px rgb(251 191 36 / 0.55), 0 0 10px 0 rgb(251 191 36 / 0.25);
          }
          50% {
            border-color: rgb(251 191 36 / 0.95);
            background-color: rgb(245 158 11 / 0.3);
            box-shadow: 0 0 0 2px rgb(251 191 36 / 0.85), 0 0 14px 0 rgb(251 191 36 / 0.4);
          }
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
          "fixed right-0 top-0 z-50 flex h-full w-full flex-col border-l border-border bg-card shadow-2xl transition-[transform,max-width] duration-200",
          hasMemories ? "max-w-[960px]" : "max-w-[640px]",
          open ? "translate-x-0" : "translate-x-full",
        )}
        aria-hidden={!open}
      >
        <header className="flex items-center justify-between gap-3 border-b border-border px-5 py-3 shrink-0">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <Zap className="h-4 w-4 text-purple-600 dark:text-purple-300" aria-hidden />
              <h3 className="text-sm font-medium text-foreground">Message trace</h3>
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
              <div className="flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-700 dark:text-red-300">
                <AlertCircle className="h-4 w-4 shrink-0 mt-0.5" aria-hidden />
                <span>{error}</span>
              </div>
            ) : spans.length === 0 ? (
              <p className="text-center text-sm text-muted-foreground">
                No trace spans recorded for this message.
              </p>
            ) : (
              <>
                <TimeAxis totalMs={totalMs} />
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
                      onToggle={(id) => setCollapsed((prev) => ({ ...prev, [id]: !prev[id] }))}
                      hoveredSpanId={hoveredSpanId}
                      hoverSource={hoverSource}
                      activePathSpanIds={activePathSpanIds}
                      onHoverSpan={handleHoverSpan}
                      flashSpanId={treeFlashSpanId}
                    />
                  ))}
                </ul>
              </>
            )}
          </div>

          {/* Right: extracted memories — shown only when present */}
          {hasMemories ? (
            <div className="relative w-[320px] shrink-0 overflow-hidden py-4">
              {/* Diagonal shimmer sweep: bottom-left → top-right */}
              {shimmering ? (
                <div
                  aria-hidden
                  className="meko-memory-shimmer pointer-events-none absolute inset-0 z-10"
                  style={{
                    width: "200%",
                    height: "100%",
                    top: "0",
                    left: "0",
                  }}
                />
              ) : null}
              {/* Horizontal padding lives on this scroll container (not the
                  outer panel) so the targeted card's amber glow has room
                  inside the clip box instead of being cut at the edges. */}
              <div className="overflow-y-auto h-full px-4">
                <div className="mb-3 flex items-center gap-2">
                  <BrainCircuit
                    className="h-4 w-4 shrink-0 text-blue-600 dark:text-blue-300"
                    aria-hidden
                  />
                  <h4 className="text-[13px] font-medium text-foreground">Extracted memories</h4>
                  <span className="ml-auto shrink-0 rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] text-blue-700 dark:text-blue-300">
                    {memoryItems.length}
                  </span>
                </div>
                <ul className="space-y-2">
                  {normalMemoryItems.map((item, idx) => (
                    <MemoryCard
                      key={item.id}
                      span={item.span}
                      memoryId={item.memoryId}
                      memoryText={item.text}
                      datapackName={datapackName}
                      datapackId={datapackId}
                      promoted={item.promoted}
                      entityNames={item.entityNames}
                      relationNames={item.relationNames}
                      signals={item.signals}
                      ordinal={idx + 1}
                      highlighted={hoveredSpanId === item.span.id}
                      targeted={!!flashMemoryId && item.memoryId === flashMemoryId}
                      hoverSource={hoverSource}
                      onHoverSpan={handleHoverSpan}
                    />
                  ))}
                </ul>

                {promotedMemoryItems.length > 0 ? (
                  <>
                    <div className="mb-2 mt-5 flex items-center gap-2">
                      <BookUp
                        className="h-3.5 w-3.5 shrink-0 text-emerald-600 dark:text-emerald-300"
                        aria-hidden
                      />
                      <h4 className="text-[13px] font-medium text-emerald-700 dark:text-emerald-300">
                        Promoted memories
                      </h4>
                      <span className="ml-auto shrink-0 rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] text-emerald-700 dark:text-emerald-300">
                        {promotedMemoryItems.length}
                      </span>
                    </div>
                    <ul className="space-y-2">
                      {promotedMemoryItems.map((item, idx) => (
                        <MemoryCard
                          key={item.id}
                          span={item.span}
                          memoryId={item.memoryId}
                          memoryText={item.text}
                          datapackName={datapackName}
                          datapackId={datapackId}
                          promoted={item.promoted}
                          entityNames={item.entityNames}
                          relationNames={item.relationNames}
                          signals={item.signals}
                          ordinal={normalMemoryItems.length + idx + 1}
                          highlighted={hoveredSpanId === item.span.id}
                          targeted={!!flashMemoryId && item.memoryId === flashMemoryId}
                          hoverSource={hoverSource}
                          onHoverSpan={handleHoverSpan}
                        />
                      ))}
                    </ul>
                  </>
                ) : null}
              </div>
            </div>
          ) : null}
        </div>

        {/*
          Footer — deep-links into the Observe Hub for this Langfuse
          session. Opens in a new tab (this drawer is contextual; we
          don't want to lose the user's place in the chat / thread).
          Renders only when we have both ids; if Langfuse hasn't
          surfaced a project id yet (e.g. trace fetch failed) we just
          hide the row rather than rendering a dead button.
        */}
        {langfuseProjectId && conversationId ? (
          <footer className="border-t border-border px-5 py-3 shrink-0">
            <a
              href={`${ROUTES.observeHub}?${new URLSearchParams({
                project: langfuseProjectId,
                session: conversationId.replace(/-/g, ""),
              }).toString()}`}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-purple-500/30 bg-purple-500/10 px-3 py-2 text-xs font-medium text-purple-700 dark:text-purple-200 transition-colors hover:bg-purple-500/15 hover:text-purple-800 dark:hover:text-purple-100"
              title="Open this conversation in the full Langfuse trace view (new tab)"
            >
              <ExternalLink className="h-3.5 w-3.5" aria-hidden />
              View Detailed Trace
            </a>
          </footer>
        ) : null}
      </aside>
    </>
  );
}

// ---------------------------------------------------------------------------
// MemoryCard: renders one extracted memory from a memory-related span.
// The `output` blob can be a JSON string (various Mem0 response shapes)
// or plain text; we try to parse it and pull out human-readable strings.
// ---------------------------------------------------------------------------

// Deep-link to the knowledge page's Promoted memories tab, targeting a
// specific promoted memory so the page can scroll to + shimmer it.
function buildPromotedKBHref(
  datapackName: string,
  datapackId: string | undefined,
  memoryId: string | undefined,
): string {
  const base = datapackKnowledgePath(datapackName, datapackId);
  const sep = base.includes("?") ? "&" : "?";
  const params = new URLSearchParams({ tab: "promoted-memories" });
  if (memoryId) params.set("promotedMemoryId", memoryId);
  return `${base}${sep}${params.toString()}`;
}

type MemoryCardProps = {
  span: DatapackTraceSpan;
  memoryId?: string;
  memoryText: string;
  /** Datapack identity for the "Promoted to KB" deep-link to the knowledge page. */
  datapackName: string;
  datapackId?: string;
  /** True when this memory was promoted to the KB and evicted from mem0.
   *  vector/entities/relations counts are meaningless then, so the card
   *  shows just the text + a "Promoted to KB" tag. */
  promoted?: boolean;
  entityNames: string[];
  relationNames: string[];
  signals: MemorySignals;
  ordinal: number;
  highlighted: boolean;
  /** Deep-link target from ?memoryId= — stronger highlight + auto-scroll. */
  targeted?: boolean;
  /** Side of the drawer that originated the current hover. We only scroll
   *  this card into view when the hover came from the *tree*, never when the
   *  user's cursor is already inside the memory list. */
  hoverSource: HoverSource;
  onHoverSpan: (id: string | null, source: HoverSource) => void;
};

function MemoryCard({
  span,
  memoryId,
  memoryText,
  datapackName,
  datapackId,
  promoted = false,
  entityNames,
  relationNames,
  signals,
  ordinal,
  highlighted,
  targeted = false,
  hoverSource,
  onHoverSpan,
}: MemoryCardProps) {
  const topNodes = entityNames.slice(0, 2);
  const topRelations = relationNames.slice(0, 2);
  const hasPreview = topNodes.length > 0 || topRelations.length > 0;

  const cardRef = useRef<HTMLLIElement>(null);

  // Only auto-scroll when the hover originated from the tree side. If the
  // cursor is already on the memory list, scrolling would shift another card
  // under the pointer and start a hover/scroll feedback loop.
  useEffect(() => {
    if (highlighted && hoverSource === "tree" && cardRef.current) {
      cardRef.current.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
    }
  }, [highlighted, hoverSource]);

  // Deep-link target: scroll the card into view, then flash it amber for
  // a few seconds before fading back to the resting card style. The
  // animation runs ~2.8s (4 pulses × 700ms); after that we clear the
  // flag so the highlight goes away on its own without the URL needing
  // to change.
  const [flashing, setFlashing] = useState(false);
  useEffect(() => {
    if (!targeted) {
      setFlashing(false);
      return;
    }
    if (cardRef.current) {
      cardRef.current.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
    }
    setFlashing(true);
    const t = window.setTimeout(() => setFlashing(false), 2800);
    return () => window.clearTimeout(t);
  }, [targeted]);

  return (
    <li
      ref={cardRef}
      className={cn(
        "rounded-lg border px-3 py-2.5 transition-colors duration-300 cursor-default",
        !flashing && highlighted
          ? "border-blue-400/60 bg-blue-500/15 ring-1 ring-blue-400/30"
          : !flashing
            ? "border-blue-500/20 bg-blue-500/5 hover:border-blue-400/40 hover:bg-blue-500/10"
            : "border-amber-400/80 bg-amber-500/15",
      )}
      style={flashing ? { animation: "meko-memory-flash 0.7s ease-in-out 4" } : undefined}
      onMouseEnter={() => onHoverSpan(span.id, "memory")}
      onMouseLeave={() => onHoverSpan(null, "memory")}
    >
      <div className="mb-1.5 flex items-center gap-1.5">
        <Brain className="h-3 w-3 shrink-0 text-blue-600 dark:text-blue-300" aria-hidden />
        <span
          className="truncate text-[11px] text-blue-700 dark:text-blue-200/70"
          title={span.name}
        >
          {`${span.name} #${ordinal}`}
        </span>
        <span className="ml-auto shrink-0 tabular-nums text-[10px] text-muted-foreground">
          {formatDuration(span.duration_ms)}
        </span>
      </div>
      <p className="text-[12.5px] leading-relaxed text-foreground/85">{memoryText}</p>
      {promoted ? (
        // Promoted memories live in the KB now (evicted from mem0), so the
        // vector/entity/relation counts no longer apply. The status row links
        // to the knowledge page's Promoted memories tab, deep-linking to this
        // entry so the user can inspect its shared-knowledge record.
        <Link
          to={buildPromotedKBHref(datapackName, datapackId, memoryId)}
          title="View in knowledge base"
          className="group mt-2.5 flex items-center gap-2 rounded-md border border-emerald-500/25 bg-emerald-500/[0.07] px-2.5 py-1.5 transition-colors hover:border-emerald-500/45 hover:bg-emerald-500/[0.12] focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400/50"
        >
          <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600 dark:text-emerald-300">
            <BookUp className="h-3.5 w-3.5" aria-hidden />
          </span>
          <div className="min-w-0 leading-tight">
            <div className="text-[11px] font-semibold text-emerald-700 dark:text-emerald-300">
              Promoted to knowledge base
            </div>
            <div className="text-[10px] text-muted-foreground">Now a shared knowledge entry</div>
          </div>
          <SquareArrowOutUpRight
            className="ml-auto h-3 w-3 shrink-0 text-emerald-600/70 transition-colors group-hover:text-emerald-600 dark:text-emerald-300/70 dark:group-hover:text-emerald-300"
            aria-hidden
          />
        </Link>
      ) : (
        <div className="mt-2 flex flex-wrap items-center gap-1.5 overflow-visible">
          <MemoryMetaBadge
            label={`vector ${signals.vector}`}
            metricName="Vector"
            metricValue={signals.vector}
            description="Vector rows linked to this memory (1 means it is embedded)."
            detail="Stored embedding row in mem0 vector collection for this memory."
            memoryId={memoryId}
            spanDurationMs={span.duration_ms}
            entityNames={entityNames}
            relationNames={relationNames}
          />
          <MemoryMetaBadge
            label={`entities ${signals.entities}`}
            metricName="Entities"
            metricValue={signals.entities}
            description="Unique graph entities (nodes) referenced by this memory."
            detail="Counted from unique graph_nodes attached to this persisted memory."
            memoryId={memoryId}
            spanDurationMs={span.duration_ms}
            entityNames={entityNames}
            relationNames={relationNames}
          />
          <MemoryMetaBadge
            label={`relations ${signals.relations}`}
            metricName="Relations"
            metricValue={signals.relations}
            description="Unique graph relations (edges) connected to this memory's entities."
            detail="Edges touching those entities within this datapack and conversation."
            memoryId={memoryId}
            spanDurationMs={span.duration_ms}
            entityNames={entityNames}
            relationNames={relationNames}
          />
        </div>
      )}
      {!promoted && hasPreview ? (
        <div className="mt-1.5 space-y-1">
          {topNodes.length > 0 ? (
            <div className="flex min-w-0 items-start gap-1 text-[10px] text-blue-700 dark:text-blue-100/65">
              <span className="shrink-0 text-blue-700/70 dark:text-blue-100/55">Sample nodes:</span>
              <span className="flex min-w-0 flex-1 flex-wrap gap-1">
                {topNodes.map((name) => (
                  <span
                    key={name}
                    className="max-w-full truncate rounded border border-blue-300/25 bg-blue-500/10 px-1.5 py-0.5 font-mono text-[10px] text-blue-700 dark:text-blue-100/90"
                    title={name}
                  >
                    {name}
                  </span>
                ))}
              </span>
            </div>
          ) : null}
          {topRelations.length > 0 ? (
            <div className="flex min-w-0 items-start gap-1 text-[10px] text-blue-700 dark:text-blue-100/65">
              <span className="shrink-0 text-blue-700/70 dark:text-blue-100/55">
                Sample relations:
              </span>
              <span className="flex min-w-0 flex-1 flex-wrap gap-1">
                {topRelations.map((name) => (
                  <span
                    key={name}
                    className="max-w-full truncate rounded border border-blue-300/25 bg-blue-500/10 px-1.5 py-0.5 font-mono text-[10px] text-blue-700 dark:text-blue-100/90"
                    title={name}
                  >
                    {name}
                  </span>
                ))}
              </span>
            </div>
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

function MemoryMetaBadge({
  label,
  metricName,
  metricValue,
  description,
  detail,
  memoryId,
  spanDurationMs,
  entityNames,
  relationNames,
}: {
  label: string;
  metricName: string;
  metricValue: number;
  description: string;
  detail: string;
  memoryId?: string;
  spanDurationMs: number;
  entityNames: string[];
  relationNames: string[];
}) {
  const dynamicSummary =
    metricName === "Vector"
      ? metricValue > 0
        ? "Embedding present for this memory."
        : "No embedding row linked to this memory."
      : metricName === "Entities"
        ? `${metricValue} graph node${metricValue === 1 ? "" : "s"} referenced.`
        : `${metricValue} edge${metricValue === 1 ? "" : "s"} connected to those nodes.`;

  const sampledNames =
    metricName === "Entities" ? entityNames : metricName === "Relations" ? relationNames : [];

  const sampledLabel =
    metricName === "Entities"
      ? "Sample nodes (subset)"
      : metricName === "Relations"
        ? "Sample relations (subset)"
        : "Sample graph labels";
  return (
    <HoverInfoBadge label={label} ariaLabel={`${label}: ${description}`}>
      <span className="mb-1 block text-[11px] font-medium text-foreground">
        {metricName}: {metricValue}
      </span>
      <span className="block text-[10px] text-foreground/85">{description}</span>
      <span className="mt-1 block text-[10px] text-muted-foreground">{detail}</span>
      <span className="mt-1 block text-[10px] text-foreground/75">{dynamicSummary}</span>
      <span className="mt-1 block text-[10px] text-muted-foreground">
        Span latency: {formatDuration(spanDurationMs)} · Source: persisted mem0 + graph DB
      </span>
      {sampledNames.length > 0 ? (
        <span className="mt-1 block text-[10px] text-foreground/75">
          {sampledLabel}
          <span className="mt-1 flex flex-wrap gap-1">
            {sampledNames.slice(0, 4).map((name) => (
              <span
                key={name}
                className="rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[10px] text-foreground/90"
              >
                {name}
              </span>
            ))}
          </span>
        </span>
      ) : null}
      {memoryId ? (
        <span className="mt-1 block font-mono text-[10px] text-muted-foreground">
          Memory ID: {compactTraceMemoryID(memoryId)}
        </span>
      ) : null}
    </HoverInfoBadge>
  );
}

function compactTraceMemoryID(id: string): string {
  const trimmed = id.trim();
  if (trimmed.length <= 14) return trimmed;
  return `${trimmed.slice(0, 8)}...${trimmed.slice(-4)}`;
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
  /** Side of the drawer that originated the current hover. We only scroll
   *  this row into view when the hover came from the *memory* side, never
   *  when the user is moving the cursor inside the tree itself. */
  hoverSource: HoverSource;
  activePathSpanIds: Set<string> | null;
  onHoverSpan: (id: string | null, source: HoverSource) => void;
  /** Span to flash + scroll into view (from "View Trace" on a step). */
  flashSpanId: string | null;
};

/** Token usage + cost aggregated across a span subtree. */
type SubtreeUsage = {
  model: string;
  input: number;
  output: number;
  total: number;
  cost: number;
  count: number;
};

/** Sum token usage and cost across the GENERATION (LLM) spans that belong
 *  to one operation (TOOL) span like "Memory Add" / "Memory Search", whose
 *  own row carries no usage — the LLM calls live on nested GENERATION
 *  children. The model is taken from the first GENERATION encountered.
 *
 *  Attribution is to the NEAREST TOOL ancestor: the walk does not descend
 *  into a nested child TOOL, so generations belonging to a sub-operation
 *  are counted on that sub-operation's own row, never double-counted here.
 *  This matches the per-operation totals computed server-side. */
function aggregateSubtreeUsage(
  spanId: string,
  byId: Record<string, DatapackTraceSpan>,
  childrenOf: Record<string, string[]>,
): SubtreeUsage {
  const agg: SubtreeUsage = { model: "", input: 0, output: 0, total: 0, cost: 0, count: 0 };
  const visit = (id: string, isRoot: boolean) => {
    const s = byId[id];
    if (!s) return;
    // A nested TOOL is a separate operation with its own row — don't count
    // its LLM calls against this one.
    if (!isRoot && s.type === "TOOL") return;
    if (s.type === "GENERATION") {
      const u = parseTraceUsage(s.usage);
      if (u) {
        agg.input += u.input ?? 0;
        agg.output += u.output ?? 0;
        agg.total += u.total ?? (u.input ?? 0) + (u.output ?? 0);
        agg.count += 1;
      }
      const c = parseTraceCost(s.total_cost);
      if (c !== null) agg.cost += c;
      if (!agg.model && s.model) agg.model = s.model;
    }
    for (const childId of childrenOf[id] ?? []) visit(childId, false);
  };
  visit(spanId, true);
  return agg;
}

function SpanTreeNode({
  spanId,
  depth,
  byId,
  childrenOf,
  totalMs,
  collapsed,
  onToggle,
  hoveredSpanId,
  hoverSource,
  activePathSpanIds,
  onHoverSpan,
  flashSpanId,
}: NodeProps) {
  const span = byId[spanId];
  if (!span) return null;
  const children = childrenOf[spanId] ?? [];
  const hasChildren = children.length > 0;
  const isCollapsed = !!collapsed[spanId];
  const isHovered = hoveredSpanId === spanId;
  const hasHoverContext = hoveredSpanId !== null;
  const isOnActivePath = activePathSpanIds?.has(spanId) ?? false;

  const rowRef = useRef<HTMLDivElement>(null);
  const isFlashing = flashSpanId === spanId;

  // The DB statement this span itself executed (if any). Each span shows
  // its OWN statement directly beneath its row — e.g. the three
  // "Execute Query" leaves under "Knowledgebase Search" each show their
  // own BEGIN / SELECT / ROLLBACK — rather than rolling the whole subtree
  // up into a single consolidated list on the operation (TOOL) span.
  const ownStatement = span.input?.trim() ?? "";
  const hasQuery = ownStatement.length > 0;
  const [showQuery, setShowQuery] = useState(false);

  // Only auto-scroll when the hover came from the memory cards. If the
  // user is moving the cursor inside the tree, scrolling this row would
  // shift a different row under the pointer and cause a hover/scroll loop
  // — exactly the runaway "drift downward" behaviour we want to avoid.
  useEffect(() => {
    if (isHovered && hoverSource === "memory" && rowRef.current) {
      rowRef.current.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
    }
  }, [isHovered, hoverSource]);

  // "View Trace" focus: scroll the targeted span into view when it flashes.
  useEffect(() => {
    if (isFlashing && rowRef.current) {
      rowRef.current.scrollIntoView({ behavior: "smooth", block: "center" });
    }
  }, [isFlashing]);

  // Waterfall sizing — left offset and width are percentages of the
  // total message wall time.
  const leftPct = (span.offset_ms / totalMs) * 100;
  const widthPct = Math.max(0.5, (span.duration_ms / totalMs) * 100);

  const accent = spanAccent(span);
  const usage = parseTraceUsage(span.usage);
  const totalCost = parseTraceCost(span.total_cost);
  const tokenText = usage ? formatTokenUsage(usage) : null;
  const costText = totalCost !== null ? formatUsdCompact(totalCost) : null;

  // Operation (TOOL) spans (e.g. "Memory Add", "Memory Search") carry no
  // usage of their own — the LLM work that extracts entities lives on
  // nested GENERATION children. Roll those up so the row shows the model +
  // total tokens + cost for the whole operation. Falls back gracefully
  // when there are no LLM calls (e.g. Knowledgebase Search).
  const aggUsage =
    !usage && span.type === "TOOL" ? aggregateSubtreeUsage(spanId, byId, childrenOf) : null;
  const showAgg = !!aggUsage && aggUsage.count > 0;
  const aggSuffix = showAgg
    ? ` (sum of ${aggUsage!.count} LLM call${aggUsage!.count === 1 ? "" : "s"})`
    : "";
  const displayModel = span.model || (showAgg ? aggUsage!.model : "");
  const displayTokens = tokenText || (showAgg ? formatTokenUsage(aggUsage!) : null);
  const displayCost =
    costText || (showAgg && aggUsage!.cost > 0 ? formatUsdCompact(aggUsage!.cost) : null);

  return (
    <li>
      <div
        ref={rowRef}
        className={cn(
          "group flex items-center gap-2 rounded-md px-1.5 py-1 transition-colors duration-100 cursor-default",
          isFlashing
            ? "border border-amber-400/80 bg-amber-500/15"
            : isHovered
              ? "bg-blue-500/15 ring-1 ring-inset ring-blue-400/30"
              : "hover:bg-muted",
          hasHoverContext && !isOnActivePath && !isFlashing && "opacity-45",
        )}
        style={{
          paddingLeft: `${depth * 14 + 6}px`,
          ...(isFlashing ? { animation: "meko-memory-flash 0.7s ease-in-out 4" } : {}),
        }}
        onMouseEnter={() => onHoverSpan(spanId, "tree")}
        onMouseLeave={() => onHoverSpan(null, "tree")}
      >
        {hasChildren ? (
          <button
            type="button"
            onClick={() => onToggle(spanId)}
            className="flex h-4 w-4 shrink-0 cursor-pointer items-center justify-center rounded text-muted-foreground hover:text-foreground"
            aria-label={isCollapsed ? "Expand" : "Collapse"}
            tabIndex={0}
          >
            {isCollapsed ? (
              <ChevronRight className="h-3 w-3" aria-hidden />
            ) : (
              <ChevronDown className="h-3 w-3" aria-hidden />
            )}
          </button>
        ) : (
          <span className="flex h-4 w-4 shrink-0 items-center justify-center" aria-hidden>
            <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground/60" />
          </span>
        )}

        <span
          className={cn(
            "inline-flex h-4 w-4 shrink-0 items-center justify-center",
            accent.iconColor,
          )}
        >
          {accent.icon}
        </span>

        <span
          className="min-w-0 flex-1 max-w-full truncate text-[13px] text-foreground/90"
          title={span.name}
        >
          {span.name}
        </span>

        <div className="flex items-center gap-3 shrink-0 ml-auto">
          {displayModel ? (
            <code
              className={cn(
                "hidden w-20 truncate text-right font-mono text-[10px] sm:inline",
                showAgg ? "text-muted-foreground/80 italic" : "text-muted-foreground",
              )}
              title={showAgg ? `Extraction model${aggSuffix}` : undefined}
            >
              {displayModel}
            </code>
          ) : null}

          {displayTokens ? (
            <span
              className={cn(
                "hidden w-28 shrink-0 text-right tabular-nums text-[10px] md:inline",
                showAgg ? "text-muted-foreground/80 italic" : "text-muted-foreground",
              )}
              title={`Tokens: ${displayTokens}${aggSuffix}`}
            >
              {displayTokens}
            </span>
          ) : null}

          {displayCost ? (
            <span
              className={cn(
                "hidden w-14 shrink-0 text-right tabular-nums text-[10px] md:inline",
                showAgg
                  ? "text-emerald-600/80 dark:text-emerald-300/70 italic"
                  : "text-emerald-600 dark:text-emerald-300/85",
              )}
              title={`Cost: ${displayCost}${aggSuffix}`}
            >
              {displayCost}
            </span>
          ) : null}

          <div className="shrink-0 flex items-center gap-3 text-[11px] tabular-nums text-muted-foreground">
            <span className="hidden sm:inline-block w-14 text-right">{`+${formatDuration(span.offset_ms)}`}</span>
            <span className="inline-block w-14 text-right">{formatDuration(span.duration_ms)}</span>
          </div>
        </div>
      </div>

      {/* Waterfall bar: renders directly under the row, indented to
          match. Uses left/width % so resizing the drawer reflows
          smoothly. */}
      <div
        className="relative mb-1 h-1 rounded-full bg-muted"
        style={{ marginLeft: `${depth * 14 + 32}px`, marginRight: "12px" }}
      >
        <div
          className={cn(
            "absolute h-1 rounded-full",
            accent.barColor,
            hasHoverContext && !isOnActivePath && "opacity-40",
          )}
          style={{ left: `${leftPct}%`, width: `${widthPct}%` }}
          title={`starts +${formatDuration(span.offset_ms)} · runs ${formatDuration(span.duration_ms)}`}
        />
      </div>

      {/* DB statement disclosure — the SQL / Cypher statement this span
          executed, shown directly beneath its own row. Bind params
          (embedding vectors) are stripped backend-side, so placeholders
          (%s / $1) are shown as-is. */}
      {hasQuery ? (
        <div
          className={cn("mb-1", hasHoverContext && !isOnActivePath && !isFlashing && "opacity-45")}
          style={{ marginLeft: `${depth * 14 + 32}px`, marginRight: "12px" }}
        >
          <button
            type="button"
            onClick={() => setShowQuery((v) => !v)}
            className="inline-flex items-center gap-1 text-[10.5px] text-muted-foreground hover:text-foreground transition-colors"
            title="Show the statement executed in this step"
          >
            <Code2 className="h-3 w-3" aria-hidden />
            {showQuery ? "Hide query" : "View query"}
          </button>
          {showQuery ? (
            <pre className="mt-1 max-h-56 overflow-auto rounded-md border border-border bg-muted/50 px-2.5 py-2 font-mono text-[10.5px] leading-relaxed text-foreground/90 whitespace-pre-wrap break-words">
              {beautifyStatement(span.name, ownStatement)}
            </pre>
          ) : null}
        </div>
      ) : null}

      {hasChildren && !isCollapsed ? (
        <ul>
          {children.map((childId) => (
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
              hoverSource={hoverSource}
              activePathSpanIds={activePathSpanIds}
              onHoverSpan={onHoverSpan}
              flashSpanId={flashSpanId}
            />
          ))}
        </ul>
      ) : null}
    </li>
  );
}

function TimeAxis({ totalMs }: { totalMs: number }) {
  const ticks = [0, 25, 50, 75, 100];
  return (
    <div className="mb-2 ml-8 mr-3 relative h-5">
      <div className="absolute inset-x-0 top-2 h-px bg-border" />
      {ticks.map((pct) => (
        <div
          key={pct}
          className="absolute top-0 -translate-x-1/2 text-[10px] text-muted-foreground tabular-nums"
          style={{ left: `${pct}%` }}
        >
          {pct === 0 ? "0ms" : pct === 100 ? formatDuration(totalMs) : `${pct}%`}
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Tree builder: turn a flat list of spans into a tree data structure
// (`byId` + `childrenOf` + `roots`) used by the UI renderer.
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

function collectPathSpanIDs(
  spanId: string | null,
  byId: Record<string, DatapackTraceSpan>,
  childrenOf: Record<string, string[]>,
): Set<string> | null {
  if (!spanId || !byId[spanId]) return null;
  const out = new Set<string>([spanId]);

  // Ancestors
  let parentId = byId[spanId].parent_observation_id;
  while (parentId && byId[parentId]) {
    out.add(parentId);
    parentId = byId[parentId].parent_observation_id;
  }

  // Descendants
  const stack = [...(childrenOf[spanId] ?? [])];
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (out.has(id)) continue;
    out.add(id);
    stack.push(...(childrenOf[id] ?? []));
  }
  return out;
}

/** Max depth from any root (root row = 0, first child row = 1, …). */
function maxSpanTreeDepthFromRoots(childrenOf: Record<string, string[]>, roots: string[]): number {
  let globalMax = 0;
  const walk = (id: string, depth: number) => {
    globalMax = Math.max(globalMax, depth);
    for (const c of childrenOf[id] ?? []) walk(c, depth + 1);
  };
  for (const r of roots) walk(r, 0);
  return globalMax;
}

/** When the forest depth is ≤ 1, collapse every span that has children by default. */
function defaultCollapsedForShallowTree(
  childrenOf: Record<string, string[]>,
  roots: string[],
): Record<string, boolean> {
  if (maxSpanTreeDepthFromRoots(childrenOf, roots) > 1) return {};
  const out: Record<string, boolean> = {};
  for (const [parentId, children] of Object.entries(childrenOf)) {
    if (children.length > 0) out[parentId] = true;
  }
  return out;
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
  const memoryLikeName = isMemoryLikeSpanName(span.name);
  const databaseLikeName = isDatabaseLikeSpanName(span.name);

  // Use one consistent icon for memory operations regardless of span type.
  if (memoryLikeName) {
    return {
      icon: <MemoryIcon className="text-[13px]" />,
      iconColor: "text-blue-600 dark:text-blue-300",
      barColor: "bg-blue-400/70",
    };
  }

  if (databaseLikeName) {
    return {
      icon: <Database className="h-3 w-3" aria-hidden />,
      iconColor: "text-cyan-600 dark:text-cyan-300",
      barColor: "bg-cyan-400/70",
    };
  }

  if (t === "GENERATION" || t === "LLM") {
    return {
      icon: <Cpu className="h-3 w-3" aria-hidden />,
      iconColor: "text-amber-600 dark:text-amber-300",
      barColor: "bg-amber-400/70",
    };
  }
  if (t === "TOOL") {
    return {
      icon: <Wrench className="h-3 w-3" aria-hidden />,
      iconColor: "text-emerald-600 dark:text-emerald-300",
      barColor: "bg-emerald-400/70",
    };
  }
  if (t === "RETRIEVAL" || t === "EMBEDDING") {
    return {
      icon: <Database className="h-3 w-3" aria-hidden />,
      iconColor: "text-cyan-600 dark:text-cyan-300",
      barColor: "bg-cyan-400/70",
    };
  }
  if (t === "AGENT" || t === "SPAN") {
    return {
      icon: <Bot className="h-3 w-3" aria-hidden />,
      iconColor: "text-purple-600 dark:text-purple-300",
      barColor: "bg-purple-400/70",
    };
  }
  return {
    icon: <Clock className="h-3 w-3" aria-hidden />,
    iconColor: "text-foreground/60",
    barColor: "bg-foreground/40",
  };
}

function isMemoryLikeSpanName(name: string): boolean {
  const normalized = name.toLowerCase();
  return normalized.includes("memory") || /mem\d/.test(normalized);
}

function isDatabaseLikeSpanName(name: string): boolean {
  const normalized = name.toLowerCase();
  return /(\bvector\b|\bgraph\b|\bdatabase\b|\bdb\b|\bquery\b|\bsql\b|\bpostgres\b|\byugabyte\b|\bretriev|\bembed|\bmage\b|\bage\b|\bcypher\b|\bpgvector\b)/.test(
    normalized,
  );
}

function normalizeMemoryTextForDedupe(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9\s]+/g, " ")
    .replace(/\s+/g, " ")
    .trim();
}

type MemorySignals = {
  vector: number;
  entities: number;
  relations: number;
};

function parseTraceUsage(
  raw: string | undefined,
): { input?: number; output?: number; total?: number } | null {
  if (!raw) return null;
  try {
    const obj = JSON.parse(raw) as Record<string, unknown>;
    if (!obj || typeof obj !== "object") return null;
    const input = toNumberOrNull(obj.input);
    const output = toNumberOrNull(obj.output);
    const total = toNumberOrNull(obj.total);
    if (input === null && output === null && total === null) return null;
    return {
      ...(input !== null ? { input } : {}),
      ...(output !== null ? { output } : {}),
      ...(total !== null ? { total } : {}),
    };
  } catch {
    return null;
  }
}

function parseTraceCost(raw: string | undefined): number | null {
  if (!raw) return null;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return parsed;
}

function toNumberOrNull(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value) && value >= 0) return value;
  if (typeof value === "string" && value.trim()) {
    const n = Number(value);
    if (Number.isFinite(n) && n >= 0) return n;
  }
  return null;
}

function formatTokenUsage(usage: { input?: number; output?: number; total?: number }): string {
  const inTokens = usage.input ?? 0;
  const outTokens = usage.output ?? 0;
  const totalTokens = usage.total ?? inTokens + outTokens;
  return `${formatCompactInt(inTokens)}→${formatCompactInt(outTokens)} (∑${formatCompactInt(totalTokens)})`;
}

function formatCompactInt(n: number): string {
  return new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(Math.round(n));
}

function formatUsdCompact(v: number): string {
  if (v < 0.001) return `$${v.toFixed(4)}`;
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    notation: "compact",
    maximumFractionDigits: 3,
  }).format(v);
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
