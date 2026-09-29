// Spec → Build → Review → Fix & merge, with the stages that have no lab yet
// dimmed. Sits on a lab's landing page to place it inside the wider lifecycle.

import { cn } from "@/lib/utils";

import { ADLC_STAGES } from "../data/adlcStages";

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
            {stage.live ? (
              // A ring that expands out of the dot and fades, so "live" reads
              // as a heartbeat rather than another static bullet. The solid
              // dot stays put underneath; only the ring moves, which keeps the
              // chip's text from shifting. Honours prefers-reduced-motion —
              // the dot alone still marks the stage.
              <span aria-hidden className="relative inline-flex h-1.5 w-1.5 shrink-0">
                <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary opacity-75 motion-reduce:animate-none" />
                <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-primary" />
              </span>
            ) : (
              <i aria-hidden className="h-1.5 w-1.5 shrink-0 rounded-full bg-muted-foreground/50" />
            )}
            {stage.label}
          </span>
          {index < ADLC_STAGES.length - 1 && (
            <span aria-hidden className="text-muted-foreground">
              →
            </span>
          )}
        </div>
      ))}
    </div>
  );
}
