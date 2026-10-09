// Replays a captured agent trace one line at a time. The pacing is the
// point: the user watches the agent reach "context sources available: none"
// before it reaches its verdict, which is what makes the verdict land.
//
// A replay, not a live stream, and the caller labels it as one. Honours
// prefers-reduced-motion by showing every line at once.

import { useEffect, useState } from "react";

import { cn } from "@/lib/utils";

import type { ColdReviewTraceLine } from "../../data/adlcReviewAgentLab/coldReviewCapture";

/** Wall-clock between lines. Short enough that nobody waits on a lesson,
 *  long enough to read each line as it lands. */
const LINE_DELAY_MS = 520;

const TONE_CLASS: Record<NonNullable<ColdReviewTraceLine["tone"]>, string> = {
  warn: "text-primary",
  ok: "text-context-knowledge",
};

export function ColdReviewTrace({
  lines,
  onFinished,
}: {
  lines: ColdReviewTraceLine[];
  /** Fired once the last line is on screen, so the caller can reveal what
   *  the trace was building up to. */
  onFinished?: () => void;
}) {
  const reduced =
    typeof window !== "undefined" &&
    window.matchMedia?.("(prefers-reduced-motion: reduce)").matches === true;

  const [shown, setShown] = useState(() => (reduced ? lines.length : 0));

  useEffect(() => {
    if (shown >= lines.length) {
      onFinished?.();
      return;
    }
    const id = window.setTimeout(() => setShown((n) => n + 1), reduced ? 0 : LINE_DELAY_MS);
    return () => window.clearTimeout(id);
  }, [shown, lines.length, reduced, onFinished]);

  return (
    <div
      // Announced as a whole once it settles rather than per line, so a
      // screen reader isn't read a five-line countdown.
      aria-live="polite"
      aria-busy={shown < lines.length}
      className="flex flex-col gap-1.5 rounded-md border border-border bg-card p-4 font-mono text-xs"
    >
      {lines.slice(0, shown).map((line) => (
        <div key={line.at + line.text} className="flex gap-4">
          <span className="w-10 shrink-0 tabular-nums text-muted-foreground">{line.at}</span>
          <span
            className={cn("min-w-0 flex-1", line.tone ? TONE_CLASS[line.tone] : "text-foreground")}
          >
            {line.text}
          </span>
        </div>
      ))}
    </div>
  );
}
