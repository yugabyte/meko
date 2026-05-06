/**
 * Shared rules for knowledge file staging (wizard + FileUpload).
 * Keep in sync with backend upload constraints.
 */
export const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS = ".pdf,.txt,.md,.json,.mp4";
export const KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB = 5;
export const KNOWLEDGE_UPLOAD_MAX_BATCH = 10;

/** Human-readable list for UI (matches `KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS`). */
export const KNOWLEDGE_UPLOAD_EXTENSIONS_HINT = KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS.split(",")
  .map((s) => s.replace(/^\./, "").trim())
  .join(", ");

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

/**
 * Drops files whose name already exists in the list or appears twice in `incoming`
 * (case-insensitive basename match).
 */
export function filterDuplicateKnowledgeFileNames(
  incoming: File[],
  existingFileLabels: string[],
): { unique: File[]; duplicateCount: number } {
  const taken = new Set(existingFileLabels.map((n) => n.trim().toLowerCase()));
  const unique: File[] = [];
  let duplicateCount = 0;
  const seenIncoming = new Set<string>();
  for (const f of incoming) {
    const key = f.name.trim().toLowerCase();
    if (taken.has(key) || seenIncoming.has(key)) {
      duplicateCount++;
      continue;
    }
    seenIncoming.add(key);
    taken.add(key);
    unique.push(f);
  }
  return { unique, duplicateCount };
}
