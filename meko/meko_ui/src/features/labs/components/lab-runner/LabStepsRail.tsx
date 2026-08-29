import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from "@/components/ui/accordion";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import type { useLabProgress } from "../../hooks/useLabProgress";
import type { LabDefinition } from "../../types";
import { LabChapterProgress } from "./LabChapterProgress";

/**
 * LEFT pane of the lab runner: a chaptered walkthrough. `progress` is a
 * single shared instance of useLabProgress owned by LabRunnerPage (not
 * called again here) — LabRunnerPage also needs the current step to fire
 * onEnterAction side effects, so both must read the same state.
 */
export function LabStepsRail({
  lab,
  progress,
}: {
  lab: LabDefinition;
  progress: ReturnType<typeof useLabProgress>;
}) {
  const { chapterIndex, stepIndex, goToChapter, goToNext, isLastStep, isLastStepOfChapter } = progress;

  return (
    <div className="flex h-full flex-col border-r border-border bg-background">
      <LabChapterProgress chapters={lab.chapters} currentChapterIndex={chapterIndex} />
      <Accordion
        type="single"
        collapsible
        value={lab.chapters[chapterIndex]?.id}
        onValueChange={(value) => {
          const index = lab.chapters.findIndex((c) => c.id === value);
          if (index >= 0) goToChapter(index);
        }}
        className="min-h-0 flex-1 overflow-y-auto px-4"
      >
        {lab.chapters.map((chapter, cIndex) => (
          <AccordionItem key={chapter.id} value={chapter.id}>
            <AccordionTrigger>{chapter.title}</AccordionTrigger>
            <AccordionContent>
              <ol className="flex flex-col gap-3">
                {chapter.steps.map((step, sIndex) => {
                  const isCurrent = cIndex === chapterIndex && sIndex === stepIndex;
                  const isDone =
                    cIndex < chapterIndex || (cIndex === chapterIndex && sIndex < stepIndex);
                  return (
                    <li key={step.id}>
                      <p
                        className={cn(
                          "text-sm font-medium",
                          isCurrent ? "text-foreground" : isDone ? "text-muted-foreground" : "text-muted-foreground/70",
                        )}
                      >
                        {sIndex + 1}. {step.title}
                      </p>
                      {isCurrent && (
                        <p className="mt-1 text-sm text-muted-foreground">{step.body}</p>
                      )}
                    </li>
                  );
                })}
              </ol>
            </AccordionContent>
          </AccordionItem>
        ))}
      </Accordion>
      <div className="border-t border-border p-4">
        <Button onClick={goToNext} disabled={isLastStep} className="w-full">
          {isLastStep ? "Done" : isLastStepOfChapter ? "Next chapter" : "Next"}
        </Button>
      </div>
    </div>
  );
}
