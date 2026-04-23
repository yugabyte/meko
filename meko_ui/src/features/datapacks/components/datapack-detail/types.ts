/**
 * Shared tab identifiers and labels for the per-datapack detail shell.
 * Keeps the tab strip and parent page in sync without duplicating string unions.
 */
export type DatapackDetailTab =
  | "overview"
  | "agents"
  | "knowledge"
  | "memory"
  | "conversations"
  | "learnings"
  | "traces";

export const DATAPACK_DETAIL_TAB_ITEMS: { id: DatapackDetailTab; label: string }[] = [
  { id: "overview", label: "Overview" },
  { id: "agents", label: "Agents" },
  { id: "knowledge", label: "Knowledge" },
  { id: "memory", label: "Memory" },
  { id: "conversations", label: "Conversations" },
  { id: "learnings", label: "Learnings" },
  { id: "traces", label: "Traces" },
];
