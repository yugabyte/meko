// "Context Match Quality" — how the retrieved results score, as a donut plus
// a High/Mid/Low legend. Second of the three Retrieval & Efficiency cards.

import { cn } from "@/lib/utils";
import { QUALITY_TONE, type QualityBand } from "@/lib/contextTones";
import { WarningBanner } from "@/components/ui/warning-banner";
import { DonutChart } from "@/components/ui/charts/DonutChart";

import { MetricCard } from "./MetricCard";

const BANDS: { band: QualityBand; label: string }[] = [
  { band: "high", label: "High" },
  { band: "mid", label: "Mid" },
  { band: "low", label: "Low" },
];

export function MatchQualityCard({
  loading,
  high,
  mid,
  low,
}: {
  loading: boolean;
  high: number;
  mid: number;
  low: number;
}) {
  const total = high + mid + low;
  const counts: Record<QualityBand, number> = { high, mid, low };
  const goodPct = total > 0 ? ((high + mid) / total) * 100 : 0;
  const lowPct = total > 0 ? (low / total) * 100 : 0;

  const banner =
    total === 0 ? undefined : goodPct >= 80 ? (
      <WarningBanner tone="success" className="px-3 py-2 text-xs">
        {Math.round(goodPct)}% of queries return High or Mid quality matches
      </WarningBanner>
    ) : (
      <WarningBanner className="px-3 py-2 text-xs">
        {Math.round(lowPct)}% of queries return Low quality matches
      </WarningBanner>
    );

  return (
    <MetricCard
      id="metric-match-quality"
      title="Context Match Quality"
      subtitle={`Score bands across ${total.toLocaleString()} results`}
      footer={banner}
      empty={
        !loading && total === 0
          ? {
              title: "No retrieval data yet",
              description: "Score bands appear once this datapack has been searched.",
            }
          : undefined
      }
    >
      {loading ? (
        <div className="space-y-2">
          <div className="h-3 w-full animate-pulse rounded bg-muted" />
          <div className="h-3 w-full animate-pulse rounded bg-muted" />
          <div className="h-3 w-full animate-pulse rounded bg-muted" />
        </div>
      ) : total === 0 ? null : (
        <div className="flex items-center gap-4">
          <DonutChart high={high} mid={mid} low={low} total={total} />
          <div className="flex-1 space-y-2">
            {BANDS.map(({ band, label }) => (
              <div key={band} className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-1.5">
                  <div className={cn("h-1 w-5 rounded-full", QUALITY_TONE[band].fill)} />
                  <span className="text-xs text-muted-foreground">{label}</span>
                </div>
                <span className="text-xs font-semibold tabular-nums">
                  {counts[band].toLocaleString()}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </MetricCard>
  );
}
