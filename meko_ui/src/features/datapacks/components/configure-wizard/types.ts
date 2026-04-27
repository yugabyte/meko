/**
 * Shared types for the post-create datapack configure wizard (especially the Sync Data step).
 * ListedSource is a lightweight row for user-added files or URLs before any server upload exists.
 */
export type ListedSource = { id: string; label: string; kind: "file" | "url" };

export type SyncSectionId = "knowledge" | "conversations" | "skills";
