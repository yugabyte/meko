/**
 * Tiny hook that re-renders every minute and returns a "5m ago" /
 * "2h ago" / "3d ago" string for an ISO timestamp. Used in the
 * conversation viewer to keep the list rail and per-message
 * timestamps current without each component owning its own timer.
 */
import { useEffect, useState } from "react";

const MINUTE_MS = 60_000;

export function formatRelativeTime(iso: string | undefined, nowMs: number): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  const diffSec = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (diffSec < 60) return "just now";
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return `${diffMin}m ago`;
  const diffHr = Math.floor(diffMin / 60);
  if (diffHr < 24) return `${diffHr}h ago`;
  const diffDay = Math.floor(diffHr / 24);
  if (diffDay < 7) return `${diffDay}d ago`;
  const diffWk = Math.floor(diffDay / 7);
  if (diffWk < 5) return `${diffWk}w ago`;
  const diffMo = Math.floor(diffDay / 30);
  // Gate the month branch on the actual day count, not on diffMo < 12.
  // 30 * 12 = 360, so diffDay in [360, 364] would produce diffMo = 12
  // (out of the months branch) but diffYr = 0 (would render "0y ago").
  if (diffDay < 365) return `${diffMo}mo ago`;
  const diffYr = Math.floor(diffDay / 365);
  return `${diffYr}y ago`;
}

/** Returns a `now` timestamp that ticks every minute. Components call
 *  `formatRelativeTime(iso, now)` to derive their display string. */
export function useNowMs(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), MINUTE_MS);
    return () => window.clearInterval(id);
  }, []);
  return now;
}
