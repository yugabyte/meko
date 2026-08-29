import { useCallback, useMemo, useState } from "react";

import { getLocalItem, setLocalItem } from "@/lib/browserStorage";

import type { LabDefinition } from "../types";

type Progress = { chapterIndex: number; stepIndex: number };

function storageKey(labId: string): string {
  return `meko:lab-progress:${labId}`;
}

function readProgress(labId: string): Progress {
  const raw = getLocalItem(storageKey(labId));
  if (!raw) return { chapterIndex: 0, stepIndex: 0 };
  try {
    const parsed = JSON.parse(raw) as Partial<Progress>;
    return {
      chapterIndex: typeof parsed.chapterIndex === "number" ? parsed.chapterIndex : 0,
      stepIndex: typeof parsed.stepIndex === "number" ? parsed.stepIndex : 0,
    };
  } catch {
    return { chapterIndex: 0, stepIndex: 0 };
  }
}

/**
 * Chapter/step position within a lab's walkthrough, persisted per lab id so
 * reloading the runner resumes where the user left off. Bounds-checks
 * against the lab's actual chapter/step counts so a lab definition edit
 * (fewer chapters/steps than a stale saved position) can't leave the rail
 * pointing at nothing.
 */
export function useLabProgress(lab: LabDefinition) {
  const [progress, setProgress] = useState<Progress>(() => readProgress(lab.id));

  const chapterIndex = Math.min(progress.chapterIndex, lab.chapters.length - 1);
  const stepIndex = Math.min(
    progress.stepIndex,
    lab.chapters[chapterIndex].steps.length - 1,
  );

  const persist = useCallback(
    (next: Progress) => {
      setProgress(next);
      setLocalItem(storageKey(lab.id), JSON.stringify(next));
    },
    [lab.id],
  );

  const goToNext = useCallback(() => {
    const chapter = lab.chapters[chapterIndex];
    if (stepIndex < chapter.steps.length - 1) {
      persist({ chapterIndex, stepIndex: stepIndex + 1 });
    } else if (chapterIndex < lab.chapters.length - 1) {
      persist({ chapterIndex: chapterIndex + 1, stepIndex: 0 });
    }
  }, [lab.chapters, chapterIndex, stepIndex, persist]);

  const goToChapter = useCallback(
    (index: number) => {
      persist({ chapterIndex: Math.max(0, Math.min(index, lab.chapters.length - 1)), stepIndex: 0 });
    },
    [lab.chapters.length, persist],
  );

  const isLastStep = useMemo(
    () =>
      chapterIndex === lab.chapters.length - 1 &&
      stepIndex === lab.chapters[chapterIndex].steps.length - 1,
    [lab.chapters, chapterIndex, stepIndex],
  );

  const isLastStepOfChapter = stepIndex === lab.chapters[chapterIndex].steps.length - 1;

  return {
    chapterIndex,
    stepIndex,
    currentChapter: lab.chapters[chapterIndex],
    currentStep: lab.chapters[chapterIndex].steps[stepIndex],
    goToNext,
    goToChapter,
    isLastStep,
    isLastStepOfChapter,
  };
}
