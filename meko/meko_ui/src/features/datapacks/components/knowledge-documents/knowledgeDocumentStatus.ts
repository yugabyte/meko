/**
 * Document lifecycle values from GET .../knowledge-bases/documents (aligned with server contract).
 */
export const KB_DOCUMENT_STATUS = {
  NOT_STARTED: "NOT_STARTED",
  QUEUED: "QUEUED",
  PROCESSING: "PROCESSING",
  COMPLETED: "COMPLETED",
  FAILED: "FAILED",
  RETRY: "RETRY",
} as const;

export type KbDocumentStatus = (typeof KB_DOCUMENT_STATUS)[keyof typeof KB_DOCUMENT_STATUS];

/** True while the indexing pipeline still has the file in hand, so
 *  destructive actions must be blocked: the rag_agent worker keeps
 *  writing chunks past a delete, orphaning them with no document row left
 *  to clean them up by. The server enforces the same thing (409
 *  DOCUMENT_PROCESSING in DeleteDocument) — this only spares the user a
 *  round-trip to find out.
 *
 *  Deliberately an allow-list rather than "not terminal", so an
 *  unrecognised status can't be blocked forever with nothing to ever move
 *  it to COMPLETED. It must list *every* non-terminal value of
 *  dist_rag.document_processing_status_enum, which has six -- checking
 *  only QUEUED and PROCESSING lets NOT_STARTED and RETRY through, and a
 *  delete in either of those is exactly the orphaned-chunk case this
 *  exists to prevent. Keep in step with the switch in DeleteDocument. */
export function isKbDocumentInFlight(s: string): boolean {
  return (
    s === KB_DOCUMENT_STATUS.NOT_STARTED ||
    s === KB_DOCUMENT_STATUS.QUEUED ||
    s === KB_DOCUMENT_STATUS.PROCESSING ||
    s === KB_DOCUMENT_STATUS.RETRY
  );
}

export function kbDocumentStatusLabel(s: string): string {
  switch (s) {
    // dist_rag's pre-pickup state. "Queued" to the user either way --
    // the distinction (row written vs. work_queue task created) is
    // internal and there is nothing different to do about it.
    case KB_DOCUMENT_STATUS.NOT_STARTED:
    case KB_DOCUMENT_STATUS.QUEUED:
      return "Queued";
    case KB_DOCUMENT_STATUS.RETRY:
      return "Retrying";
    case KB_DOCUMENT_STATUS.PROCESSING:
      return "Processing";
    case KB_DOCUMENT_STATUS.COMPLETED:
      return "Completed";
    case KB_DOCUMENT_STATUS.FAILED:
      return "Failed";
    default:
      return s;
  }
}

export function kbDocumentStatusTone(s: string): "muted" | "blue" | "yellow" | "green" | "red" {
  switch (s) {
    case KB_DOCUMENT_STATUS.COMPLETED:
      return "green";
    case KB_DOCUMENT_STATUS.FAILED:
      return "red";
    case KB_DOCUMENT_STATUS.PROCESSING:
    case KB_DOCUMENT_STATUS.RETRY:
      return "yellow";
    case KB_DOCUMENT_STATUS.NOT_STARTED:
    case KB_DOCUMENT_STATUS.QUEUED:
      return "blue";
    default:
      return "muted";
  }
}

export function kbDocumentStatusTooltip(s: string): string {
  switch (s) {
    case KB_DOCUMENT_STATUS.NOT_STARTED:
    case KB_DOCUMENT_STATUS.QUEUED:
      return "Waiting for an indexing worker to become available. Processing will start automatically.";
    case KB_DOCUMENT_STATUS.RETRY:
      return "A previous attempt didn't finish. Indexing will be retried automatically.";
    case KB_DOCUMENT_STATUS.PROCESSING:
      return "Currently reading, chunking, and embedding the file into the vector database.";
    case KB_DOCUMENT_STATUS.COMPLETED:
      return "Document is successfully processed and ready for search and retrieval.";
    case KB_DOCUMENT_STATUS.FAILED:
      return "Processing failed. Please check the file format or contact support if the issue persists.";
    default:
      return "";
  }
}
