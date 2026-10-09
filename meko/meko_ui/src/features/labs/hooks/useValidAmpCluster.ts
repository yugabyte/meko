import { useEffect } from "react";

import { useClusters } from "@/features/datapacks/hooks/useAmpClusters";

import type { useLabRunnerSession } from "./useLabRunnerSession";

/**
 * Confirms a persisted `session.ampClusterId` still exists in the
 * datapack's live AMP cluster list, clearing it if not. Switching datapacks
 * is already handled by useLabRunnerSession's own eviction effect -- this
 * covers the other way a persisted choice goes stale: the cluster is
 * deleted from the datapack detail page (ClusterRow.tsx) while this lab
 * isn't open to see it happen, so datapackId never changes and that effect
 * never fires. Both CreateAmpClusterStage and ReviewDashboardStage need this
 * same check before trusting `session.ampClusterId`, hence a shared hook.
 */
export function useValidAmpCluster(
  datapackId: string,
  session: ReturnType<typeof useLabRunnerSession>,
) {
  const clustersQuery = useClusters(datapackId);
  const { data: clusters, isLoading } = clustersQuery;
  const { ampClusterId, clearAmpCluster } = session;

  useEffect(() => {
    if (isLoading || !ampClusterId || !clusters) return;
    if (!clusters.some((cluster) => cluster.id === ampClusterId)) clearAmpCluster();
  }, [isLoading, ampClusterId, clusters, clearAmpCluster]);

  return clustersQuery;
}
