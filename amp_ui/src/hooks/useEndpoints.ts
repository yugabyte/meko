// react-query hooks for branch endpoints (compute). Kept separate from the
// core resource hooks in lib/api/queries.ts; they reuse the centralised query
// keys (`qk`) and resource calls from there.

import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query";
import { qk } from "@/lib/api/queries";
import {
  createEndpoint,
  listEndpoints,
  resizeEndpoint,
  type ResizeResult,
} from "@/lib/api/resources";
import type { Branch, Endpoint, EndpointRole } from "@/lib/api/types";

/** Fan out across branches to fetch each one's endpoints, keyed by branch id. */
export function useBranchEndpoints(
  poolId: string | undefined,
  projectId: string | undefined,
  branches: Branch[] | undefined,
) {
  const list = branches ?? [];
  return useQueries({
    queries: list.map((b) => ({
      queryKey: qk.endpoints(poolId ?? "", projectId ?? "", b.branch_id),
      enabled: !!poolId && !!projectId,
      queryFn: () => listEndpoints(poolId!, projectId!, b.branch_id),
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
      projectId: string;
      branchId: string;
      role: EndpointRole;
      cpu?: string;
      memory?: string;
    }): Promise<Endpoint> =>
      createEndpoint(input.poolId, input.projectId, input.branchId, {
        role: input.role,
        // Only send a resources block when the caller specified something; an
        // omitted block lets the controller apply its 2-core / 4Gi default.
        ...(input.cpu || input.memory
          ? { resources: { cpu: input.cpu, memory: input.memory } }
          : {}),
      }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.projectId, input.branchId),
      });
    },
  });
}

export function useResizeEndpoint() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      projectId: string;
      branchId: string;
      endpointId: string;
      cpu?: string;
      memory?: string;
    }): Promise<ResizeResult> =>
      resizeEndpoint(input.poolId, input.projectId, input.branchId, input.endpointId, {
        resources: { cpu: input.cpu, memory: input.memory },
      }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.projectId, input.branchId),
      });
    },
  });
}
