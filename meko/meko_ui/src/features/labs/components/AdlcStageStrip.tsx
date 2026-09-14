// Spec → Build → Review → Fix & merge, with the stages that have no lab yet
// dimmed. Sits above the lab list to place Labs inside the wider lifecycle.

import { cn } from "@/lib/utils";

import { ADLC_STAGES, ADLC_STAGES_HINT } from "../data/adlcStages";

export function AdlcStageStrip() {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {ADLC_STAGES.map((stage, index) => (
        <div key={stage.label} className="flex items-center gap-2">
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs",
              stage.live
                ? "border-border bg-card text-foreground"
                : "border-dashed border-border text-muted-foreground",
            )}
          >
            <i
              aria-hidden
              className={cn(
                "h-1.5 w-1.5 rounded-full",
                stage.live ? "bg-primary" : "bg-muted-foreground/50",
              )}
            />
            {stage.label}
          </span>
          {index < ADLC_STAGES.length - 1 && (
            <span aria-hidden className="text-muted-foreground">
              →
            </span>
          )}
        </div>
      ))}
      <p className="ml-1 text-xs text-muted-foreground">{ADLC_STAGES_HINT}</p>
    </div>
  );
}
