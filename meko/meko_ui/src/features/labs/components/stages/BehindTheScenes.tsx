// Band 3 of a lesson: what Meko did with what the user just handed it, as a
// chain of icons. Shared by the Chapter 1 lessons that write into the
// datapack.
//
// Illustrative, NOT telemetry — it draws the pipeline, it does not measure it.
// The measured half is the right-hand pane, where the row this produced
// actually appears. Keeping that line clear matters: a diagram that animates
// like a live trace is read as one.

import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";
import { CONTEXT_TONE, type ContextConcept } from "@/lib/contextTones";

export type PipelineStep = {
  icon: LucideIcon;
  label: string;
  /** One short line under the label — the mechanism, not a sentence. */
  detail?: string;
};

export function BehindTheScenes({
  concept,
  steps,
  className,
}: {
  /** Colours the chain in the hue of whatever it produced. */
  concept: ContextConcept;
  steps: PipelineStep[];
  className?: string;
}) {
  const tone = CONTEXT_TONE[concept];
  return (
    <section className={cn("flex flex-col gap-2.5", className)}>
      <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        What happened behind the scenes
      </h3>
      <ol className="flex flex-wrap items-stretch gap-2">
        {steps.map((step, i) => {
          const Icon = step.icon;
          return (
            <li key={step.label} className="flex items-stretch gap-2">
              <div className="flex min-w-[8.5rem] flex-1 flex-col gap-1.5 rounded-md border border-border bg-card px-3 py-2.5">
                <span
                  className={cn("flex h-6 w-6 items-center justify-center rounded-md", tone.soft)}
                >
                  <Icon className={cn("h-3.5 w-3.5", tone.text)} aria-hidden />
                </span>
                <span className="text-xs font-medium text-foreground">{step.label}</span>
                {step.detail && (
                  <span className="text-[11px] leading-tight text-muted-foreground">
                    {step.detail}
                  </span>
                )}
              </div>
              {i < steps.length - 1 && (
                <span aria-hidden className="self-center text-muted-foreground">
                  →
                </span>
              )}
            </li>
          );
        })}
      </ol>
    </section>
  );
}
