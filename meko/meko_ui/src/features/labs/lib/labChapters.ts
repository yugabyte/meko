import type { LabChapter, LabStage, LabStageId } from "../types";

/** Flattens a lab's chapters into one ordered stage list -- e.g. for
 *  LabDetailPage's overview timeline, which numbers every stage 1..N
 *  regardless of which chapter it's in. */
export function flattenLabStages(chapters: LabChapter[]): LabStage[] {
  return chapters.flatMap((chapter) => chapter.stages);
}

/** Locates a stage by id across every chapter. Used by useLabProgress's
 *  goToStageById -- jumping to a named stage (e.g. a degrade prompt's "Go to
 *  Deploy Agent" button) shouldn't hardcode a flat position that silently
 *  points at the wrong stage if a chapter is inserted or reordered. */
export function findStageLocation(
  chapters: LabChapter[],
  stageId: LabStageId,
): { chapterIndex: number; stageIndex: number } | null {
  for (let chapterIndex = 0; chapterIndex < chapters.length; chapterIndex++) {
    const stageIndex = chapters[chapterIndex].stages.findIndex((s) => s.id === stageId);
    if (stageIndex !== -1) return { chapterIndex, stageIndex };
  }
  return null;
}
