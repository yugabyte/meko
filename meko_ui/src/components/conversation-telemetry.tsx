// Tiny shared helpers for the assistant-turn telemetry strip ("⏱ 1.2s · 🧠 3
// memories · 🖥 2 llm"). Used by the Conversations viewer and the Playground
// chat so both surfaces format the same numbers identically.
//
// Kept intentionally lean — no styling decisions live here beyond the literal
// emoji glyph; callers control sizing via `className` so each surface can
// blend the row into its own visual scale.

import { cn } from "@/lib/utils";

/**
 * Format a latency in ms for display.
 *
 *   <1s   → "950ms" (rounded; floor-clamped to 1ms)
 *   <60s  → "1.2s"  (one decimal)
 *   ≥60s  → "1m 4s" (long tool chains)
 */
export function formatLatencyLabel(latencyMs: number): string {
  if (latencyMs < 1000) {
    return `${Math.max(1, Math.round(latencyMs))}ms`;
  }
  if (latencyMs < 60_000) {
    return `${(latencyMs / 1000).toFixed(1)}s`;
  }
  const mins = Math.floor(latencyMs / 60_000);
  const secs = Math.floor((latencyMs % 60_000) / 1000);
  return `${mins}m ${secs}s`;
}

/**
 * Inline 🧠 emoji used as the "memories extracted" icon. Rendered as a
 * `<span role="img">` so it's announced cleanly by screen readers and can
 * be sized via className without distorting line-height.
 */
export function MemoryIcon({ className }: { className?: string }) {
  return (
    <span
      role="img"
      aria-label="memory"
      className={cn("inline-block leading-none", className)}
    >
      🧠
    </span>
  );
}
