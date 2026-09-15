import { useQuery } from "@tanstack/react-query";
import { getDatapackMetrics, type DatapackMetrics } from "@/lib/api/mekoApi";

export const DATAPACK_METRICS_QUERY_KEY = "datapack-metrics";

export function useDatapackMetrics(datapackId: string | null | undefined) {
  return useQuery<DatapackMetrics>({
    queryKey: [DATAPACK_METRICS_QUERY_KEY, datapackId],
    queryFn: ({ signal }) => {
      if (!datapackId) {
        throw new Error("datapackId is required to fetch metrics");
      }
      return getDatapackMetrics(datapackId, { signal });
    },
    staleTime: 0,
    enabled: Boolean(datapackId),
  });
}
