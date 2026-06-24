// Byte-quantity helpers — display formatting + threshold builders.
//
// One file (the established `fileSize` name) covers two concerns that
// previously had three implementations spread across the codebase:
//
//   1. `formatFileSize(bytes)` — human-readable size string for the UI.
//      Used by KB upload + document tables. Accepts null/undefined so
//      callers passing optional fields don't need their own guard.
//
//   2. `MB(n)` / `GB(n)` and the underlying `KiB / MiB / GiB` constants —
//      threshold builders for code that compares against caps (e.g.
//      `lib/freeTier.ts`'s `MB(100)` Free-tier KB-storage limit).
//
// Naming: we use the conventional "KB / MB / GB" labels in display
// strings even though the underlying math is power-of-two (KiB / MiB /
// GiB) — the UI audience doesn't distinguish, and "MiB" looks like a
// typo. Internal constants keep the technically-correct names so code
// reading them isn't surprised by 1024.

/** Bytes per kibibyte (1024). Power-of-two convention — matches what
 *  filesystems and most "MB" UI affordances actually mean. */
export const KiB = 1024;
/** Bytes per mebibyte (1024^2 = 1_048_576). */
export const MiB = KiB * 1024;
/** Bytes per gibibyte (1024^3). */
export const GiB = MiB * 1024;

/** Threshold builder — `MB(100)` → 100 * MiB bytes. Used for cap
 *  comparisons where you want to write "100 MB" in the source rather
 *  than the raw byte count. */
export function MB(n: number): number {
  return n * MiB;
}

/** Threshold builder — `GB(2)` → 2 * GiB bytes. */
export function GB(n: number): number {
  return n * GiB;
}

// Reuse formatter instance instead of creating one each call.
const numberFormatter = new Intl.NumberFormat();

function withUnit(value: number, unit: "KB" | "MB" | "GB"): string {
  if (value < 10) {
    return `${value.toFixed(1)} ${unit}`;
  }
  return `${Math.round(value)} ${unit}`;
}

/**
 * Human-readable byte count. Picks the largest unit that keeps the
 * leading number ≥ 1, then renders with 1 decimal place when small
 * (< 10) and whole number when large — matches the "X.X KB" /
 * "X MB" feel users expect from file-size affordances.
 *
 *   formatFileSize(0)            → "0 B"
 *   formatFileSize(900)          → "900 B"
 *   formatFileSize(2048)         → "2.0 KB"
 *   formatFileSize(3_500_000)    → "3.3 MB"
 *   formatFileSize(2_000_000_000)→ "1.9 GB"
 *
 * Accepts `null` / `undefined` (returns "—") so callers passing an
 * optional `doc.size_bytes` don't need their own null guard at every
 * call site. Negative / non-finite values also render as "—".
 */
export function formatFileSize(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes) || bytes < 0) return "—";

  const roundedBytes = Math.round(bytes);
  if (roundedBytes < KiB) return `${numberFormatter.format(roundedBytes)} B`;
  if (roundedBytes < MiB) return withUnit(roundedBytes / KiB, "KB");
  if (roundedBytes < GiB) return withUnit(roundedBytes / MiB, "MB");
  return withUnit(roundedBytes / GiB, "GB");
}
