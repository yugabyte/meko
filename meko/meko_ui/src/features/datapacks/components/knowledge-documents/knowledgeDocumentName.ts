/** Last path segment for keys like `policies/refunds.pdf` — compact label; full path stays in `title`. */
export function documentNameLastSegment(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "—";
  const parts = trimmed.split("/").filter(Boolean);
  return parts.length > 0 ? parts[parts.length - 1]! : trimmed;
}

/** Filename shortened for use *inside a sentence* — toasts, alerts, any
 *  fixed-width surface with no room for a `title` tooltip. Knowledge base
 *  uploads are routinely long enough (release names, hashed exports) to
 *  blow out a toast, and since a filename is one unbroken token the
 *  browser clips it rather than wrapping, cutting off the end of the
 *  sentence with it. Elided in the middle so the extension survives,
 *  which is usually the part worth reading. Anywhere with room for the
 *  whole name should render it in full and put it in `title` instead. */
export function documentNameForMessage(name: string, max = 48): string {
  const base = documentNameLastSegment(name);
  if (base.length <= max) return base;
  const head = Math.ceil((max - 1) / 2);
  const tail = Math.floor((max - 1) / 2);
  return `${base.slice(0, head)}…${base.slice(-tail)}`;
}

/** Lowercased file extension (e.g. ".pdf") from a document name/path, or
 *  "" if there isn't one. `document_type` on a `KnowledgeBaseDocument` is
 *  the S3 object's MIME content-type (e.g. "text/csv"), not an
 *  extension — anything that needs to key off the upload allow-list's
 *  extension strings (file-icon tone, the type filter's available-chips
 *  sample) should use this instead. */
export function documentFileExtension(name: string): string {
  const base = documentNameLastSegment(name);
  const match = /\.[^./]+$/.exec(base);
  return match ? match[0].toLowerCase() : "";
}
