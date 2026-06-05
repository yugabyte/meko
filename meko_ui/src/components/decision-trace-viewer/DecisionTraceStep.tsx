// One row inside a phase accordion — represents a single Langfuse
// observation (memory.search, llm.completion, etc.). Collapsed by
// default; expanding shows PARAMS / STATS sections and a footer that
// deep-links the row's `span_id` back into Observe Hub.

import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight, ExternalLink } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import type { ProcessingStep } from "@/lib/inferenceGateway";
import {
  ACCENT_BADGE,
  ACCENT_BORDER,
  ACCENT_TEXT,
  type AccentKey,
} from "./PhaseAccordion";
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
}: {
  step: ProcessingStep;
  accent: AccentKey;
  conversationId: string;
  langfuseProjectId?: string;
}) {
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
            <KvBlock title="PARAMS" entries={paramsObj} accent={accent} />
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

          {step.span_id ? (
            <div className="flex items-center justify-between border-t border-border/40 pt-2 text-[10.5px]">
              <span className="text-muted-foreground">
                span_id{" "}
                <code className="font-mono text-foreground/70">
                  {truncateMiddle(step.span_id, 16)}
                </code>
              </span>
              {traceUrl ? (
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
