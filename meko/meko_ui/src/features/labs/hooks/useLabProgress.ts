// Where the user is in a lab run, and which stages they've actually
// finished -- both persisted per lab id so reloading the runner resumes
// exactly where they left off.

import { useCallback, useMemo, useState } from "react";

import { getLocalItem, setLocalItem } from "@/lib/browserStorage";

import { findStageLocation, flattenLabStages } from "../lib/labChapters";
import type { LabDefinition, LabStageId } from "../types";

type Progress = { chapterIndex: number; stageIndex: number };

function storageKey(labId: string): string {
  return `meko:lab-progress:${labId}`;
}

/** Separate from the position key so a lab definition edit that invalidates
 *  one doesn't discard the other -- a user who finished four lessons keeps
 *  that credit even if their saved position no longer resolves. */
function completedKey(labId: string): string {
  return `meko:lab-completed:${labId}`;
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

function readCompleted(labId: string): Set<LabStageId> {
  const raw = getLocalItem(completedKey(labId));
  if (!raw) return new Set();
  try {
    const parsed = JSON.parse(raw) as unknown;
    // Stage ids that no longer exist are harmless -- every read of this set
    // is a lookup by a live stage's own id, so a stale entry is never hit.
    return Array.isArray(parsed) ? new Set(parsed as LabStageId[]) : new Set();
  } catch {
    return new Set();
  }
}

function clamp(chapters: LabDefinition["chapters"], chapterIndex: number, stageIndex: number): Progress {
  const c = Math.max(0, Math.min(chapterIndex, chapters.length - 1));
  const s = Math.max(0, Math.min(stageIndex, chapters[c].stages.length - 1));
  return { chapterIndex: c, stageIndex: s };
}

/**
 * Which chapter and stage within it the user is on, plus the set of stages
 * they've completed. Bounds-checked against the lab's actual chapter/stage
 * counts on every read and mutation, so a lab definition edit (fewer
 * chapters/stages than a stale saved position) can't leave the nav pointing
 * at nothing.
 *
 * Completion is reported by the stages themselves rather than derived from
 * datapack counts: a user reusing a datapack they filled in an earlier run
 * would otherwise start with every gate already satisfied and never see the
 * lesson.
 */
export function useLabProgress(lab: LabDefinition) {
  const [progress, setProgress] = useState<Progress>(() => readProgress(lab.id));
  const [completedStages, setCompletedStages] = useState<Set<LabStageId>>(() => readCompleted(lab.id));
  const { chapterIndex, stageIndex } = clamp(lab.chapters, progress.chapterIndex, progress.stageIndex);

  const orderedStages = useMemo(() => flattenLabStages(lab.chapters), [lab.chapters]);

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


  const markStageComplete = useCallback(
    (stageId: LabStageId) => {
      setCompletedStages((prev) => {
        if (prev.has(stageId)) return prev;
        const next = new Set(prev).add(stageId);
        setLocalItem(completedKey(lab.id), JSON.stringify(Array.from(next)));
        return next;
      });
    },
    [lab.id],
  );

  const currentChapter = lab.chapters[chapterIndex];
  const currentStage = currentChapter.stages[stageIndex];

  const flatIndex = orderedStages.findIndex((s) => s.id === currentStage.id);
  const isLastStage = flatIndex === orderedStages.length - 1;

  /** The reason Next is blocked, or null when it isn't. A gated stage stays
   *  blocked until it reports itself complete; everything else is free. */
  const gateMessage =
    currentStage.gate && !completedStages.has(currentStage.id) ? currentStage.gate.message : null;

  /** How far the user has ever got, so the rail can offer every stage they've
   *  reached and disable the ones ahead. Derived rather than persisted: a
   *  completed stage is by definition one they reached, and the current
   *  position covers the stage they're on but haven't finished. */
  const furthestFlatIndex = useMemo(() => {
    const lastCompleted = orderedStages.reduce(
      (acc, stage, i) => (completedStages.has(stage.id) ? i : acc),
      -1,
    );
    // +1 so the stage after the last completed one is reachable -- that's the
    // one the user is being sent to next.
    return Math.max(flatIndex, lastCompleted + 1);
  }, [orderedStages, completedStages, flatIndex]);

  // Moving on IS finishing a stage. Without this only gated stages ever get a
  // tick, so the rail checks the lessons that made you do something and
  // silently skips the explainers you read and left -- and the progress count
  // under-reports by however many of those the lab has. A gated stage can't
  // reach here unmet anyway: Next stays disabled until its gate opens.
  const goToNext = useCallback(() => {
    const next = orderedStages[flatIndex + 1];
    if (!next) return;
    markStageComplete(currentStage.id);
    goToStageById(next.id);
  }, [orderedStages, flatIndex, currentStage.id, markStageComplete, goToStageById]);

  const goToPrevious = useCallback(() => {
    const previous = orderedStages[flatIndex - 1];
    if (previous) goToStageById(previous.id);
  }, [orderedStages, flatIndex, goToStageById]);

  return {
    chapterIndex,
    stageIndex,
    currentChapter,
    currentStage,
    orderedStages,
    flatIndex,
    furthestFlatIndex,
    completedStages,
    markStageComplete,
    goToStage,
    goToStageById,
    goToNext,
    goToPrevious,
    gateMessage,
    isLastStage,
  };
}
