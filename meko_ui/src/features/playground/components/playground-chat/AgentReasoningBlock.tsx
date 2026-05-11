// Collapsible "Agent reasoning" panel rendered above each assistant
// bubble. Three sections, top to bottom:
//
//   1. Memories used   — grouped by [mem|graph|promoted], each with a
//                        score chip and char count.
//   2. Knowledge used  — grouped by document_id (a chunk-by-chunk dump
//                        keeps repeating the same UUID; one header per
//                        document keeps the panel scannable), each chunk
//                        with score + char count.
//   3. Steps            — the per-turn timeline (conversation_create,
//                        memory_search, knowledgebase_search, llm_invoke,
//                        conversation_add_message, memory_add). Lightweight
//                        observability — for the full picture the user
//                        clicks "View trace" instead.
//
// Collapsed by default per spec; counts in the header give a quick
// signal so the user can decide whether to expand.

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
  const [expanded, setExpanded] = useState(false);

  // Group KB chunks by document_id so each document shows up once with
  // its chunks nested. Preserves the order chunks arrived in (top-K
  // sorted by relevance) — keeps the strongest hit per document first.
  const kbGroups = useMemo(() => groupKbHitsByDocument(kbHits), [kbHits]);

  const memoryCount = memoryHits.length;
  const kbChunkCount = kbHits.length;
  const kbDocCount = kbGroups.length;
  const stepCount = processingSteps.length;

  // Nothing to show → hide the entire panel. A bubble with zero hits and
  // zero steps is a degenerate case (e.g. greetings with retrieval
  // skipped AND no step list — shouldn't happen but defensive).
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
          Agent reasoning
        </span>
        {summary ? (
          <span className="text-[11px] text-muted-foreground">· {summary}</span>
        ) : null}
      </button>
      {expanded ? (
        <div className="space-y-4 border-t border-purple-500/15 px-3 py-3">
          {memoryCount > 0 ? (
            <MemorySection hits={memoryHits} />
          ) : null}
          {kbChunkCount > 0 ? (
            <KbSection groups={kbGroups} />
          ) : null}
          {stepCount > 0 ? (
            <StepsSection steps={processingSteps} />
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// --------------------------------------------------------------------------- //
// Memories
// --------------------------------------------------------------------------- //

function MemorySection({ hits }: { hits: MemoryHit[] }) {
  return (
    <div>
      <SectionHeader
        icon={<Sparkles className="h-3 w-3 text-violet-400" aria-hidden />}
        label={`Memories used (${hits.length})`}
        accent="text-purple-200/80"
      />
      <ul className="space-y-1.5">
        {hits.map((h, i) => (
          // Index suffix because two graph triples could repeat as text.
          // eslint-disable-next-line react/no-array-index-key
          <li key={`${h.kind}-${i}`}>
            <div className="flex items-start gap-2">
              <KindBadge kind={h.kind} />
              <div className="min-w-0 flex-1">
                <p className="break-words text-[12px] leading-relaxed text-foreground/85">
                  {h.text}
                </p>
                <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px] text-muted-foreground">
                  {/*
                    Score chip is only rendered for `mem` hits — `graph`
                    and `promoted` come from Mem0's relations array and
                    don't carry a similarity score. The kind badge to
                    the left already tells the user that's what they are,
                    so we'd rather omit the chip than render "no score".
                    Memory uses Mem0's native `score` (cosine similarity,
                    0..1, higher = better) — same scale Mem0 logs into
                    its own traces, so the number lines up if you cross-
                    reference the Langfuse view.
                  */}
                  {typeof h.score === "number" ? (
                    <MemoryScoreChip score={h.score} />
                  ) : null}
                  {typeof h.char_count === "number" && h.char_count > 0 ? (
                    <span>{h.char_count.toLocaleString()} chars</span>
                  ) : null}
                </div>
              </div>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}

function KindBadge({ kind }: { kind: MemoryHit["kind"] }) {
  // Colours match the canonical Meko accents: mem/promoted purple-ish,
  // graph blue-ish so the three sources are visually distinct.
  const palette: Record<MemoryHit["kind"], { bg: string; text: string; label: string }> = {
    mem: {
      bg: "bg-purple-500/15",
      text: "text-purple-200",
      label: "mem",
    },
    graph: {
      bg: "bg-blue-500/15",
      text: "text-blue-200",
      label: "graph",
    },
    promoted: {
      bg: "bg-amber-500/15",
      text: "text-amber-200",
      label: "promoted",
    },
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
// Knowledge — grouped by document_id
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

function KbSection({ groups }: { groups: KbGroup[] }) {
  return (
    <div>
      <SectionHeader
        icon={<FileText className="h-3 w-3 text-emerald-400" aria-hidden />}
        label={`Knowledge used (${groups.reduce((n, g) => n + g.chunks.length, 0)})`}
        accent="text-emerald-200/80"
      />
      <ul className="space-y-2.5">
        {groups.map((g) => (
          <li key={g.documentId} className="rounded-md border border-emerald-500/15 bg-emerald-500/[0.04] px-2.5 py-2">
            <div className="mb-1.5 flex items-center gap-2">
              <FileText className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />
              <code
                className="min-w-0 flex-1 truncate font-mono text-[10px] text-emerald-200/90"
                title={g.documentId}
              >
                {g.documentId}
              </code>
              <span className="shrink-0 rounded-full bg-emerald-500/15 px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-emerald-200">
                {g.chunks.length} {g.chunks.length === 1 ? "chunk" : "chunks"}
              </span>
            </div>
            <ul className="space-y-1.5 pl-1">
              {g.chunks.map((c, i) => (
                <li
                  // eslint-disable-next-line react/no-array-index-key
                  key={i}
                  className="flex items-start gap-2"
                >
                  <CornerDownRight
                    className="mt-1 h-2.5 w-2.5 shrink-0 text-emerald-500/60"
                    aria-hidden
                  />
                  <div className="min-w-0 flex-1">
                    <p className="break-words text-[11.5px] leading-relaxed text-foreground/85 whitespace-pre-wrap line-clamp-4">
                      {c.chunk_text}
                    </p>
                    <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[10px] text-muted-foreground">
                      {/*
                        KB hits surface the raw pgvector distance — same
                        number the trace drawer / DB logs show, no rescaling
                        and no "match %" editorializing. Lower = better.
                      */}
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
          </li>
        ))}
      </ul>
    </div>
  );
}

// --------------------------------------------------------------------------- //
// Steps timeline
// --------------------------------------------------------------------------- //

function StepsSection({ steps }: { steps: ProcessingStep[] }) {
  return (
    <div>
      <SectionHeader
        icon={<Clock className="h-3 w-3 text-sky-300" aria-hidden />}
        label="Steps"
        accent="text-sky-200/80"
      />
      <ol className="space-y-1">
        {steps.map((s, i) => (
          // eslint-disable-next-line react/no-array-index-key
          <li key={`${s.name}-${i}`} className="flex items-center gap-2 text-[11px]">
            <StatusIcon status={s.status} />
            <code className="min-w-0 flex-1 truncate font-mono text-foreground/85">
              {s.name}
            </code>
            {s.duration_ms > 0 ? (
              <span className="shrink-0 tabular-nums text-muted-foreground">
                {formatDuration(s.duration_ms)}
              </span>
            ) : null}
            {s.detail ? (
              <span
                className="shrink-0 max-w-[160px] truncate text-muted-foreground/70"
                title={s.detail}
              >
                — {s.detail}
              </span>
            ) : null}
          </li>
        ))}
      </ol>
    </div>
  );
}

function StatusIcon({ status }: { status: ProcessingStep["status"] }) {
  if (status === "ok") {
    return <Check className="h-3 w-3 shrink-0 text-emerald-400" aria-hidden />;
  }
  if (status === "error") {
    return <XIcon className="h-3 w-3 shrink-0 text-red-400" aria-hidden />;
  }
  // skipped
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

// --------------------------------------------------------------------------- //
// Shared
// --------------------------------------------------------------------------- //

function SectionHeader({
  icon,
  label,
  accent,
}: {
  icon: React.ReactNode;
  label: string;
  accent: string;
}) {
  return (
    <div
      className={cn(
        "mb-1.5 inline-flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider",
        accent,
      )}
    >
      {icon}
      {label}
    </div>
  );
}

/**
 * Memory hits: Mem0's native cosine-similarity score in [0..1].
 * Higher = closer match. Buckets: ≥0.7 strong, ≥0.4 marginal, else weak.
 * The `MEMORY_MIN_SCORE = 0.3` gateway threshold means the weakest hits
 * we surface land in the 30–40% band.
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
 *
 * Buckets are calibrated to Meko's empirical KB distribution rather than
 * the textbook cosine ranges, because the embedding model produces
 * under-spread distances — genuine matches commonly sit in the 0.8–1.0
 * band here, not 0.0–0.3 like a textbook example would suggest. So:
 *   < 0.7  → green   (rare; very strong)
 *   < 1.0  → amber   (the realistic "good" band for this KB)
 *   ≥ 1.0  → grey    (orthogonal-ish; we still show because the
 *                     KB_MAX_DISTANCE threshold permits up to 1.2)
 *
 * Tooltip explains the "lower = better" convention so users don't have
 * to remember it.
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
      title="Cosine distance from pgvector (lower = closer match). 0 = identical, 1 = unrelated. Meko's KB embeddings tend to cluster near 1.0 even for relevant chunks — compare relative distance between hits, not the absolute number."
    >
      dist {distance.toFixed(3)}
    </span>
  );
}
