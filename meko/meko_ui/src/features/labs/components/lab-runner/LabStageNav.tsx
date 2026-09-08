import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import type { useLabProgress } from "../../hooks/useLabProgress";
import type { LabChapter } from "../../types";

/**
 * LEFT pane of the lab runner, styled after YugabyteDB AEON's own lab
 * walkthrough: a persistent "Chapter N: <title>" header, then ONLY that
 * chapter's own stages (the active stage a full card with a Next button,
 * siblings plain clickable pills) -- other chapters are not shown at all,
 * not even collapsed. Stage numbering ("Step N.") restarts at 1 within each
 * chapter, since only one chapter's steps are ever visible at once.
 *
 * Moving between chapters is a deliberate, separate action (a "Next
 * Chapter" / "Previous chapter" control below the stage list) rather than
 * an implicit side effect of clicking the last stage's "Next" -- that
 * button now stops (reads "Done", disabled) at the end of the current
 * chapter instead of silently crossing into the next one.
 */
export function LabStageNav({
  chapters,
  progress,
}: {
  chapters: LabChapter[];
  progress: ReturnType<typeof useLabProgress>;
}) {
  const {
    chapterIndex,
    stageIndex,
    goToStage,
    goToNext,
    goToNextChapter,
    goToPreviousChapter,
    isLastStageInChapter,
    isLastChapter,
  } = progress;
  const chapter = chapters[chapterIndex];

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto border-r border-border bg-muted/20 p-4">
      <div className="flex items-center gap-2 rounded-xl bg-muted/60 px-3 py-2.5">
        <span aria-hidden>📖</span>
        <p className="text-sm">
          <span className="font-semibold text-foreground">Chapter {chapterIndex + 1}:</span>{" "}
          <span className="text-foreground">{chapter.title}</span>
        </p>
      </div>

      {chapter.stages.map((stage, sIdx) => {
        const isCurrent = sIdx === stageIndex;
        const number = sIdx + 1;

        if (isCurrent) {
          return (
            <div key={stage.id} className="rounded-xl bg-card p-4 shadow-md">
              <span className="inline-flex items-center rounded-full bg-primary/10 px-3 py-1.5 text-sm font-semibold text-primary">
                Step {number}. {stage.title}
              </span>
              <p className="mt-3 text-sm text-muted-foreground">{stage.summary}</p>
              <div className="mt-4 flex justify-end">
                <Button size="sm" onClick={goToNext} disabled={isLastStageInChapter}>
                  {isLastStageInChapter ? "Done" : "Next"}
                </Button>
              </div>
            </div>
          );
        }

        return (
          <button
            key={stage.id}
            type="button"
            onClick={() => goToStage(chapterIndex, sIdx)}
            className={cn(
              "rounded-xl bg-card p-4 text-left shadow-sm transition-shadow hover:shadow-md",
            )}
          >
            <span className="inline-flex items-center rounded-full bg-muted px-3 py-1.5 text-sm font-semibold text-muted-foreground">
              Step {number}. {stage.title}
            </span>
          </button>
        );
      })}

      {(chapterIndex > 0 || !isLastChapter) && (
        <div className="mt-2 flex items-center justify-between">
          {chapterIndex > 0 ? (
            <Button variant="ghost" size="sm" onClick={goToPreviousChapter}>
              ← Previous chapter
            </Button>
          ) : (
            <span />
          )}
          {!isLastChapter && (
            <Button variant="outline" size="sm" onClick={goToNextChapter}>
              Next Chapter &gt;
            </Button>
          )}
        </div>
      )}
    </div>
  );
}
