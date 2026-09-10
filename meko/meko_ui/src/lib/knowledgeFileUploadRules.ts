/**
 * Shared rules for knowledge file staging (wizard + FileUpload).
 * Keep in sync with backend upload constraints.
 */

import { MB } from "@/lib/fileSize";
import { FREE_TIER_LIMITS } from "@/lib/freeTier";

/**
 * Single source of truth: accepted extension → MIME type. Mirrors
 * api_server's uploadMimeByExt (SUPPORTED_DOCUMENT_TYPES); an extension
 * missing there uploads cleanly and sits QUEUED unindexed. The `accept`
 * attribute, hint text, and sniffer all derive from this. Most common first.
 */
export const KNOWLEDGE_UPLOAD_MIME_BY_EXT: Record<string, string> = {
  ".pdf": "application/pdf",
  ".txt": "text/plain",
  ".text": "text/plain",
  ".md": "text/markdown",
  ".csv": "text/csv",
  ".json": "application/json",
  ".xml": "text/xml",
  ".html": "text/html",
  ".htm": "text/html",
};

const ACCEPTED_EXTENSION_LIST = Object.keys(KNOWLEDGE_UPLOAD_MIME_BY_EXT);

/** Raw list export so callers (e.g. the Files table's type-filter chips)
 *  stay in sync with upload's allow-list without duplicating it. */
export const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSION_LIST = ACCEPTED_EXTENSION_LIST;

/** Comma-joined form for a file input's `accept` attribute. */
export const KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS = ACCEPTED_EXTENSION_LIST.join(",");

/** Fallback per-file cap before /auth/me lands (see
 *  useKnowledgeUploadMaxFileSizeMB for the real per-tier cap). Free tier,
 *  the lowest, so a MEKO_FREE_TIER_KB_FILE_BYTES override applies here too. */
export const KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB = FREE_TIER_LIMITS.kbFileBytes / MB(1);
export const KNOWLEDGE_UPLOAD_MAX_BATCH = 10;

/** Human-readable list for UI, e.g. "pdf, txt, text, md, csv, json, xml, html, htm". */
export const KNOWLEDGE_UPLOAD_EXTENSIONS_HINT = ACCEPTED_EXTENSION_LIST.map((e) =>
  e.startsWith(".") ? e.slice(1) : e,
).join(", ");

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
  return ACCEPTED_EXTENSION_LIST.includes(knowledgeFileExtension(filename));
}

/**
 * Rejection message for an unsupported extension, or null when acceptable.
 * Decided from the filename alone, so callers can refuse a file before
 * spending a content hash or a presign round trip on it.
 */
export function unsupportedKnowledgeFileMessage(filename: string): string | null {
  if (isAcceptedKnowledgeFile(filename)) return null;
  const ext = knowledgeFileExtension(filename);
  const subject = ext
    ? `${ext} files aren't supported`
    : "Files without an extension aren't supported";
  return `${subject}. Allowed types: ${KNOWLEDGE_UPLOAD_EXTENSIONS_HINT}.`;
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
  maxFileSizeMB: number = KNOWLEDGE_UPLOAD_MAX_FILE_SIZE_MB,
): KnowledgeBatchFilterResult {
  const valid: File[] = [];
  let skippedSize = 0;
  let skippedType = 0;

  for (const f of incoming) {
    // Infinity (un-capped tiers) never trips this branch.
    if (f.size > maxFileSizeMB * 1024 * 1024) {
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
    // Upsell copy rather than a bare limit statement -- the cap is
    // tier-dependent (5 MB free / 50 MB standard), so nudge toward an
    // upgrade without naming a specific plan.
    warningParts.push(
      `${skippedSize} skipped (over ${maxFileSizeMB}MB — upgrade your plan for larger file uploads)`,
    );
  }
  if (skippedLimit > 0) {
    warningParts.push(`${skippedLimit} skipped (${KNOWLEDGE_UPLOAD_MAX_BATCH} files already in progress)`);
  }

  return { accepted, warningParts };
}

