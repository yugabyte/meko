/**
 * Document lifecycle values from GET .../knowledge-bases/documents (aligned with server contract).
 */
export const KB_DOCUMENT_STATUS = {
  QUEUED: "QUEUED",
  PROCESSING: "PROCESSING",
  COMPLETED: "COMPLETED",
  FAILED: "FAILED",
} as const;

export type KbDocumentStatus = (typeof KB_DOCUMENT_STATUS)[keyof typeof KB_DOCUMENT_STATUS];

export function kbDocumentStatusLabel(s: string): string {
  switch (s) {
    case KB_DOCUMENT_STATUS.QUEUED:
      return "Queued";
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

export function kbDocumentStatusTone(
  s: string,
): "muted" | "blue" | "green" | "red" {
  switch (s) {
    case KB_DOCUMENT_STATUS.COMPLETED:
      return "green";
    case KB_DOCUMENT_STATUS.FAILED:
      return "red";
    case KB_DOCUMENT_STATUS.PROCESSING:
      return "blue";
    default:
      return "muted";
  }
}
