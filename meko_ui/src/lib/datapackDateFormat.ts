/** Format an ISO timestamp for display in UTC (matches Datapack metadata in the app). */
export function formatDatapackCreatedAtUtc(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const formatted = d.toLocaleString("en-US", {
    timeZone: "UTC",
    weekday: "short",
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    hour12: true,
  });
  return `${formatted} UTC`;
}

/**
 * Numeric sort key for a datapack `created_at` ISO string (UTC).
 * Missing or invalid timestamps sort last when using descending order.
 */
export function datapackCreatedAtSortKey(iso: string | undefined): number {
  if (iso == null || String(iso).trim() === "") return Number.NEGATIVE_INFINITY;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? t : Number.NEGATIVE_INFINITY;
}

/** Newest `created_at` first; ties keep stable order via `datapack_id` when present. */
export function sortDatapacksByCreatedAtDesc<T extends { created_at?: string; datapack_id?: string }>(
  items: readonly T[],
): T[] {
  return [...items].sort((a, b) => {
    const diff = datapackCreatedAtSortKey(b.created_at) - datapackCreatedAtSortKey(a.created_at);
    if (diff !== 0) return diff;
    return String(a.datapack_id ?? "").localeCompare(String(b.datapack_id ?? ""));
  });
}
