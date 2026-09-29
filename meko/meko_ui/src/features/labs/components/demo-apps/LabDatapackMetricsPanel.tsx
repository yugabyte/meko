// The datapack's own Analytics tab, rendered inside the lab.
//
// Reuses DatapackMetricsSection wholesale rather than drawing a second,
// smaller set of charts: the point of the Metrics tab in Chapter 3 is that
// what the lesson just built shows up in the same surface the product uses,
// so a divergent copy here would undercut the claim. Only the four counts
// that section needs are fetched, from the same getDatapack call the detail
// page makes.

import { useQuery } from "@tanstack/react-query";

import { getDatapack } from "@/lib/api/mekoApi";
import { DatapackMetricsSection } from "@/features/datapacks/components/datapack-detail/DatapackMetricsSection";

export function LabDatapackMetricsPanel({ datapackId }: { datapackId: string | null }) {
  const { data } = useQuery({
    queryKey: ["lab-datapack-counts", datapackId],
    queryFn: ({ signal }) => getDatapack(datapackId as string, { signal }),
    enabled: Boolean(datapackId),
    staleTime: 30_000,
  });

  if (!datapackId) {
    return (
      <p className="text-xs text-muted-foreground">
        Pick a datapack to see what it has built up.
      </p>
    );
  }

  return (
    <DatapackMetricsSection
      datapackId={datapackId}
      knowledgeCount={data?.knowledge_count ?? 0}
      memoryCount={data?.memory_count ?? 0}
      collectiveMemoryCount={data?.collective_memory_count ?? 0}
      learningsCount={data?.learnings_count ?? 0}
    />
  );
}
