/**
 * Shared section identifiers for the per-datapack detail page.
 * Each maps to a dedicated summary route navigated to from the
 * CollectiveLearningPipeline tiles; tone palettes in the pipeline
 * are keyed off the same union.
 */
export const DATAPACK_DETAIL_TAB = {
  conversations: "conversations",
  memories: "memories",
  learnings: "learnings",
  knowledge: "knowledge",
  collectiveMemory: "collective-memory",
} as const;

export type DatapackDetailTab = (typeof DATAPACK_DETAIL_TAB)[keyof typeof DATAPACK_DETAIL_TAB];
