// The Analytics tab: Meko Cloud's datapack dashboard (what the datapack holds, what retrieval
// costs against it) from the same /metrics payload, then Meko local's own Usage section from the
// call log.

import { api } from "../lib/api";
import { bytes, count } from "../lib/format";
import { derive, formatTokens } from "../lib/metrics";
import { navigate } from "../lib/route";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { ErrorNote, Loading } from "../components/ui";
import { ActivityCard } from "../components/metrics/ActivityCard";
import { MemoryBreakdownCard } from "../components/metrics/MemoryBreakdownCard";
import { EfficiencyCard, MatchQualityCard, QualityWindowCard } from "../components/metrics/RetrievalCards";
import { SectionHeader } from "../components/metrics/SectionHeader";
import { SplitCard } from "../components/metrics/SplitCard";
import { StatPairCard } from "../components/metrics/StatPairCard";
import { AgentsCard, RecentCallsCard, ToolsCard } from "../components/metrics/UsageCards";

export function AnalyticsPage({ datapack }: { datapack: Datapack }) {
  const id = datapack.datapack_id;
  const metrics = useLoad(() => api.metrics(id), [id]);
  const usage = useLoad(() => api.usage(id), [id]);

  if (metrics.loading && !metrics.data) return <Loading />;
  if (metrics.error) return <ErrorNote error={metrics.error} />;
  const m = metrics.data;
  if (!m) return null;
  const d = derive(m, datapack);
  const refresh = () => {
    metrics.reload();
    usage.reload();
  };

  return (
    <div>
      <SectionHeader label="Meko context" onRefresh={refresh} refreshing={metrics.loading} />
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <SplitCard
          title="Context size — by token count"
          subtitle="Estimated token footprint of all stored content"
          split={d.tokens}
          format={formatTokens}
          unit="tokens"
        />
        <SplitCard
          title="Context size — by raw data size"
          subtitle="Actual storage footprint of indexed content"
          split={d.bytes}
          format={bytes}
        />
        <MemoryBreakdownCard
          promoted={d.promoted}
          privateCount={d.privateCount}
          pending={d.pending}
          onReview={() => navigate({ datapackId: id, tab: "memories" })}
        />
      </div>
      <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-2">
        <StatPairCard
          title="Knowledge base"
          subtitle="Indexed chunks, files and storage"
          stats={[
            [count(m.kb_chunk_count), "Vector chunks"],
            [count(m.kb_file_count), "Files"],
          ]}
          footnote={
            <>
              Total size: <b className="text-foreground">{bytes(m.kb_bytes)}</b>
            </>
          }
        />
        <StatPairCard
          title="Captured & extracted"
          subtitle="Conversations indexed into this datapack"
          stats={[
            [count(m.conversation_count), "Conversations"],
            [count(m.memory_extraction_count), "Memories extracted"],
          ]}
          footnote="Meko local saves memories word for word; extracting them from conversations comes later."
        />
      </div>

      <SectionHeader label="Retrieval & efficiency" onRefresh={refresh} refreshing={metrics.loading} />
      <ActivityCard activity={m.daily_activity} total={d.queries} />
      <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-3">
        <EfficiencyCard d={d} requests={m.total_requests} />
        <MatchQualityCard d={d} />
        <QualityWindowCard d={d} />
      </div>

      <SectionHeader label="Usage" onRefresh={refresh} refreshing={usage.loading} />
      {usage.error ? (
        <ErrorNote error={usage.error} />
      ) : !usage.data ? (
        <Loading />
      ) : (
        <div className="space-y-4">
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <ToolsCard usage={usage.data} />
            <AgentsCard usage={usage.data} />
          </div>
          <RecentCallsCard usage={usage.data} />
        </div>
      )}
    </div>
  );
}
