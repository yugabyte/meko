// Generic accordion shell used for the three Decision Trace phases
// (Preprocess / Plan / Execute). Owns the accent palette so any
// child component that needs it can import the constants from here.
//
// Body is one of:
//   - explicit `children` (Plan phase passes its split-source PlanContent)
//   - the default: optional reasoning prose + a list of `DecisionTraceStep`
// Defaults to closed; each accordion carries its own `useState`, and the
// parent thread keys turns by `message_id`, so expanding a phase on one
// message does NOT leak into other messages.

import { useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import type { KbHit, MemoryHit, ProcessingStep } from "@/lib/inferenceGateway";
import { DecisionTraceStep } from "./DecisionTraceStep";
import { formatDuration } from "./helpers";

export type AccentKey = "blue" | "purple" | "emerald";

export const ACCENT_BORDER: Record<AccentKey, string> = {
  blue: "border-blue-500/20",
  purple: "border-purple-500/20",
  emerald: "border-emerald-500/20",
};
export const ACCENT_BG: Record<AccentKey, string> = {
  blue: "bg-blue-500/[0.03]",
  purple: "bg-purple-500/[0.03]",
  emerald: "bg-emerald-500/[0.03]",
};
export const ACCENT_TEXT: Record<AccentKey, string> = {
  blue: "text-blue-700 dark:text-blue-200/90",
  purple: "text-purple-700 dark:text-purple-200/90",
  emerald: "text-emerald-700 dark:text-emerald-200/90",
};
export const ACCENT_BADGE: Record<AccentKey, string> = {
  blue: "bg-blue-500/15 text-blue-700 border-blue-500/30 dark:text-blue-200",
  purple: "bg-purple-500/15 text-purple-700 border-purple-500/30 dark:text-purple-200",
  emerald: "bg-emerald-500/15 text-emerald-700 border-emerald-500/30 dark:text-emerald-200",
};

export function PhaseAccordion({
  number,
  label,
  accent,
  icon,
  steps,
  reasoning,
  taglineFallback,
  conversationId,
  langfuseProjectId,
  datapackName,
  datapackId,
  userEmail,
  enableConversationLink,
  onViewTrace,
  memoryHits,
  kbHits,
  children,
  hasContentOverride,
}: {
  number: number;
  label: string;
  accent: AccentKey;
  icon: React.ReactNode;
  steps: ProcessingStep[];
  reasoning?: string;
  taglineFallback: string;
  conversationId: string;
  langfuseProjectId?: string;
  datapackName?: string;
  datapackId?: string;
  userEmail?: string;
  enableConversationLink?: boolean;
  onViewTrace?: (spanId: string) => void;
  /** Per-turn retrieval hits, forwarded to retrieval steps so they can
   *  show their matched chunks/memories. */
  memoryHits?: MemoryHit[];
  kbHits?: KbHit[];
  /** When supplied, replaces the default reasoning + steps body.
   *  Used by the Plan phase to mount the split-source `PlanContent`. */
  children?: React.ReactNode;
  /** When supplied, overrides the default content-presence check.
   *  Pass `true` to force the body open even when neither `reasoning`
   *  nor `steps` are populated (e.g. when `children` carries the
   *  content). */
  hasContentOverride?: boolean;
}) {
  const [open, setOpen] = useState(false);

  const phaseDuration = steps.reduce((acc, s) => acc + (s.duration_ms ?? 0), 0);
  const hasContent =
    hasContentOverride !== undefined
      ? hasContentOverride
      : steps.length > 0 || (reasoning && reasoning.length > 0);

  return (
    <section className={cn("rounded-md border", ACCENT_BORDER[accent], ACCENT_BG[accent])}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full cursor-pointer items-center gap-2 px-3 py-1.5 text-left"
        aria-expanded={open}
      >
        <span
          className={cn(
            "inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-sm border text-[10px] font-medium tabular-nums",
            ACCENT_BADGE[accent],
          )}
        >
          {number}
        </span>
        {icon}
        <span className={cn("text-[11px] font-medium uppercase tracking-wide", ACCENT_TEXT[accent])}>
          {label}
        </span>
        {phaseDuration > 0 ? (
          <span className="text-[11px] text-muted-foreground tabular-nums">
            {formatDuration(phaseDuration)}
          </span>
        ) : null}
        <span className="ml-2 text-[11px] text-muted-foreground truncate">
          · {taglineFallback}
        </span>
        <span className="ml-auto">
          {open ? (
            <ChevronDown className="h-3 w-3 text-muted-foreground" aria-hidden />
          ) : (
            <ChevronRight className="h-3 w-3 text-muted-foreground" aria-hidden />
          )}
        </span>
      </button>

      {open && hasContent ? (
        <div className="border-t border-border/40 px-2 py-2 space-y-1.5">
          {children ? (
            children
          ) : (
            <>
              {reasoning ? (
                <div className={cn(
                  "rounded-md border px-3 py-2 text-[12px] italic leading-relaxed whitespace-pre-wrap",
                  ACCENT_BORDER[accent],
                  "bg-background/40 text-foreground/90",
                )}>
                  <MarkdownMessage text={reasoning} />
                </div>
              ) : null}
              {steps.map((step, i) => (
                <DecisionTraceStep
                  key={`${step.name}-${i}-${step.span_id ?? ""}`}
                  step={step}
                  accent={accent}
                  conversationId={conversationId}
                  langfuseProjectId={langfuseProjectId}
                  datapackName={datapackName}
                  datapackId={datapackId}
                  userEmail={userEmail}
                  enableConversationLink={enableConversationLink}
                  onViewTrace={onViewTrace}
                  memoryHits={memoryHits}
                  kbHits={kbHits}
                />
              ))}
            </>
          )}
        </div>
      ) : null}

      {open && !hasContent ? (
        <div className="border-t border-border/40 px-3 py-2 text-[11px] italic text-muted-foreground">
          (nothing recorded for this phase)
        </div>
      ) : null}
    </section>
  );
}
