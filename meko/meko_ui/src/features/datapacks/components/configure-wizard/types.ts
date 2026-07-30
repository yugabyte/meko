/**
 * Shared types for the post-create datapack configure wizard (especially the Sync Knowledge step).
 * ListedSource tracks staged files or URLs; knowledge files may carry upload lifecycle for S3 ingest.
 */
import type { KnowledgeUploadStatus } from "@/lib/knowledgeUploadStatus";

export type { KnowledgeUploadStatus };

export type ListedSource = {
  id: string;
  label: string;
  kind: "file" | "url";
  file?: File;
  sizeBytes?: number;
  uploadStatus?: KnowledgeUploadStatus;
  uploadProgress?: number;
  uploadError?: string;
  /** Set when the upload was skipped because identical content already existed. */
  uploadNote?: string;
};

export type SyncSectionId = "knowledge" | "conversations" | "skills";
