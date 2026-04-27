/**
 * Shared tab identifiers and labels for the per-datapack detail shell.
 * Keeps the tab strip and parent page in sync without duplicating string unions.
 */
export const DATAPACK_DETAIL_TAB = {
  learnings: "learnings",
  conversations: "conversations",
  memory: "memory",
  knowledge: "knowledge",
} as const;

export type DatapackDetailTab = (typeof DATAPACK_DETAIL_TAB)[keyof typeof DATAPACK_DETAIL_TAB];

export const DATAPACK_DETAIL_TAB_ITEMS: { id: DatapackDetailTab; label: string }[] = [
  { id: DATAPACK_DETAIL_TAB.learnings, label: "Learnings" },
  { id: DATAPACK_DETAIL_TAB.conversations, label: "Conversations" },
  { id: DATAPACK_DETAIL_TAB.memory, label: "Memory" },
  { id: DATAPACK_DETAIL_TAB.knowledge, label: "Knowledge" }
];
