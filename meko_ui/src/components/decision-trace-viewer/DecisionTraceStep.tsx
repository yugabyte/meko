// One row inside a phase accordion — represents a single Langfuse
// observation (memory.search, llm.completion, etc.). Collapsed by
// default; expanding shows PARAMS / STATS sections and a footer that
// deep-links the row's `span_id` back into Observe Hub.

import { useMemo, useState } from "react";
import { ChartSpline, ChevronDown, ChevronRight, ExternalLink, FileText, Sparkles } from "lucide-react";
import { ROUTES, datapackConversationsPath } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import type { KbHit, MemoryHit, ProcessingStep } from "@/lib/inferenceGateway";
import {
  ACCENT_BADGE,
  ACCENT_BORDER,
  ACCENT_TEXT,
  type AccentKey,
} from "./PhaseAccordion";
import { useDecisionTraceContext } from "./decisionTraceContext";
import {
  formatDuration,
  prettifyStepName,
  statusTextColor,
  stepKindLabel,
  stepSummary,
  truncateMiddle,
} from "./helpers";

export function DecisionTraceStep({
  step,
  accent,
  conversationId,
  langfuseProjectId,
  memoryHits,
  kbHits,
}: {
  step: ProcessingStep;
  accent: AccentKey;
  conversationId: string;
  langfuseProjectId?: string;
  /** Per-turn retrieval hits. The matching arrays are rendered as an
   *  expandable "matched chunks/memories" list on the retrieval step they
   *  belong to (memory.search → memoryHits, knowledge.retrieve → kbHits). */
  memoryHits?: MemoryHit[];
  kbHits?: KbHit[];
}) {
  // Card-wide config (datapack/user labelling, deep-link toggle, view-trace
  // handler) comes from context rather than being drilled through the tree.
  const { onViewTrace } = useDecisionTraceContext();
  const [open, setOpen] = useState(false);
  const op = prettifyStepName(step.name);
  const kind = stepKindLabel(step.name);
  const summary = stepSummary(step);

  const paramsObj = useMemo(() => {
    const out: Record<string, unknown> = {};
    if (step.query) out.query = step.query;
    if (step.tool_args) Object.assign(out, step.tool_args);
    return out;
  }, [step.query, step.tool_args]);
  const hasParams = Object.keys(paramsObj).length > 0;

  const subTimings = step.sub_timings ?? {};
  const subTimingEntries = Object.entries(subTimings).filter(([, v]) => v != null);
  const hasSubTimings = subTimingEntries.length > 0;

  // Retrieval hits belonging to this step. memory.search shows its memory
  // hits; knowledge.retrieve shows its KB chunks (grouped by document).
  const matchedMemory = step.name === "memory_search" ? memoryHits ?? [] : [];
  const matchedKb = step.name === "knowledgebase_search" ? kbHits ?? [] : [];

  const traceUrl =
    step.span_id && langfuseProjectId && conversationId
      ? `${ROUTES.observeHub}?${new URLSearchParams({
          project: langfuseProjectId,
          session: conversationId.replace(/-/g, ""),
        }).toString()}#${step.span_id}`
      : "";

  return (
    <div className={cn("rounded-md border bg-background/30", ACCENT_BORDER[accent])}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full cursor-pointer items-center gap-2 px-2.5 py-1.5 text-left transition-colors hover:bg-background/60"
        aria-expanded={open}
      >
        {open ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-muted-foreground" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-muted-foreground" aria-hidden />
        )}
        {kind ? (
          <span className={cn(
            "shrink-0 rounded border px-1.5 py-[1px] font-mono text-[9.5px] uppercase tracking-wider",
            ACCENT_BADGE[accent],
          )}>
            {kind}
          </span>
        ) : null}
        <code className={cn("shrink-0 font-mono text-[11.5px]", statusTextColor(step.status))}>
          {op}
        </code>
        {summary ? (
          <span className="min-w-0 truncate text-[11px] text-muted-foreground">
            {summary}
          </span>
        ) : null}
        {step.duration_ms > 0 ? (
          <span className="ml-auto shrink-0 tabular-nums text-[11px] text-muted-foreground">
            {formatDuration(step.duration_ms)}
          </span>
        ) : null}
      </button>

      {open ? (
        <div className="border-t border-border/40 px-3 py-2 space-y-2 text-[11.5px]">
          {hasParams ? (
            <KvBlock
              title="PARAMS"
              entries={paramsObj}
              accent={accent}
            />
          ) : null}

          {hasSubTimings || step.detail ? (
            <div className="space-y-1">
              <div className={cn("text-[10px] uppercase tracking-wider", ACCENT_TEXT[accent])}>
                STATS
              </div>
              <div className="flex flex-wrap items-center gap-x-4 gap-y-1 font-mono">
                {step.detail ? (
                  <span className="text-muted-foreground">
                    results: <span className="text-foreground/90">{step.detail}</span>
                  </span>
                ) : null}
                {subTimingEntries.map(([key, value]) => (
                  <span key={key} className="text-muted-foreground">
                    {key}: <span className="text-foreground/90 tabular-nums">{value}</span>
                  </span>
                ))}
              </div>
            </div>
          ) : null}

          {matchedMemory.length > 0 || matchedKb.length > 0 ? (
            <MatchedHits accent={accent} memoryHits={matchedMemory} kbHits={matchedKb} />
          ) : null}

          {step.span_id ? (
            <div className="flex items-center justify-between border-t border-border/40 pt-2 text-[10.5px]">
              <span className="text-muted-foreground">
                span_id{" "}
                <code className="font-mono text-foreground/70">
                  {truncateMiddle(step.span_id, 16)}
                </code>
              </span>
              {onViewTrace ? (
                <button
                  type="button"
                  onClick={() => onViewTrace(step.span_id!)}
                  className="inline-flex items-center gap-1 text-purple-700 dark:text-purple-300 hover:text-purple-800 dark:hover:text-purple-200 transition-colors"
                  title="Open this step in the trace panel (shows the query and its hits)"
                >
                  <ChartSpline className="h-2.5 w-2.5" aria-hidden />
                  View Trace
                </button>
              ) : traceUrl ? (
                <a
                  href={traceUrl}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="inline-flex items-center gap-1 text-purple-700 dark:text-purple-300 hover:text-purple-800 dark:hover:text-purple-200 transition-colors"
                  title="Jump to this span in Observe Hub (new tab)"
                >
                  <ExternalLink className="h-2.5 w-2.5" aria-hidden />
                  Open in Langfuse
                </a>
              ) : null}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/** Expandable list of retrieval hits for a memory.search / knowledge.retrieve
 *  step. Collapsed by default behind a "See all N matched chunks/memories"
 *  toggle; KB chunks are grouped by their source document. */
function MatchedHits({
  accent,
  memoryHits,
  kbHits,
}: {
  accent: AccentKey;
  memoryHits: MemoryHit[];
  kbHits: KbHit[];
}) {
  const [open, setOpen] = useState(false);
  const isKb = kbHits.length > 0;
  const total = isKb ? kbHits.length : memoryHits.length;
  const noun = isKb
    ? total === 1 ? "matched chunk" : "matched chunks"
    : total === 1 ? "matched memory" : "matched memories";

  // Split KB hits: document chunks (grouped by source document) vs.
  // promoted-memory hits (memories elevated into the KB — shown together
  // under one "Promoted memory" group, not as fake documents).
  const { docGroups, promotedHits } = useMemo(() => {
    const m = new Map<string, KbHit[]>();
    const promoted: KbHit[] = [];
    for (const h of kbHits ?? []) {
      if (h.source === "memory") {
        promoted.push(h);
        continue;
      }
      const arr = m.get(h.document_id) ?? [];
      arr.push(h);
      m.set(h.document_id, arr);
    }
    return { docGroups: Array.from(m.entries()), promotedHits: promoted };
  }, [kbHits]);

  return (
    <div className="space-y-1.5 border-t border-border/40 pt-2">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={cn(
          "inline-flex items-center gap-1 text-[10.5px] font-medium transition-colors hover:opacity-80",
          ACCENT_TEXT[accent],
        )}
        aria-expanded={open}
      >
        {open ? (
          <ChevronDown className="h-3 w-3" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3" aria-hidden />
        )}
        {open ? `Hide ${noun}` : `See all ${total} ${noun}`}
      </button>

      {open ? (
        <div className="space-y-2">
          {isKb ? (
            <>
              {docGroups.map(([docId, chunks]) => {
                const docName = chunks[0]?.document_name ?? "";
                return (
                  <div
                    key={docId}
                    className="overflow-hidden rounded-md border border-border/50 bg-background/30"
                  >
                    <div className="flex items-center gap-1.5 border-b border-border/40 bg-muted/30 px-2.5 py-1.5">
                      <FileText className={cn("h-3.5 w-3.5 shrink-0", ACCENT_TEXT[accent])} aria-hidden />
                      <div className="min-w-0 flex-1">
                        <div className="truncate text-[11px] font-medium text-foreground/90" title={docName || docId}>
                          {docName || docId}
                        </div>
                        {docName ? (
                          <div className="break-all font-mono text-[9.5px] text-muted-foreground">
                            {docId}
                          </div>
                        ) : null}
                      </div>
                      <span className="ml-auto shrink-0 rounded-full border border-border/50 px-1.5 py-[1px] text-[9.5px] tabular-nums text-muted-foreground">
                        {chunks.length} chunk{chunks.length === 1 ? "" : "s"}
                      </span>
                    </div>
                    <ul className="space-y-1.5 px-2.5 py-2">
                      {chunks.map((c, i) => (
                        <HitRow key={i} text={c.chunk_text} score={c.score} accent={accent} />
                      ))}
                    </ul>
                  </div>
                );
              })}

              {promotedHits.length > 0 ? (
                <div className="overflow-hidden rounded-md border border-border/50 bg-background/30">
                  <div className="flex items-center gap-1.5 border-b border-border/40 bg-muted/30 px-2.5 py-1.5">
                    <Sparkles className={cn("h-3.5 w-3.5 shrink-0", ACCENT_TEXT[accent])} aria-hidden />
                    <span className="min-w-0 shrink truncate text-[11px] font-medium text-foreground/90">
                      Promoted memories
                    </span>
                    <span className="ml-auto shrink-0 rounded-full border border-border/50 px-1.5 py-[1px] text-[9.5px] tabular-nums text-muted-foreground">
                      {promotedHits.length} chunk{promotedHits.length === 1 ? "" : "s"}
                    </span>
                  </div>
                  <ul className="space-y-1.5 px-2.5 py-2">
                    {promotedHits.map((c, i) => (
                      <HitRow key={i} text={c.chunk_text} badge="promoted" accent={accent} />
                    ))}
                  </ul>
                </div>
              ) : null}
            </>
          ) : (
            <ul className="space-y-1.5">
              {memoryHits.map((h, i) => (
                <HitRow key={i} text={h.text} score={h.score} badge={h.kind} accent={accent} />
              ))}
            </ul>
          )}
        </div>
      ) : null}
    </div>
  );
}

function HitRow({
  text,
  badge,
  accent,
}: {
  text: string;
  // Relevance score is accepted but intentionally not displayed for now
  // (cosine-distance-derived values read misleadingly low).
  score?: number;
  badge?: string;
  accent: AccentKey;
}) {
  return (
    <li className="rounded border border-border/40 bg-background/40 px-2 py-1 text-[11px]">
      {badge ? (
        <div className="flex items-center gap-2">
          <span
            className={cn(
              "shrink-0 rounded border px-1 py-[1px] font-mono text-[8.5px] uppercase tracking-wider",
              ACCENT_BADGE[accent],
            )}
          >
            {badge}
          </span>
        </div>
      ) : null}
      <p className="whitespace-pre-wrap break-words text-foreground/90">{text}</p>
    </li>
  );
}

function KvBlock({
  title,
  entries,
  accent,
}: {
  title: string;
  entries: Record<string, unknown>;
  accent: AccentKey;
}) {
  return (
    <div className="space-y-1">
      <div className={cn("text-[10px] uppercase tracking-wider", ACCENT_TEXT[accent])}>
        {title}
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 font-mono">
        {Object.entries(entries).map(([k, v]) => (
          <KvRow key={k} k={k} v={v} />
        ))}
      </dl>
    </div>
  );
}

function KvRow({ k, v }: { k: string; v: unknown }) {
  // Card-wide labelling config is read from context (see DecisionTrace).
  const { datapackName, datapackId, userEmail, enableConversationLink } =
    useDecisionTraceContext();

  // A `datapack_id` param is shown as the datapack name when we know it
  // (and the id matches the datapack we're in, so we never mislabel a
  // cross-datapack id). Falls through to the raw value otherwise.
  if (
    k === "datapack_id" &&
    datapackName &&
    typeof v === "string" &&
    (!datapackId || datapackId === v)
  ) {
    return (
      <>
        <dt className="text-muted-foreground">datapack_name:</dt>
        <dd className="break-all text-foreground/90">{`"${datapackName}"`}</dd>
      </>
    );
  }

  // A `user_id` param is shown as the user's email when known.
  if (k === "user_id" && userEmail && typeof v === "string") {
    return (
      <>
        <dt className="text-muted-foreground">user:</dt>
        <dd className="break-all text-foreground/90">{`"${userEmail}"`}</dd>
      </>
    );
  }

  // A `conversation_id` param links to that conversation in the viewer
  // (new tab) when enabled. The viewer route expects the dash-less
  // session id form (matches how Collective Learnings builds its
  // conversation deep-links).
  const convId =
    k === "conversation_id" && typeof v === "string" ? v.replace(/-/g, "") : "";
  if (convId && datapackId && enableConversationLink) {
    const href = datapackConversationsPath(datapackId, convId);
    return (
      <>
        <dt className="text-muted-foreground">{k}:</dt>
        <dd className="break-all">
          <a
            href={href}
            target="_blank"
            rel="noreferrer noopener"
            className="inline-flex items-center gap-1 text-purple-700 dark:text-purple-300 hover:text-purple-800 dark:hover:text-purple-200 hover:underline transition-colors"
            title="Open this conversation (new tab)"
          >
            {`"${v}"`}
            <ExternalLink className="h-2.5 w-2.5 shrink-0" aria-hidden />
          </a>
        </dd>
      </>
    );
  }

  const display =
    typeof v === "string"
      ? `"${v}"`
      : typeof v === "number" || typeof v === "boolean" || v == null
        ? String(v)
        : JSON.stringify(v);
  return (
    <>
      <dt className="text-muted-foreground">{k}:</dt>
      <dd className="break-all text-foreground/90">{display}</dd>
    </>
  );
}
