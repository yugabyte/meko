// react-query hooks for branch endpoints (compute). Kept separate from the
// core resource hooks in lib/api/queries.ts; they reuse the centralised query
// keys (`qk`) and resource calls from there.

import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query";
import { qk } from "@/lib/api/queries";
import {
  createEndpoint,
  deleteEndpoint,
  listEndpoints,
  resizeEndpoint,
  updateEndpointSettings,
  type ResizeResult,
} from "@/lib/api/resources";
import type {
  Branch,
  Endpoint,
  EndpointRole,
  SettingsUpdateResult,
  UpdateEndpointSettingsInput,
} from "@/lib/api/types";

/** Fan out across branches to fetch each one's endpoints, keyed by branch id. */
export function useBranchEndpoints(
  poolId: string | undefined,
  clusterId: string | undefined,
  branches: Branch[] | undefined,
) {
  const list = branches ?? [];
  return useQueries({
    queries: list.map((b) => ({
      queryKey: qk.endpoints(poolId ?? "", clusterId ?? "", b.branch_id),
      enabled: !!poolId && !!clusterId,
      queryFn: () => listEndpoints(poolId!, clusterId!, b.branch_id),
      select: (data: { endpoints: Endpoint[] }) => data.endpoints,
    })),
    combine: (results) => {
      const byBranch: Record<string, Endpoint[]> = {};
      results.forEach((r, i) => {
        byBranch[list[i].branch_id] = r.data ?? [];
      });
      return { byBranch, isLoading: results.some((r) => r.isLoading) };
    },
  });
}

export function useCreateEndpoint() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      role: EndpointRole;
      cpu?: string;
      memory?: string;
    }): Promise<Endpoint> =>
      createEndpoint(input.poolId, input.clusterId, input.branchId, {
        role: input.role,
        // Only send a resources block when the caller specified something; an
        // omitted block lets the controller apply its 2-core / 4Gi default.
        ...(input.cpu || input.memory
          ? { resources: { cpu: input.cpu, memory: input.memory } }
          : {}),
      }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.clusterId, input.branchId),
      });
    },
  });
}

export function useDeleteEndpoint() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      endpointId: string;
    }): Promise<void> =>
      deleteEndpoint(input.poolId, input.clusterId, input.branchId, input.endpointId),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.clusterId, input.branchId),
      });
    },
  });
}

export function useResizeEndpoint() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      endpointId: string;
      cpu?: string;
      memory?: string;
    }): Promise<ResizeResult> =>
      resizeEndpoint(input.poolId, input.clusterId, input.branchId, input.endpointId, {
        resources: { cpu: input.cpu, memory: input.memory },
      }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.clusterId, input.branchId),
      });
    },
  });
}

/** Update one endpoint's settings.
 *
 * The mutation resolves with what the controller could reach: `restarted` names
 * the services the endpoint bounced onto the new config, `unreachable` says why
 * it could not be told. The settings are stored either way, so the caller shows
 * the outcome rather than treating an unreachable endpoint as a failure.
 *
 * Only the surfaces the caller passes are sent — an omitted one is left alone,
 * `{}` clears it — so this deliberately does not default them to empty objects.
 */
export function useUpdateEndpointSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      endpointId: string;
      settings: UpdateEndpointSettingsInput;
    }): Promise<SettingsUpdateResult> =>
      updateEndpointSettings(
        input.poolId,
        input.clusterId,
        input.branchId,
        input.endpointId,
        input.settings,
      ),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.clusterId, input.branchId),
      });
    },
  });
}
