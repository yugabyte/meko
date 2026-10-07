// Command palette labels and filtering. Pure, so it's unit-tested.

/** A piece of a palette label: plain text, or a session's or project's name, drawn in its own style. */
export type LabelPart = string | { session: string } | { project: string };

/** The label as plain text: what the filter searches and what screen readers read. */
export function labelText(parts: LabelPart[]): string {
  return parts.map((p) => (typeof p === 'string' ? p : 'session' in p ? p.session : p.project)).join('');
}

/** The items whose label and keywords contain every word typed, in their order. */
export function filterItems<T extends { label: LabelPart[]; keywords: string }>(items: T[], q: string): T[] {
  const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return items;
  return items.filter((it) => {
    const hay = `${labelText(it.label)} ${it.keywords}`.toLowerCase();
    return terms.every((t) => hay.includes(t));
  });
}
