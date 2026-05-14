// Collapsible "Agent reasoning" panel rendered above each assistant
// bubble. Single chronological timeline of the per-turn pipeline:
//
//   ✓ conversation_create        (first turn only)
//   ✓ memory_search              ← expandable: shows hits + the query
//   ✓ knowledgebase_search       ← expandable: KB chunks grouped by doc,
//                                  each doc collapsible
//   ✓ llm_invoke
//   ✓ conversation_add_message
//   ⊘ memory_add                 (fire-and-forget background)
//
// Memory and KB hits live INSIDE their respective step rows now (instead
// of as separate sections at the top) so the user reads top-to-bottom in
// the order things actually happened. Each retrieval step also surfaces
// the exact query the gateway sent to MCP — useful when the user's
// prompt was long and they want to know what slice was searched on.

import { useMemo, useState } from "react";
import {
  Brain,
  Check,
  ChevronDown,
  ChevronRight,
  Clock,
  CornerDownRight,
  FileText,
  MinusCircle,
  Search,
  Sparkles,
  X as XIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";
import type {
  KbHit,
  MemoryHit,
  ProcessingStep,
} from "@/lib/inferenceGateway";

type Props = {
  memoryHits: MemoryHit[];
  kbHits: KbHit[];
  processingSteps: ProcessingStep[];
};

export function AgentReasoningBlock({
  memoryHits,
  kbHits,
  processingSteps,
}: Props) {
  // Outer panel: collapsed by default, click header to expand.
  const [expanded, setExpanded] = useState(false);

  // Group KB chunks by document_id so each document shows up once with
  // its chunks nested. Preserves arrival order (top-K by relevance) so
  // the strongest match per doc is first inside its group.
  const kbGroups = useMemo(() => groupKbHitsByDocument(kbHits), [kbHits]);

  const memoryCount = memoryHits.length;
  const kbChunkCount = kbHits.length;
  const kbDocCount = kbGroups.length;
  const stepCount = processingSteps.length;

  // Hide the entire panel when there's nothing to show. A bubble with
  // zero hits and zero steps is a degenerate case (shouldn't happen
  // post-trivial-query but defensive against gateway oddities).
  if (memoryCount === 0 && kbChunkCount === 0 && stepCount === 0) return null;

  const summaryParts: string[] = [];
  if (memoryCount > 0) {
    summaryParts.push(`${memoryCount} ${memoryCount === 1 ? "memory" : "memories"}`);
  }
  if (kbChunkCount > 0) {
    summaryParts.push(
      `${kbChunkCount} ${kbChunkCount === 1 ? "chunk" : "chunks"}` +
      ` from ${kbDocCount} ${kbDocCount === 1 ? "doc" : "docs"}`,
    );
  }
  if (stepCount > 0) {
    summaryParts.push(`${stepCount} step${stepCount === 1 ? "" : "s"}`);
  }
  const summary = summaryParts.join(" · ");

  return (
    <div className="mb-2 rounded-lg border border-purple-500/25 bg-purple-500/[0.04]">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full items-center gap-2 rounded-lg px-3 py-1.5 text-left transition-colors hover:bg-purple-500/[0.08]"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-purple-300" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-purple-300" aria-hidden />
        )}
        <Brain className="h-3 w-3 shrink-0 text-purple-300" aria-hidden />
        <span className="text-[11px] font-medium uppercase tracking-wide text-purple-200/90">
          Decision Trace
        </span>
        {summary ? (
          <span className="text-[11px] text-muted-foreground">· {summary}</span>
        ) : null}
      </button>
      {expanded ? (
        <ol className="space-y-1.5 border-t border-purple-500/15 px-3 py-3">
          {processingSteps.map((step, i) => (
            <li
              // Index suffix on the key because a single turn can have
              // multiple steps with the same name in pathological cases
              // (e.g. error → retry; not common but cheap to guard).
              key={`${step.name}-${i}`}
            >
              <StepRow
                step={step}
                memoryHits={step.name === "memory_search" ? memoryHits : []}
                kbGroups={step.name === "knowledgebase_search" ? kbGroups : []}
              />
            </li>
          ))}
        </ol>
      ) : null}
    </div>
  );
}

// --------------------------------------------------------------------------- //
// Step rows
// --------------------------------------------------------------------------- //

function StepRow({
  step,
  memoryHits,
  kbGroups,
}: {
  step: ProcessingStep;
  /** Populated only when `step.name === "memory_search"` — drives the
   *  nested hits list inside the expandable row. */
  memoryHits: MemoryHit[];
  /** Populated only when `step.name === "knowledgebase_search"`. */
  kbGroups: KbGroup[];
}) {
  // Only retrieval steps have content worth expanding. Other steps
  // (conversation_create, llm_invoke, etc.) render as flat one-liners.
  const isExpandable =
    (step.name === "memory_search" && memoryHits.length > 0) ||
    (step.name === "knowledgebase_search" && kbGroups.length > 0);

  const [expanded, setExpanded] = useState(false);

  const headerContent = (
    <>
      {isExpandable ? (
        expanded ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-purple-300/80" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-purple-300/80" aria-hidden />
        )
      ) : (
        // Spacer so flat steps align under the chevroned ones — keeps
        // the icon/name column visually consistent.
        <span className="h-3 w-3 shrink-0" aria-hidden />
      )}
      <StatusIcon status={step.status} />
      <code className="min-w-0 truncate font-mono text-[11.5px] text-foreground/85">
        {step.name}
      </code>
      {step.duration_ms > 0 ? (
        <span className="shrink-0 tabular-nums text-[11px] text-muted-foreground">
          {formatDuration(step.duration_ms)}
        </span>
      ) : null}
      {step.detail ? (
        <span
          className="shrink-0 max-w-[180px] truncate text-[11px] text-muted-foreground/70"
          title={step.detail}
        >
          — {step.detail}
        </span>
      ) : null}
    </>
  );

  // The query line sits BELOW the step header (not inline) so it doesn't
  // get visually crushed between the duration and the detail. It's also
  // always visible when the outer panel is open — the user shouldn't
  // have to expand each step just to see what was searched on.
  // Indented to line up under the chevron/name column.
  const queryLine = step.query ? (
    <div className="ml-5 mt-0.5 flex items-center gap-1.5 text-[11px]">
      <Search className="h-2.5 w-2.5 shrink-0 text-purple-300/70" aria-hidden />
      <span className="text-muted-foreground/80">Query:</span>
      <span
        className="min-w-0 truncate font-mono text-foreground/80"
        title={step.query}
      >
        "{truncateQuery(step.query, 80)}"
      </span>
    </div>
  ) : null;

  // "MCP args" sub-expand — sits below the query line, collapsed by
  // default to keep the timeline scannable. Click to reveal the rest of
  // the args the gateway sent to MCP for this step (limit, datapack_id,
  // etc. — everything except `query` and the deliberately-skipped
  // `scope`/`conversation_id`).
  const toolArgsRow =
    step.tool_args && Object.keys(step.tool_args).length > 0 ? (
      <ToolArgsRow args={step.tool_args} />
    ) : null;

  if (!isExpandable) {
    return (
      <div>
        <div className="flex items-center gap-2 px-1 py-0.5">{headerContent}</div>
        {queryLine}
        {toolArgsRow}
      </div>
    );
  }

  return (
    <div>
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full items-center gap-2 rounded px-1 py-0.5 text-left transition-colors hover:bg-purple-500/[0.05]"
        aria-expanded={expanded}
      >
        {headerContent}
      </button>
      {queryLine}
      {toolArgsRow}
      {expanded ? (
        <div className="mt-1 ml-5 border-l border-purple-500/15 pl-3">
          {step.name === "memory_search" ? (
            <MemoryHitsList hits={memoryHits} />
          ) : null}
          {step.name === "knowledgebase_search" ? (
            <KbGroupsList groups={kbGroups} />
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// --------------------------------------------------------------------------- //
// "MCP args" sub-expand — independently collapsible below each step row.
// --------------------------------------------------------------------------- //

function ToolArgsRow({ args }: { args: Record<string, unknown> }) {
  const [expanded, setExpanded] = useState(false);
  const entries = Object.entries(args);

  return (
    <div className="ml-5 mt-0.5">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="inline-flex items-center gap-1.5 text-[11px] text-muted-foreground/80 transition-colors hover:text-foreground"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-2.5 w-2.5 shrink-0" aria-hidden />
        ) : (
          <ChevronRight className="h-2.5 w-2.5 shrink-0" aria-hidden />
        )}
        <span>MCP args</span>
        <span className="text-muted-foreground/50">({entries.length})</span>
      </button>
      {expanded ? (
        <dl className="ml-3.5 mt-1 space-y-0.5">
          {entries.map(([key, value]) => (
            <div
              key={key}
              className="flex gap-2 font-mono text-[10.5px]"
            >
              <dt className="shrink-0 text-muted-foreground/70">{key}:</dt>
              <dd
                className="min-w-0 truncate text-foreground/85"
                title={String(value)}
              >
                {formatArgValue(value)}
              </dd>
            </div>
          ))}
        </dl>
      ) : null}
    </div>
  );
}

/** Render an MCP arg value in a compact, readable form. Strings are
 *  quoted so empty strings are visible; numbers/booleans show as-is;
 *  nested objects/arrays render as compact JSON (with a tooltip showing
 *  the full string via the parent's `title` attribute). */
function formatArgValue(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "string") return `"${v}"`;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// --------------------------------------------------------------------------- //
// Memories inside memory_search step
// --------------------------------------------------------------------------- //

function MemoryHitsList({ hits }: { hits: MemoryHit[] }) {
  return (
    <ul className="space-y-1.5 py-1.5">
      {hits.map((h, i) => (
        // eslint-disable-next-line react/no-array-index-key
        <li key={`${h.kind}-${i}`} className="flex items-start gap-2">
          <KindBadge kind={h.kind} />
          <div className="min-w-0 flex-1">
            <p className="break-words text-[11.5px] leading-relaxed text-foreground/85">
              {h.text}
            </p>
            <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px] text-muted-foreground">
              {/*
                Score chip only for `mem` hits — graph/promoted come from
                Mem0's relations array and don't carry a similarity score.
                The kind badge to the left already tells the user that's
                what they are, so we'd rather omit the chip than render
                "no score".
              */}
              {typeof h.score === "number" ? <MemoryScoreChip score={h.score} /> : null}
              {typeof h.char_count === "number" && h.char_count > 0 ? (
                <span>{h.char_count.toLocaleString()} chars</span>
              ) : null}
            </div>
          </div>
        </li>
      ))}
    </ul>
  );
}

function KindBadge({ kind }: { kind: MemoryHit["kind"] }) {
  // Colours match the canonical Meko accents: mem/promoted purple-ish,
  // graph blue-ish so the three sources are visually distinct.
  const palette: Record<MemoryHit["kind"], { bg: string; text: string; label: string }> = {
    mem: { bg: "bg-purple-500/15", text: "text-purple-200", label: "mem" },
    graph: { bg: "bg-blue-500/15", text: "text-blue-200", label: "graph" },
    promoted: { bg: "bg-amber-500/15", text: "text-amber-200", label: "promoted" },
  };
  const p = palette[kind];
  return (
    <span
      className={cn(
        "mt-0.5 inline-flex shrink-0 rounded px-1.5 py-0.5 font-mono text-[9px] uppercase tracking-wider",
        p.bg,
        p.text,
      )}
    >
      {p.label}
    </span>
  );
}

// --------------------------------------------------------------------------- //
// KB grouped by document, each doc independently collapsible
// --------------------------------------------------------------------------- //

type KbGroup = {
  documentId: string;
  chunks: KbHit[];
};

function groupKbHitsByDocument(hits: KbHit[]): KbGroup[] {
  const order: string[] = [];
  const map = new Map<string, KbHit[]>();
  for (const h of hits) {
    const key = h.document_id || "(unknown)";
    if (!map.has(key)) {
      order.push(key);
      map.set(key, []);
    }
    map.get(key)!.push(h);
  }
  return order.map((documentId) => ({
    documentId,
    chunks: map.get(documentId) ?? [],
  }));
}

function KbGroupsList({ groups }: { groups: KbGroup[] }) {
  return (
    <ul className="space-y-1.5 py-1.5">
      {groups.map((g) => (
        <li key={g.documentId}>
          <KbDocCard group={g} />
        </li>
      ))}
    </ul>
  );
}

function KbDocCard({ group }: { group: KbGroup }) {
  // Each document is independently collapsible — small KBs may have
  // one doc with many chunks; large ones may have many docs with one
  // chunk each. Collapsible-per-doc handles both gracefully.
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="rounded-md border border-emerald-500/15 bg-emerald-500/[0.04]">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full items-center gap-2 px-2.5 py-1.5 text-left transition-colors hover:bg-emerald-500/[0.08]"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />
        )}
        <FileText className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />
        <code
          className="min-w-0 flex-1 truncate font-mono text-[10px] text-emerald-200/90"
          title={group.documentId}
        >
          {group.documentId}
        </code>
        <span className="shrink-0 rounded-full bg-emerald-500/15 px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-emerald-200">
          {group.chunks.length} {group.chunks.length === 1 ? "chunk" : "chunks"}
        </span>
      </button>
      {expanded ? (
        <ul className="space-y-1.5 border-t border-emerald-500/15 px-2.5 py-2">
          {group.chunks.map((c, i) => (
            // eslint-disable-next-line react/no-array-index-key
            <li key={i} className="flex items-start gap-2">
              <CornerDownRight
                className="mt-1 h-2.5 w-2.5 shrink-0 text-emerald-500/60"
                aria-hidden
              />
              <div className="min-w-0 flex-1">
                <p className="break-words text-[11.5px] leading-relaxed text-foreground/85 whitespace-pre-wrap line-clamp-4">
                  {c.chunk_text}
                </p>
                <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px] text-muted-foreground">
                  {typeof c.distance === "number" ? (
                    <KbDistanceChip distance={c.distance} />
                  ) : null}
                  {typeof c.char_count === "number" && c.char_count > 0 ? (
                    <span>{c.char_count.toLocaleString()} chars</span>
                  ) : null}
                </div>
              </div>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

// --------------------------------------------------------------------------- //
// Shared bits
// --------------------------------------------------------------------------- //

function StatusIcon({ status }: { status: ProcessingStep["status"] }) {
  if (status === "ok") {
    return <Check className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />;
  }
  if (status === "error") {
    return <XIcon className="h-3 w-3 shrink-0 text-red-400" aria-hidden />;
  }
  return (
    <MinusCircle
      className="h-3 w-3 shrink-0 text-muted-foreground/60"
      aria-hidden
    />
  );
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms.toLocaleString()}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

/** Trim a query string to a max length for the inline chip; full string
 *  always lives on the `title` attribute so users can hover to see it. */
function truncateQuery(query: string, max: number): string {
  const trimmed = query.trim().replace(/\s+/g, " ");
  if (trimmed.length <= max) return trimmed;
  return `${trimmed.slice(0, max - 1)}…`;
}

/**
 * Memory hits: Mem0's native cosine-similarity score in [0..1].
 * Higher = closer match. Buckets: ≥0.7 strong, ≥0.4 marginal, <0.4 weak.
 */
function MemoryScoreChip({ score }: { score: number }) {
  const tone =
    score >= 0.7
      ? "bg-emerald-500/15 text-emerald-300"
      : score >= 0.4
        ? "bg-amber-500/15 text-amber-300"
        : "bg-muted/40 text-muted-foreground";
  return (
    <span
      className={cn(
        "rounded px-1 py-0.5 font-mono text-[10px] tabular-nums",
        tone,
      )}
      title="Cosine similarity score from Mem0 (0–1, higher = closer match)."
    >
      score {score.toFixed(2)}
    </span>
  );
}

/**
 * KB hits: raw pgvector cosine distance in [0..2]. Lower = closer match.
 * Buckets calibrated to Meko's empirical distribution — relevant matches
 * cluster near 1.0 here, not 0.0–0.3 like a textbook example.
 *   < 0.7  → green (rare; very strong)
 *   < 1.0  → amber (realistic "good" band for this KB)
 *   ≥ 1.0  → grey  (orthogonal-ish; passes 1.2 threshold but weak)
 */
function KbDistanceChip({ distance }: { distance: number }) {
  const tone =
    distance < 0.7
      ? "bg-emerald-500/15 text-emerald-300"
      : distance < 1.0
        ? "bg-amber-500/15 text-amber-300"
        : "bg-muted/40 text-muted-foreground";
  return (
    <span
      className={cn(
        "rounded px-1 py-0.5 font-mono text-[10px] tabular-nums",
        tone,
      )}
      title="Cosine distance from pgvector (lower = closer match). Meko's KB embeddings cluster near 1.0 even for relevant chunks — compare relative distance between hits, not the absolute number."
    >
      dist {distance.toFixed(3)}
    </span>
  );
}
