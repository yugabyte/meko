// "Memory Breakdown" — promoted vs private vs pending, for the first metrics
// row. Follows the same rhythm as the two Total Context cards beside it
// (headline number, then bar, then legend) so the three bars sit on one line
// instead of this one starting where the others' numbers are.

import { cn } from "@/lib/utils";
import { CONTEXT_TONE } from "@/lib/contextTones";
import { WarningBanner } from "@/components/ui/warning-banner";

import { MetricCard, MetricStat, MetricStatDivider, MetricStatRow } from "./MetricCard";

/** Promoted is collective (sky), private is a plain memory (blue), and a
 *  pending item is a learning awaiting a human (purple) — the same three hues
 *  the detail page's pipeline uses for those words. */
const SEGMENTS = [
  { key: "promoted", label: "Promoted", concept: "collective" },
  { key: "private", label: "Private", concept: "memory" },
  { key: "pending", label: "Pending", concept: "learning" },
] as const;

export function MemoryBreakdownCard({
  promoted,
  privateCount,
  pending,
  onReviewPending,
}: {
  promoted: number;
  privateCount: number;
  pending: number;
  /** Absent when there is nothing to review, or no datapack to navigate to. */
  onReviewPending?: () => void;
}) {
  const counts = { promoted, private: privateCount, pending };
  const barTotal = promoted + privateCount + pending;
  const totalMemories = promoted + privateCount;
  const sharedPct = totalMemories > 0 ? Math.round((promoted / totalMemories) * 100) : 0;

  const pendingBanner =
    pending > 0 && onReviewPending ? (
      <button type="button" onClick={onReviewPending} className="w-full text-left">
        <WarningBanner className="px-3 py-2 text-xs transition-colors hover:brightness-105">
          {pending} {pending === 1 ? "memory" : "memories"} pending review — promote to share with
          all agents
        </WarningBanner>
      </button>
    ) : undefined;

  return (
    <MetricCard
      title="Memory Breakdown"
      subtitle="Shared vs private vs pending promotion"
      footer={pendingBanner}
    >
      {barTotal === 0 ? (
        <p className="text-xs text-muted-foreground">No memories yet.</p>
      ) : (
        <>
          <MetricStatRow>
            <MetricStat value={totalMemories.toLocaleString()} label="Total memories" />
            {totalMemories > 0 && (
              <>
                <MetricStatDivider />
                <MetricStat value={`${sharedPct}%`} label="Shared" />
              </>
            )}
          </MetricStatRow>

          <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
            {SEGMENTS.map((s) => (
              <div
                key={s.key}
                className={CONTEXT_TONE[s.concept].fill}
                style={{ width: `${(counts[s.key] / barTotal) * 100}%` }}
              />
            ))}
          </div>

          <div className="flex flex-wrap gap-x-4 gap-y-1">
            {SEGMENTS.filter((s) => s.key !== "pending" || pending > 0).map((s) => (
              <div key={s.key} className="flex items-center gap-1.5">
                <div className={cn("h-2.5 w-2.5 shrink-0 rounded-full", CONTEXT_TONE[s.concept].fill)} />
                <span className="text-xs text-muted-foreground">{s.label}</span>
                <span className="text-xs font-semibold tabular-nums text-foreground">
                  {counts[s.key]}
                </span>
              </div>
            ))}
          </div>
        </>
      )}
    </MetricCard>
  );
}
