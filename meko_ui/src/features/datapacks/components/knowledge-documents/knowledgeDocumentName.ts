/** Last path segment for keys like `policies/refunds.pdf` — compact label; full path stays in `title`. */
export function documentNameLastSegment(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "—";
  const parts = trimmed.split("/").filter(Boolean);
  return parts.length > 0 ? parts[parts.length - 1]! : trimmed;
}
