import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import type { useLabProgress } from "../../hooks/useLabProgress";
import type { LabStage } from "../../types";

/**
 * LEFT pane of the lab runner: a stacked-card list of the lab's 5 fixed
 * stages, styled after YugabyteDB Cloud's own lab walkthrough — the active
 * stage expands into a full card (heading + body + a Next button that
 * advances progress), every other stage collapses to a plain clickable
 * title pill. Both paths work: click any collapsed card to jump straight
 * to it, or use Next to move through them in order.
 */
export function LabStageNav({
  stages,
  progress,
}: {
  stages: LabStage[];
  progress: ReturnType<typeof useLabProgress>;
}) {
  const { stageIndex, goToStage, goToNext, isLastStage } = progress;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto border-r border-border bg-muted/20 p-4">
      {stages.map((stage, index) => {
        const isCurrent = index === stageIndex;

        if (isCurrent) {
          return (
            <div key={stage.id} className="rounded-xl bg-card p-4 shadow-md">
              <span className="inline-flex items-center rounded-full bg-primary/10 px-3 py-1.5 text-sm font-semibold text-primary">
                Step {index + 1}. {stage.title}
              </span>
              <p className="mt-3 text-sm text-muted-foreground">{stage.summary}</p>
              <div className="mt-4 flex justify-end">
                <Button size="sm" onClick={goToNext} disabled={isLastStage}>
                  {isLastStage ? "Done" : "Next"}
                </Button>
              </div>
            </div>
          );
        }

        return (
          <button
            key={stage.id}
            type="button"
            onClick={() => goToStage(index)}
            className={cn(
              "rounded-xl bg-card p-4 text-left shadow-sm transition-shadow hover:shadow-md",
            )}
          >
            <span className="inline-flex items-center rounded-full bg-muted px-3 py-1.5 text-sm font-semibold text-muted-foreground">
              Step {index + 1}. {stage.title}
            </span>
          </button>
        );
      })}
    </div>
  );
}
