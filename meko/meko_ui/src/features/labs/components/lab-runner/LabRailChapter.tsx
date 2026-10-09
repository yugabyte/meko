// One chapter group in the lab runner's left rail: a numbered header that
// turns into a check once every stage in it is done, then its own steps.

import { Check } from "lucide-react";

import { cn } from "@/lib/utils";

import type { LabChapter, LabStageId } from "../../types";
import { LabRailStep, type LabRailStepState } from "./LabRailStep";

export type LabRailChapterState = "done" | "active" | "todo";

export function LabRailChapter({
  chapter,
  number,
  state,
  currentStageId,
  completedStages,
  isStageReachable,
  onSelectStage,
}: {
  chapter: LabChapter;
  number: number;
  state: LabRailChapterState;
  currentStageId: LabStageId;
  completedStages: ReadonlySet<LabStageId>;
  /** Whether the user has got far enough for this stage to be selectable. */
  isStageReachable: (stageId: LabStageId) => boolean;
  onSelectStage: (stageId: LabStageId) => void;
}) {
  return (
    <div className={cn("flex flex-col gap-0.5", state === "todo" && "opacity-60")}>
      <div className="flex items-center gap-2.5 px-2 py-1.5">
        <span
          aria-hidden
          className={cn(
            "flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold",
            state === "done" && "bg-primary text-primary-foreground",
            state === "active" && "bg-primary/15 text-primary",
            state === "todo" && "bg-muted text-muted-foreground",
          )}
        >
          {state === "done" ? <Check className="h-3 w-3" strokeWidth={3} /> : number}
        </span>
        <h2
          className={cn(
            "min-w-0 flex-1 truncate text-sm font-semibold",
            state === "active" ? "text-foreground" : "text-muted-foreground",
          )}
        >
          {chapter.title}
        </h2>
      </div>

      <div className="flex flex-col gap-0.5 pl-3.5">
        {chapter.stages.map((stage, index) => {
          const stepState: LabRailStepState =
            stage.id === currentStageId
              ? "current"
              : completedStages.has(stage.id)
                ? "done"
                : "todo";
          return (
            <LabRailStep
              key={stage.id}
              title={stage.title}
              number={index + 1}
              state={stepState}
              disabled={stage.id !== currentStageId && !isStageReachable(stage.id)}
              onClick={() => onSelectStage(stage.id)}
            />
          );
        })}
      </div>
    </div>
  );
}
