/**
 * Shared rules for knowledge file staging (wizard + FileUpload).
 * Keep in sync with backend upload constraints.
 */
export const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS = ".pdf,.txt,.md,.json,.mp4";
export const KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB = 5;
export const KNOWLEDGE_UPLOAD_MAX_BATCH = 10;

export function knowledgeFileExtension(filename: string): string {
  return `.${filename.split(".").pop()?.toLowerCase() ?? ""}`;
}

export function isAcceptedKnowledgeFile(filename: string): boolean {
  return KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS.includes(knowledgeFileExtension(filename));
}

export type KnowledgeBatchFilterResult = {
  accepted: File[];
  warningParts: string[];
};

/**
 * Filters incoming files by type/size and caps by remaining batch slots (same logic as FileUpload.addFiles).
 */
export function filterFilesForKnowledgeUploadBatch(
  incoming: File[],
  currentNonDoneStagingCount: number,
): KnowledgeBatchFilterResult {
  const valid: File[] = [];
  let skippedSize = 0;
  let skippedType = 0;

  for (const f of incoming) {
    if (f.size > KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB * 1024 * 1024) {
      skippedSize++;
      continue;
    }
    if (!isAcceptedKnowledgeFile(f.name)) {
      skippedType++;
      continue;
    }
    valid.push(f);
  }

  const slots = Math.max(0, KNOWLEDGE_UPLOAD_MAX_BATCH - currentNonDoneStagingCount);
  const skippedLimit = Math.max(0, valid.length - slots);
  const accepted = valid.slice(0, Math.max(0, slots));

  const warningParts: string[] = [];
  if (skippedType > 0) warningParts.push(`${skippedType} skipped (unsupported type)`);
  if (skippedSize > 0) warningParts.push(`${skippedSize} skipped (over ${KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB}MB)`);
  if (skippedLimit > 0) {
    warningParts.push(`${skippedLimit} skipped (max ${KNOWLEDGE_UPLOAD_MAX_BATCH} per batch)`);
  }

  return { accepted, warningParts };
}
