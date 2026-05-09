/**
 * Attempt to extract one or more readable memory strings from the raw
 * output JSON returned by Mem0 / memory tool spans. Returns an array
 * of strings to display; falls back to the raw value when parsing fails.
 *
 * Shared by TraceDrawer (memory cards) and MemoryHoverCard (popover).
 */
export function parseMemoryOutput(raw: string): string[] {
  const trimmed = raw.trim();
  if (!trimmed || trimmed === "null" || trimmed === '""' || trimmed === "{}") {
    return [];
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return [trimmed];
  }

  // Unbox a quoted string: `"\"some text\""` → `"some text"`.
  if (typeof parsed === "string") {
    const inner = parsed.trim();
    if (!inner) return [];
    // Inner value might itself be JSON (double-encoded).
    try {
      const inner2 = JSON.parse(inner);
      if (typeof inner2 === "string") return inner2 ? [inner2] : [];
      parsed = inner2;
    } catch {
      return [inner];
    }
  }

  if (!parsed || typeof parsed !== "object") return [String(parsed)];

  const obj = parsed as Record<string, unknown>;

  // { results: [{ memory: "..." }, ...] }
  if (Array.isArray(obj.results)) {
    const texts = (obj.results as Record<string, unknown>[])
      .map((r) => (typeof r.memory === "string" ? r.memory.trim() : ""))
      .filter(Boolean);
    if (texts.length) return texts;
  }

  // { memory: "..." }
  if (typeof obj.memory === "string" && obj.memory.trim()) {
    return [obj.memory.trim()];
  }

  // { data: "..." } or { text: "..." } or { content: "..." } or { message: "..." }
  for (const key of ["data", "text", "content", "message"]) {
    const val = obj[key];
    if (typeof val === "string" && val.trim()) return [val.trim()];
  }

  return [JSON.stringify(parsed)];
}
