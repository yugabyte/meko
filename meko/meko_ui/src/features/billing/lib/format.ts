import type { PlanLimit } from "@/features/billing/constants/plans";

/**
 * Compact rendering for unit-less limits like "datapacks", "collaborators",
 * or "memory ops / mo". Sentinel values become "Custom" / "Unlimited".
 *
 * `perMonth` appends "/mo" only when the caller is rendering a monthly
 * metric (memory ops, MCP calls) so plain counts (datapacks, seats) stay
 * un-suffixed.
 */
export function formatLimitNumeric(
  value: PlanLimit,
  opts: { perMonth?: boolean } = {},
): string {
  if (value === "custom") return "Custom";
  if (value === "unlimited") return "Unlimited";
  return `${compactNumber(value)}${opts.perMonth ? "/mo" : ""}`;
}

/** Storage cap (MB → "100 MB" / "2 GB" / "500 GB"). */
export function formatStorageLimit(mb: PlanLimit): string {
  if (mb === "custom") return "Custom";
  if (mb === "unlimited") return "Unlimited";
  if (mb < 1024) return `${mb} MB`;
  const gb = mb / 1024;
  return `${gb % 1 === 0 ? gb.toFixed(0) : gb.toFixed(1)} GB`;
}

/** Day count, but Free's "1 day" is special-cased to "24 hours" to
 *  match the marketing-page wording for decision traces. */
export function formatDays(value: PlanLimit, opts: { hoursWhenOne?: boolean } = {}): string {
  if (value === "custom") return "Configurable";
  if (value === "unlimited") return "Unlimited";
  if (value === 1 && opts.hoursWhenOne) return "24 hours";
  return `${value} days`;
}

/** 1_000 → "1K", 1_500 → "1.5K", 1_000_000 → "1M", etc. */
export function compactNumber(n: number): string {
  if (n >= 1_000_000) {
    const m = n / 1_000_000;
    return `${m % 1 === 0 ? m.toFixed(0) : m.toFixed(1)}M`;
  }
  if (n >= 1_000) {
    const k = n / 1_000;
    return `${k % 1 === 0 ? k.toFixed(0) : k.toFixed(0)}K`;
  }
  return String(n);
}
