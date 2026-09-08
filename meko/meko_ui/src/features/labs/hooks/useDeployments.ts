// TanStack Query hooks for the agent-deployment surface, mirroring
// useAmpClusters.ts's shape: mutations invalidate/seed the query cache
// directly, and useDeploymentStatus polls GET /deployments/:id at the same
// 2s-ish cadence useKnowledgeFilesTable.ts uses for in-progress work,
// stopping once the deployment reaches a terminal status.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  confirmByocDeployment,
  createManagedDeployment,
  getDeployment,
  getSharedDeployment,
  initByocDeployment,
  type Deployment,
} from "@/lib/api/deploymentsApi";

export const deploymentKeys = {
  detail: (id: string) => ["deployments", id] as const,
  shared: (agentSlug: string) => ["deployments", "shared", agentSlug] as const,
};

export function useInitByocDeployment() {
  return useMutation({
    mutationFn: (agentSlug: string) => initByocDeployment(agentSlug),
  });
}

export function useConfirmByocDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { id: string; roleArn: string }) => confirmByocDeployment(vars.id, vars.roleArn),
    onSuccess: (deployment) => qc.setQueryData(deploymentKeys.detail(deployment.id), deployment),
  });
}

export function useCreateManagedDeployment() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (agentSlug: string) => createManagedDeployment(agentSlug),
    onSuccess: (deployment) => qc.setQueryData(deploymentKeys.detail(deployment.id), deployment),
  });
}

const TERMINAL_STATUSES: Deployment["status"][] = ["active", "failed", "deleted"];

export function useDeploymentStatus(id: string | null) {
  return useQuery({
    queryKey: deploymentKeys.detail(id ?? ""),
    queryFn: ({ signal }) => getDeployment(id as string, { signal }),
    enabled: Boolean(id),
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status && TERMINAL_STATUSES.includes(status) ? false : 3000;
    },
  });
}

/** Status of agentSlug's shared deployment -- one entry in the farm of
 *  pre-deployed instances every signed-in user can use. `enabled` lets
 *  callers skip fetching/polling entirely when the user isn't in "shared"
 *  mode or agentSlug isn't known yet. 404s (no farm entry for this agent)
 *  surface as an error, same as any other query failure -- callers show
 *  that as "unavailable". */
export function useSharedDeploymentStatus(agentSlug: string | null, enabled: boolean) {
  return useQuery({
    queryKey: deploymentKeys.shared(agentSlug ?? ""),
    queryFn: ({ signal }) => getSharedDeployment(agentSlug as string, { signal }),
    enabled: enabled && Boolean(agentSlug),
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status && TERMINAL_STATUSES.includes(status) ? false : 3000;
    },
  });
}
