// One step row in the lab runner's left rail: a state dot plus the stage
// title. Shared by every chapter group in LabStageNav.

import { Check } from "lucide-react";

import { cn } from "@/lib/utils";

export type LabRailStepState = "done" | "current" | "todo";

export function LabRailStep({
  title,
  number,
  state,
  disabled,
  onClick,
}: {
  title: string;
  /** Position within its own chapter, restarting at 1 each chapter. */
  number: number;
  state: LabRailStepState;
  /** True for stages the user hasn't reached yet — visible, but not a
   *  shortcut past the lesson in front of them. */
  disabled: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-current={state === "current" ? "step" : undefined}
      className={cn(
        "flex w-full items-center gap-2.5 rounded-md px-2 py-1.5 text-left text-sm transition-colors",
        state === "current" && "bg-primary/10 font-medium text-foreground",
        state !== "current" && !disabled && "text-muted-foreground hover:bg-muted hover:text-foreground",
        disabled && "cursor-default text-muted-foreground/50",
      )}
    >
      <span
        aria-hidden
        className={cn(
          "flex h-4 w-4 shrink-0 items-center justify-center rounded-full border text-[9px]",
          state === "done" && "border-primary bg-primary text-primary-foreground",
          state === "current" && "border-primary bg-background ring-2 ring-primary/30",
          state === "todo" && "border-border bg-background",
        )}
      >
        {state === "done" ? <Check className="h-2.5 w-2.5" strokeWidth={3} /> : null}
      </span>
      <span className="min-w-0 flex-1 truncate">
        <span className="tabular-nums text-muted-foreground">{number}.</span> {title}
      </span>
    </button>
  );
}
