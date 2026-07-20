/**
 * Shared rules for knowledge file staging (wizard + FileUpload).
 * Keep in sync with backend upload constraints.
 */
export const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS = ".pdf,.txt,.md,.json,.mp4";
export const KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB = 5;
export const KNOWLEDGE_UPLOAD_MAX_BATCH = 10;

/** Exact set of accepted extensions, derived from the comma-separated list
 *  above. Used for membership checks so we match whole extensions (`.md`)
 *  rather than substrings of the raw string (`.includes(".m")` would wrongly
 *  pass). */
const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSION_SET = new Set(
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS.split(",").map((s) => s.trim().toLowerCase()),
);

/** Human-readable list for UI (matches `KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS`). */
export const KNOWLEDGE_UPLOAD_EXTENSIONS_HINT = KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS.split(",")
  .map((s) => s.replace(/^\./, "").trim())
  .join(", ");

/** Mirrors the backend's check (api_server: `filepath.Ext` in
 *  knowledge_base_handler.go) so both sides accept/reject the same
 *  filenames: the suffix from the last '.' onward, or "" if there's no
 *  dot at all. Deliberately does NOT special-case a leading dot --
 *  `filepath.Ext(".pdf")` returns ".pdf", not "", so a dotfile named
 *  literally ".pdf" must be treated the same way here. */
export function knowledgeFileExtension(filename: string): string {
  const dot = filename.lastIndexOf(".");
  return dot === -1 ? "" : filename.slice(dot).toLowerCase();
}

// Mirrors backend sanitizeFilename (s3_service.go) so a raw File.name
// matches the sanitized S3 key in pipelineDetails.document_name.
export function sanitizeFilenameForMatch(name: string): string {
  return name.replace(/[^a-zA-Z0-9_.-]/g, "_");
}

/** Copy for a content-hash dedup. `awaitingProcessing` = matched an
 *  unprocessed orphan, not a truly-already-ingested file (FileUpload's
 *  case; the wizard has no orphans and always passes `false`). `existingName`
 *  comes back from the backend already sanitized (it's the S3 key's
 *  basename), while `uploadedName` is the raw, un-sanitized `File.name` --
 *  normalize before comparing so "same name, different-looking because of
 *  sanitization" doesn't get reported as "stored under a different name". */
export function dedupNote(
  uploadedName: string,
  existingName: string | undefined,
  awaitingProcessing: boolean,
): string {
  const sameName = !existingName || sanitizeFilenameForMatch(uploadedName) === existingName;
  if (awaitingProcessing) {
    if (!sameName) {
      return `Already uploaded as "${existingName}" — adding it to the knowledge base now.`;
    }
    return "Already uploaded — adding it to the knowledge base now.";
  }
  if (!sameName) {
    return `An identical document (${existingName}) is already processed`;
  }
  return "The document is already processed";
}

export function isAcceptedKnowledgeFile(filename: string): boolean {
  const ext = knowledgeFileExtension(filename);
  // `knowledgeFileExtension` returns "" when there's no extension; the Set
  // never contains "", so that case is rejected without a special guard.
  return KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSION_SET.has(ext);
}

export type KnowledgeBatchFilterResult = {
  accepted: File[];
  warningParts: string[];
};

/**
 * Filters incoming files by type/size and caps by remaining batch slots
 * (same logic as FileUpload.addFiles). Deliberately does NOT dedupe by
 * name/size client-side -- guessing "same file" from metadata alone is
 * unreliable (two different files can share a name+size) and can silently
 * drop a real file before it's ever given a chance. The server's
 * content-hash check at upload time is the single source of truth for
 * duplicates and reports back "already uploaded" (see `dedupNote`).
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
  if (skippedSize > 0) {
    // Upsell copy rather than a bare limit statement -- the 5MB cap is the
    // same for every tier today, but this nudges Free users toward Pro
    // without promising a bigger limit that doesn't exist yet.
    warningParts.push(
      `${skippedSize} skipped (over ${KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB}MB — upgrade to Pro for larger file uploads)`,
    );
  }
  if (skippedLimit > 0) {
    warningParts.push(`${skippedLimit} skipped (${KNOWLEDGE_UPLOAD_MAX_BATCH} files already in progress)`);
  }

  return { accepted, warningParts };
}

