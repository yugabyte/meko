/**
 * Shared tab identifiers and labels for the per-datapack detail shell.
 * Keeps the tab strip and parent page in sync without duplicating string unions.
 */
export const DATAPACK_DETAIL_TAB = {
  conversations: "conversations",
  memories: "memories",
  learnings: "learnings",
  knowledge: "knowledge",
} as const;

export type DatapackDetailTab = (typeof DATAPACK_DETAIL_TAB)[keyof typeof DATAPACK_DETAIL_TAB];

export const DATAPACK_DETAIL_TAB_ITEMS: { id: DatapackDetailTab; label: string }[] = [
  { id: DATAPACK_DETAIL_TAB.conversations, label: "Conversations" },
  { id: DATAPACK_DETAIL_TAB.memories, label: "Memories" },
  { id: DATAPACK_DETAIL_TAB.learnings, label: "Learnings" },
  { id: DATAPACK_DETAIL_TAB.knowledge, label: "Knowledge" }
];
