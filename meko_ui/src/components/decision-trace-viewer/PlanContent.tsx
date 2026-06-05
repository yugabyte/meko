// Plan phase body — the only Decision Trace phase that doesn't render
// a step list. Reasoning prose and the structured plan checklist are
// two distinct fields on the same Langfuse Reasoning observation
// (`observation.output` vs `observation.metadata.plan`). They can land
// independently — a turn can have one, the other, both, or neither —
// so each pane gets its own header, source attribution, and empty
// state instead of being collapsed into one prose blob.
//
// `model` lives on the same observation but is rendered as a single-cell
// footer because sampling params and TTFT aren't hook-extractable from
// any supported host today (Claude Code transcripts carry `message.model`
// but not temperature / max_tokens / first-token timing).

import { Brain, Cpu, Hash, Workflow } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import {
  ACCENT_BADGE,
  ACCENT_BORDER,
  ACCENT_TEXT,
  type AccentKey,
} from "./PhaseAccordion";

export function PlanContent({
  reasoning,
  plan,
  model,
  accent,
}: {
  reasoning: string;
  plan: string[];
  model?: string;
  accent: AccentKey;
}) {
  const hasReasoning = reasoning.length > 0;
  const hasPlan = plan.length > 0;
  const hasModel = !!model;

  return (
    <div className="space-y-2">
      <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
        <PlanPane
          accent={accent}
          icon={<Brain className="h-3 w-3 text-purple-300" aria-hidden />}
          label="Agent reasoning"
          sourceField="reasoning"
          sourceStore="observation.output"
          emptyHint="No reasoning captured for this turn"
          hasContent={hasReasoning}
        >
          <div className={cn(
            "rounded-md border px-3 py-2 text-[12px] italic leading-relaxed whitespace-pre-wrap",
            ACCENT_BORDER[accent],
            "bg-background/40 text-foreground/90",
          )}>
            <MarkdownMessage text={reasoning} />
          </div>
        </PlanPane>

        <PlanPane
          accent={accent}
          icon={<Workflow className="h-3 w-3 text-purple-300" aria-hidden />}
          label="Planned execution"
          sourceField="plan"
          sourceStore="observation.metadata.plan"
          countBadge={hasPlan ? `${plan.length} ${plan.length === 1 ? "step" : "steps"}` : null}
          emptyHint="No plan recorded for this turn"
          hasContent={hasPlan}
        >
          <ol className={cn(
            "rounded-md border px-3 py-2 space-y-1.5",
            ACCENT_BORDER[accent],
            "bg-background/40",
          )}>
            {plan.map((step, i) => (
              <li
                key={`${i}-${step.slice(0, 16)}`}
                className="flex items-start gap-2.5 text-[12px] leading-relaxed text-foreground/90"
              >
                <span
                  className={cn(
                    "mt-[2px] inline-flex h-4 w-4 shrink-0 items-center justify-center rounded font-mono text-[9.5px] tabular-nums",
                    ACCENT_BADGE[accent],
                  )}
                >
                  {i + 1}
                </span>
                <span>{step}</span>
              </li>
            ))}
          </ol>
        </PlanPane>
      </div>

      {hasModel ? (
        <div
          className={cn(
            "flex items-center gap-2 rounded-md border px-3 py-2",
            ACCENT_BORDER[accent],
            "bg-background/40",
          )}
        >
          <Cpu className="h-3 w-3 text-purple-300" aria-hidden />
          <span className="text-[10px] uppercase tracking-wider text-muted-foreground">
            Model
          </span>
          <span className="font-mono text-[11px] text-foreground/90">
            {model}
          </span>
        </div>
      ) : null}
    </div>
  );
}

function PlanPane({
  accent,
  icon,
  label,
  sourceField,
  sourceStore,
  countBadge,
  emptyHint,
  hasContent,
  children,
}: {
  accent: AccentKey;
  icon: React.ReactNode;
  label: string;
  sourceField: string;
  sourceStore: string;
  countBadge?: string | null;
  emptyHint: string;
  hasContent: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("rounded-md border", ACCENT_BORDER[accent], "bg-background/20")}>
      <div className={cn(
        "flex flex-wrap items-center gap-2 border-b px-2.5 py-1.5",
        ACCENT_BORDER[accent],
      )}>
        {icon}
        <span className={cn("text-[10.5px] font-medium uppercase tracking-wide", ACCENT_TEXT[accent])}>
          {label}
        </span>
        {countBadge ? (
          <span className={cn(
            "rounded border px-1.5 py-px font-mono text-[9.5px] tabular-nums",
            ACCENT_BADGE[accent],
          )}>
            {countBadge}
          </span>
        ) : null}
        <span className="ml-auto inline-flex items-center gap-1 font-mono text-[9.5px] text-muted-foreground">
          <Hash className="h-2.5 w-2.5" aria-hidden />
          <span className="text-foreground/70">{sourceField}</span>
          <span className="text-muted-foreground/60">·</span>
          <span>{sourceStore}</span>
        </span>
      </div>
      <div className="px-2 py-2">
        {hasContent ? (
          children
        ) : (
          <div className="px-1 py-1 text-[11px] italic text-muted-foreground">
            {emptyHint}
          </div>
        )}
      </div>
    </div>
  );
}
