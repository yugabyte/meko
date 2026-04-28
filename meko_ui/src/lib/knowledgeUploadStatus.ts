/**
 * Lifecycle values for knowledge file S3 uploads (configure wizard + datapack file UI).
 * Use {@link KNOWLEDGE_UPLOAD_STATUS} in comparisons and assignments so literals stay single-sourced.
 */
export const KNOWLEDGE_UPLOAD_STATUS = {
  PENDING: "pending",
  UPLOADING: "uploading",
  DONE: "done",
  ERROR: "error",
} as const;

export type KnowledgeUploadStatus = (typeof KNOWLEDGE_UPLOAD_STATUS)[keyof typeof KNOWLEDGE_UPLOAD_STATUS];
