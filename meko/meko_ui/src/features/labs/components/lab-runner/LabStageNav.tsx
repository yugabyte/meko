// LEFT pane of the lab runner: overall progress, then every chapter and its
// steps. The rail IS the navigation -- there is no separate chapter control,
// because each chapter's steps are always on screen.

import { Progress } from "@/components/ui/progress";

import type { useLabProgress } from "../../hooks/useLabProgress";
import type { LabChapter, LabStageId } from "../../types";
import { LabRailChapter, type LabRailChapterState } from "./LabRailChapter";

export function LabStageNav({
  chapters,
  progress,
}: {
  chapters: LabChapter[];
  progress: ReturnType<typeof useLabProgress>;
}) {
  const {
    chapterIndex,
    currentStage,
    completedStages,
    orderedStages,
    flatIndex,
    furthestFlatIndex,
    goToStageById,
  } = progress;

  // Steps completed, not steps visited -- the bar should reflect work done,
  // so clicking back through finished lessons never walks it backwards.
  const completedCount = orderedStages.filter((stage) => completedStages.has(stage.id)).length;
  const percent = Math.round((completedCount / orderedStages.length) * 100);

  const isStageReachable = (stageId: LabStageId) =>
    orderedStages.findIndex((stage) => stage.id === stageId) <= furthestFlatIndex;

  return (
    <nav
      aria-label="Lab steps"
      className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto border-r border-border bg-muted/20 p-4"
    >
      <div className="flex flex-col gap-2">
        <div className="flex items-baseline justify-between text-xs text-muted-foreground">
          <span className="font-medium uppercase tracking-wide">Progress</span>
          <span className="tabular-nums">
            {completedCount} of {orderedStages.length}
          </span>
        </div>
        <Progress value={percent} className="h-1" />
      </div>

      <div className="flex flex-col gap-3">
        {chapters.map((chapter, index) => {
          const state: LabRailChapterState =
            chapter.stages.every((stage) => completedStages.has(stage.id))
              ? "done"
              : index === chapterIndex
                ? "active"
                : "todo";
          return (
            <LabRailChapter
              key={chapter.id}
              chapter={chapter}
              number={index + 1}
              state={state}
              currentStageId={currentStage.id}
              completedStages={completedStages}
              isStageReachable={isStageReachable}
              onSelectStage={goToStageById}
            />
          );
        })}
      </div>

      <p className="mt-auto pt-2 text-xs text-muted-foreground">
        Step {flatIndex + 1} of {orderedStages.length}
      </p>
    </nav>
  );
}
