// Small shared helpers for building telemetry event properties, so the same
// derivations (file type, byte→KB) aren't re-implemented at each call site.

/** Document file types reported on the document_uploaded event. */
export type TelemetryFileType = "pdf" | "docx" | "txt" | "md" | "csv" | "other";

/** Map a filename's extension to the bounded file_type enum. */
export function fileTypeFromName(name: string): TelemetryFileType {
  const ext = name.split(".").pop()?.toLowerCase() ?? "";
  switch (ext) {
    case "pdf":
      return "pdf";
    case "docx":
      return "docx";
    case "txt":
      return "txt";
    case "md":
      return "md";
    case "csv":
      return "csv";
    default:
      return "other";
  }
}

/** Convert a byte count to whole kilobytes (integer), for file_size_kb. */
export function bytesToKb(bytes: number): number {
  return Math.round(bytes / 1024);
}

/** Current time as an ISO-8601 string, for *_timestamp event properties. */
export function nowIso(): string {
  return new Date().toISOString();
}
