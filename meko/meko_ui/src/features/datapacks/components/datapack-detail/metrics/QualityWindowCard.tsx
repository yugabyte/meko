// "Quality vs Window Use" — two arc gauges and a verdict. Third of the three
// Retrieval & Efficiency cards; the gauges sit in a centred, flex-1 band so
// they land at the same height as the donut and the compression bar beside
// them, and the verdict is pinned to the footer like the other banners.

import { formatTokens } from "@/lib/tokenSavings";
import { contextColorVar, qualityColorVar } from "@/lib/contextTones";
import { WarningBanner } from "@/components/ui/warning-banner";
import { ArcGauge } from "@/components/ui/charts/ArcGauge";

import { MetricCard } from "./MetricCard";

/** What the model is given per query, as a share of this window. */
const CONTEXT_WINDOW_LABEL = "of 128K";

function verdict(qualityScore: number): { headline: string; detail: string; good: boolean } {
  if (qualityScore > 0.8) {
    return {
      headline: "Strong retrieval",
      detail: "High + Mid quality matches — retrieval is working well.",
      good: true,
    };
  }
  if (qualityScore > 0.5) {
    return {
      headline: "Moderate",
      detail: "Moderate matches. Consider refining your knowledge base content.",
      good: false,
    };
  }
  return {
    headline: "Needs attention",
    detail: "Low match quality. Review and improve indexed content.",
    good: false,
  };
}

export function QualityWindowCard({
  loading,
  qualityScore,
  hasScores,
  windowUsed,
  avgTokensSent,
}: {
  loading: boolean;
  /** (High + Mid) / total, 0–1. */
  qualityScore: number;
  hasScores: boolean;
  /** Avg tokens sent as a share of the context window, 0–1, or null. */
  windowUsed: number | null;
  avgTokensSent: number | null;
}) {
  const v = verdict(qualityScore);

  // Nothing has been queried at all: two dashed gauges over a status banner
  // in a tone that means nothing is worse than simply saying so.
  const isEmpty = !loading && !hasScores && windowUsed == null;

  const footer =
    loading || !hasScores ? undefined : (
      <WarningBanner tone={v.good ? "success" : "warning"} className="px-3 py-2 text-xs">
        <p className="font-semibold">{v.headline}</p>
        <p className="mt-0.5 opacity-80">{v.detail}</p>
      </WarningBanner>
    );

  return (
    <MetricCard
      title="Quality vs Window Use"
      subtitle="Match score and context window utilization"
      footer={footer}
      empty={
        isEmpty
          ? {
              title: "No queries yet",
              description: "Run queries against this datapack to see match quality.",
            }
          : undefined
      }
    >
      {loading ? (
        <div className="flex flex-1 justify-center gap-6">
          <div className="h-20 w-20 animate-pulse rounded-full bg-muted" />
          <div className="h-20 w-20 animate-pulse rounded-full bg-muted" />
        </div>
      ) : (
        <div className="flex flex-1 items-center justify-center gap-4">
          <ArcGauge
            value={hasScores ? qualityScore : null}
            label="Match score"
            color={qualityColorVar("high")}
            pill={hasScores ? "High + Mid" : undefined}
            tooltip="% of queries that returned High or Mid quality matches. High = strong semantic match; Mid = relevant but less precise."
          />
          <ArcGauge
            value={windowUsed}
            label="Window used"
            color={contextColorVar("memory")}
            pill={windowUsed != null ? CONTEXT_WINDOW_LABEL : undefined}
            tooltip={`Average number of tokens sent to the model as a percentage of a 128K context window. Lower is better; Meko keeps this small to maximize token efficiency.${
              avgTokensSent != null
                ? ` Current average: ~${formatTokens(avgTokensSent)} tokens per query.`
                : ""
            }`}
          />
        </div>
      )}
    </MetricCard>
  );
}
