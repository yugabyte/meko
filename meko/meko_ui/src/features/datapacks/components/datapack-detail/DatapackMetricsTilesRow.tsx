// The three overview tiles above the datapack detail tabs — total context,
// efficiency, quality — each clicking through to its section on Analytics.
// Falls back to the onboarding chips while the datapack is still empty.

import { useNavigate } from "react-router-dom";
import { Database, Sparkles, Zap } from "lucide-react";

import { EfficiencyTile } from "@/components/efficiency-tile";
import { formatTokens } from "@/lib/tokenSavings";
import { deriveDatapackMetrics } from "@/lib/datapackMetrics";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";

import { DatapackOnboardingStepper } from "./DatapackOnboardingStepper";
import { METRIC_ANCHOR } from "./metrics/metricAnchors";

export function DatapackMetricsTilesRow({
  datapackId,
  knowledgeCount,
  memoryCount,
  collectiveMemoryCount,
  onNavigateToMetric,
}: {
  datapackId: string | null;
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
  onNavigateToMetric?: (sectionId: string) => void;
}) {
  const navigate = useNavigate();
  const { data: metrics, isLoading } = useDatapackMetrics(datapackId);
  const d = deriveDatapackMetrics(metrics, {
    knowledgeCount,
    memoryCount,
    collectiveMemoryCount,
  });

  if (!isLoading && !d.hasAnyData) {
    return <DatapackOnboardingStepper datapackId={datapackId} navigate={navigate} />;
  }

  const savingsPct = d.savedPct != null ? Math.round(d.savedPct * 100) : null;
  const highMidPct = d.scores.total > 0 ? Math.round(d.qualityScore * 100) : null;

  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.(METRIC_ANCHOR.contextSize)}
      >
        <EfficiencyTile
          icon={<Database className="h-4 w-4" />}
          title="Context Size - by token count"
          accent="violet"
          loading={isLoading}
          isEmpty={!isLoading && d.tokens.total === 0}
          emptyLabel="No content indexed yet."
          headline={d.tokens.total > 0 ? `${formatTokens(d.tokens.total)} tokens` : "—"}
          subtitle="KB · memory · conversations"
          bars={[]}
        />
      </div>

      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.(METRIC_ANCHOR.retrievalSection)}
      >
        <EfficiencyTile
          icon={<Zap className="h-4 w-4" />}
          title="Context Efficiency"
          badge={d.savedPct != null ? "live" : undefined}
          accent="emerald"
          loading={isLoading}
          isEmpty={!isLoading && d.savedPct == null}
          emptyLabel="No contextual searches recorded yet."
          headline={savingsPct != null ? `${savingsPct}% saved` : "—"}
          subtitle={
            d.avgTokensSent != null && d.avgTokensIfRaw != null
              ? `~${formatTokens(d.avgTokensSent)} sent vs ~${formatTokens(d.avgTokensIfRaw)} baseline / query`
              : "vs fixed baseline per query"
          }
          bars={[]}
        />
      </div>

      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.(METRIC_ANCHOR.retrievalSection)}
      >
        <EfficiencyTile
          icon={<Sparkles className="h-4 w-4" />}
          title="Context Quality"
          accent="amber"
          loading={isLoading}
          isEmpty={!isLoading && d.scores.total === 0}
          emptyLabel="No retrieval data yet."
          headline={highMidPct != null ? `${highMidPct}% relevant` : "—"}
          subtitle={
            d.scores.total > 0
              ? `across ${d.scores.total.toLocaleString()} results`
              : "High + Mid matches"
          }
          bars={[]}
        />
      </div>
    </div>
  );
}
