// Small formatting helpers shared by the data pages.

/** "Jun 29, 2026, 3:03 PM" — compact absolute timestamp. */
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

/** First segment of a UUID — enough to identify a row at a glance. */
export function shortId(id: string): string {
  return id.split("-")[0] ?? id;
}
