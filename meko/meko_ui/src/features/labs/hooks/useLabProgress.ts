import { useCallback, useState } from "react";

import { getLocalItem, setLocalItem } from "@/lib/browserStorage";

import { findStageLocation } from "../lib/labChapters";
import type { LabDefinition, LabStageId } from "../types";

type Progress = { chapterIndex: number; stageIndex: number };

function storageKey(labId: string): string {
  return `meko:lab-progress:${labId}`;
}

function readProgress(labId: string): Progress {
  const raw = getLocalItem(storageKey(labId));
  if (!raw) return { chapterIndex: 0, stageIndex: 0 };
  try {
    const parsed = JSON.parse(raw) as Partial<Progress>;
    // An older blob (flat {stageIndex}, or the pre-restructuring
    // {chapterIndex, stepIndex}) has no valid {chapterIndex, stageIndex}
    // pair, so this falls back to the start -- no migration needed.
    if (typeof parsed.chapterIndex === "number" && typeof parsed.stageIndex === "number") {
      return { chapterIndex: parsed.chapterIndex, stageIndex: parsed.stageIndex };
    }
    return { chapterIndex: 0, stageIndex: 0 };
  } catch {
    return { chapterIndex: 0, stageIndex: 0 };
  }
}

function clamp(chapters: LabDefinition["chapters"], chapterIndex: number, stageIndex: number): Progress {
  const c = Math.max(0, Math.min(chapterIndex, chapters.length - 1));
  const s = Math.max(0, Math.min(stageIndex, chapters[c].stages.length - 1));
  return { chapterIndex: c, stageIndex: s };
}

/**
 * Which chapter and stage within it the user is on, persisted per lab id so
 * reloading the runner resumes where they left off. Bounds-checked against
 * the lab's actual chapter/stage counts on every read and mutation, so a lab
 * definition edit (fewer chapters/stages than a stale saved position) can't
 * leave the nav pointing at nothing.
 */
export function useLabProgress(lab: LabDefinition) {
  const [progress, setProgress] = useState<Progress>(() => readProgress(lab.id));
  const { chapterIndex, stageIndex } = clamp(lab.chapters, progress.chapterIndex, progress.stageIndex);

  const persist = useCallback(
    (next: Progress) => {
      setProgress(next);
      setLocalItem(storageKey(lab.id), JSON.stringify(next));
    },
    [lab.id],
  );

  const goToStage = useCallback(
    (nextChapterIndex: number, nextStageIndex: number) => {
      persist(clamp(lab.chapters, nextChapterIndex, nextStageIndex));
    },
    [lab.chapters, persist],
  );

  const goToStageById = useCallback(
    (stageId: LabStageId) => {
      const location = findStageLocation(lab.chapters, stageId);
      if (location) goToStage(location.chapterIndex, location.stageIndex);
    },
    [lab.chapters, goToStage],
  );

  const isLastStageInChapter = stageIndex === lab.chapters[chapterIndex].stages.length - 1;
  const isLastChapter = chapterIndex === lab.chapters.length - 1;
  const isLastStage = isLastStageInChapter && isLastChapter;

  const goToNext = useCallback(() => {
    if (isLastStageInChapter) {
      if (!isLastChapter) goToStage(chapterIndex + 1, 0);
      return;
    }
    goToStage(chapterIndex, stageIndex + 1);
  }, [chapterIndex, stageIndex, isLastStageInChapter, isLastChapter, goToStage]);

  // Explicit chapter-to-chapter navigation, distinct from `goToNext` (which
  // advances one stage at a time and happens to cross a chapter boundary as
  // a side effect). The nav renders only the current chapter's own stages,
  // so moving between chapters needs its own deliberate action rather than
  // an implicit consequence of clicking the last stage's "Next".
  const goToNextChapter = useCallback(() => {
    if (!isLastChapter) goToStage(chapterIndex + 1, 0);
  }, [chapterIndex, isLastChapter, goToStage]);

  const goToPreviousChapter = useCallback(() => {
    if (chapterIndex > 0) goToStage(chapterIndex - 1, 0);
  }, [chapterIndex, goToStage]);

  return {
    chapterIndex,
    stageIndex,
    currentChapter: lab.chapters[chapterIndex],
    currentStage: lab.chapters[chapterIndex].stages[stageIndex],
    goToStage,
    goToStageById,
    goToNext,
    goToNextChapter,
    goToPreviousChapter,
    isLastStage,
    isLastStageInChapter,
    isLastChapter,
  };
}
