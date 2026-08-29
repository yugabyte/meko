import { useCallback, useState } from "react";

import { getLocalItem, setLocalItem } from "@/lib/browserStorage";

import type { LabDefinition } from "../types";

type Progress = { stageIndex: number };

function storageKey(labId: string): string {
  return `meko:lab-progress:${labId}`;
}

function readProgress(labId: string): Progress {
  const raw = getLocalItem(storageKey(labId));
  if (!raw) return { stageIndex: 0 };
  try {
    const parsed = JSON.parse(raw) as Partial<Progress>;
    // A pre-restructuring blob ({chapterIndex, stepIndex}) has no
    // stageIndex key, so this falls back to 0 — no migration needed.
    return { stageIndex: typeof parsed.stageIndex === "number" ? parsed.stageIndex : 0 };
  } catch {
    return { stageIndex: 0 };
  }
}

/**
 * Which of the lab's 5 fixed stages the user is on, persisted per lab id so
 * reloading the runner resumes where they left off. Bounds-checked against
 * the lab's actual stage count so a lab definition edit (fewer stages than a
 * stale saved index) can't leave the nav pointing at nothing.
 */
export function useLabProgress(lab: LabDefinition) {
  const [progress, setProgress] = useState<Progress>(() => readProgress(lab.id));
  const stageIndex = Math.max(0, Math.min(progress.stageIndex, lab.stages.length - 1));

  const persist = useCallback(
    (next: Progress) => {
      setProgress(next);
      setLocalItem(storageKey(lab.id), JSON.stringify(next));
    },
    [lab.id],
  );

  const goToStage = useCallback(
    (index: number) => {
      persist({ stageIndex: Math.max(0, Math.min(index, lab.stages.length - 1)) });
    },
    [lab.stages.length, persist],
  );

  const goToNext = useCallback(() => goToStage(stageIndex + 1), [goToStage, stageIndex]);

  const isLastStage = stageIndex === lab.stages.length - 1;

  return {
    stageIndex,
    currentStage: lab.stages[stageIndex],
    goToStage,
    goToNext,
    isLastStage,
  };
}
