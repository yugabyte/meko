// The datapack Analytics tab: what this datapack holds, what retrieval costs
// against it, and who reads it. Composes the cards in metrics/ — the numbers
// all come from one deriveDatapackMetrics call so no two cards can disagree.

import { useNavigate } from "react-router-dom";

import { formatFileSize } from "@/lib/fileSize";
import { formatTokens } from "@/lib/tokenSavings";
import { useNowMs } from "@/lib/relativeTime";
import { deriveDatapackMetrics } from "@/lib/datapackMetrics";
import { datapackLearningsPath } from "@/app/routes/paths";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";

import { ActivityCard } from "./metrics/ActivityCard";
import { AgentsUsersCard } from "./metrics/AgentsUsersCard";
import { ContextEfficiencyCard } from "./metrics/ContextEfficiencyCard";
import { MatchQualityCard } from "./metrics/MatchQualityCard";
import { MemoryBreakdownCard } from "./metrics/MemoryBreakdownCard";
import { MetricsSectionHeader } from "./metrics/MetricsSectionHeader";
import { METRIC_ANCHOR } from "./metrics/metricAnchors";
import { QualityWindowCard } from "./metrics/QualityWindowCard";
import { TotalContextCard } from "./metrics/TotalContextCard";
import { CapturedExtractedCard, KnowledgeBaseCard } from "./metrics/KnowledgeStatsCards";

type Props = {
  datapackId: string | null;
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
  learningsCount: number;
};

export function DatapackMetricsSection({
  datapackId,
  knowledgeCount,
  memoryCount,
  collectiveMemoryCount,
  learningsCount,
}: Props) {
  const { data: metrics, isLoading, isFetching, refetch } = useDatapackMetrics(datapackId);
  const nowMs = useNowMs();
  const navigate = useNavigate();

  const d = deriveDatapackMetrics(metrics, {
    knowledgeCount,
    memoryCount,
    collectiveMemoryCount,
  });

  const activity = metrics?.daily_activity ?? [];
  const totalQueryCount = activity.reduce((s, day) => s + day.hybrid + day.kb + day.memory, 0);

  return (
    <div>
      <MetricsSectionHeader
        label="Meko Context"
        onRefresh={() => void refetch()}
        refreshing={isFetching}
      />

      <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
        <TotalContextCard
          id={METRIC_ANCHOR.contextSize}
          title="Context Size - by token count"
          subtitle="Estimated token footprint of all stored content"
          breakdown={d.tokens}
          format={formatTokens}
          unitSuffix="tokens"
          emptyLabel="No context loaded yet."
        />
        <TotalContextCard
          title="Context Size - by raw data size"
          subtitle="Actual storage footprint of indexed content"
          breakdown={d.bytes}
          format={formatFileSize}
          emptyLabel="No data available yet."
        />
        <MemoryBreakdownCard
          promoted={collectiveMemoryCount}
          privateCount={Math.max(0, memoryCount - collectiveMemoryCount)}
          pending={learningsCount}
          onReviewPending={
            datapackId ? () => navigate(datapackLearningsPath(datapackId)) : undefined
          }
        />
      </div>

      <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
        <KnowledgeBaseCard
          loading={isLoading}
          chunkCount={metrics?.kb_chunk_count ?? null}
          fileCount={metrics?.kb_file_count ?? null}
          bytes={metrics?.kb_bytes ?? null}
        />
        <CapturedExtractedCard
          loading={isLoading}
          conversationCount={metrics?.conversation_count ?? null}
          extractedCount={metrics?.memory_extraction_count ?? null}
          extractionRate={metrics?.extraction_rate ?? null}
        />
      </div>

      <MetricsSectionHeader
        id={METRIC_ANCHOR.retrievalSection}
        label="Retrieval & Efficiency"
        className="mt-8"
        onRefresh={() => void refetch()}
        refreshing={isFetching}
      />

      <div className="mb-4" id={METRIC_ANCHOR.queryActivity}>
        <ActivityCard
          activity={activity}
          totalQueryCount={totalQueryCount}
          isLoading={isLoading}
          title="Context Queries"
        />
      </div>

      <div className="grid grid-cols-1 items-stretch gap-4 md:grid-cols-3">
        <ContextEfficiencyCard
          loading={isLoading}
          savedPct={d.savedPct}
          avgTokensSent={d.avgTokensSent}
          avgTokensIfRaw={d.avgTokensIfRaw}
          avgTokensSaved={d.avgTokensSaved}
          totalRequests={metrics?.total_requests ?? 0}
          activity={activity}
        />
        <MatchQualityCard
          loading={isLoading}
          high={d.scores.high}
          mid={d.scores.mid}
          low={d.scores.low}
        />
        <QualityWindowCard
          loading={isLoading}
          qualityScore={d.qualityScore}
          hasScores={d.scores.total > 0}
          windowUsed={d.windowUsed}
          avgTokensSent={d.avgTokensSent}
        />
      </div>

      <MetricsSectionHeader label="Usage" className="mt-8" />

      <AgentsUsersCard
        loading={isLoading}
        agentCount={metrics?.agent_count ?? null}
        humanCount={metrics?.human_count ?? null}
        lastAccessedAt={metrics?.last_accessed_at}
        nowMs={nowMs}
        className="md:w-1/2"
      />
    </div>
  );
}

export { DatapackMetricsTilesRow } from "./DatapackMetricsTilesRow";
