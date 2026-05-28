/**
 * Tiny helpers for "X ago" relative timestamps. Used across the
 * conversations viewer, the SharedKnowledge activity log, and any
 * other surface that wants its timestamps to stay current without
 * each component owning its own timer.
 *
 * Usage:
 *   const now = useNowMs();
 *   const label = formatRelativeTime(iso, now);
 *
 * `useNowMs` ticks once a minute (cheap, single re-render across all
 * consumers in the same tree). `formatRelativeTime` is a pure function
 * so callers can also feed it a fixed `nowMs` in tests.
 */
import { useEffect, useState } from "react";

const MINUTE_MS = 60_000;

/**
 * @param localTime - When true, strips the UTC offset/Z suffix before parsing
 *   so the browser interprets the value in its local timezone. Use this when
 *   the backend returns a local-time value that is incorrectly labelled as UTC.
 */
export function formatRelativeTime(
  iso: string | undefined | null,
  nowMs: number,
  localTime = false,
): string {
  if (!iso) return "—";
  const toParse = localTime ? iso.replace(/Z$|[+-]\d{2}:\d{2}$/, "") : iso;
  const t = Date.parse(toParse);
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
