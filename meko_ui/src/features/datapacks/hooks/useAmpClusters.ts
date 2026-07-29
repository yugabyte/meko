// TanStack Query hooks for the AMP cluster / branch / compute surface. Queries
// are the single cache source (no per-component useState lists); each mutation
// invalidates the exact query key it affects so dependent lists refetch on
// their own — no manual reload tokens or onChanged callbacks. All HTTP still
// goes through ampApi. Gated by SHOW_PG_AMP at the call sites.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createBranch,
  createCluster,
  createCompute,
  deleteCompute,
  listBranches,
  listClusters,
  listCompute,
  resizeCompute,
  type AmpBranch,
  type AmpCluster,
  type AmpCompute,
  type AmpResources,
} from "@/lib/api/ampApi";

// Hierarchical query keys so a mutation can invalidate the precise list it
// changed (compute under a branch, branches under a cluster, …).
export const ampKeys = {
  clusters: (datapackId: string) => ["amp", "clusters", datapackId] as const,
  branches: (datapackId: string, clusterId: string) =>
    ["amp", "branches", datapackId, clusterId] as const,
  compute: (datapackId: string, clusterId: string, branchId: string) =>
    ["amp", "compute", datapackId, clusterId, branchId] as const,
};

// A freshly created cluster/branch/compute should appear on the next nav; the
// mutations invalidate for instant feedback, so a short window is plenty.
const STALE = 30_000;

export function useClusters(datapackId: string, options?: { enabled?: boolean }) {
  return useQuery<AmpCluster[]>({
    queryKey: ampKeys.clusters(datapackId),
    queryFn: ({ signal }) => listClusters(datapackId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

export function useCreateCluster(datapackId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: { name: string; pg_version?: number }) => createCluster(datapackId, payload),
    onSuccess: () => qc.invalidateQueries({ queryKey: ampKeys.clusters(datapackId) }),
  });
}

export function useBranches(datapackId: string, clusterId: string, options?: { enabled?: boolean }) {
  return useQuery<AmpBranch[]>({
    queryKey: ampKeys.branches(datapackId, clusterId),
    queryFn: ({ signal }) => listBranches(datapackId, clusterId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

export function useCreateBranch(datapackId: string, clusterId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: { name: string; parent_branch_id?: string; fork_point_lsn?: string }) =>
      createBranch(datapackId, clusterId, payload),
    onSuccess: () => qc.invalidateQueries({ queryKey: ampKeys.branches(datapackId, clusterId) }),
  });
}

export function useCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  options?: { enabled?: boolean },
) {
  return useQuery<AmpCompute[]>({
    queryKey: ampKeys.compute(datapackId, clusterId, branchId),
    queryFn: ({ signal }) => listCompute(datapackId, clusterId, branchId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

export function useCreateCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { role: "read_write" | "read_only"; resources?: AmpResources }) =>
      createCompute(datapackId, clusterId, branchId, vars.role, vars.resources ?? {}),
    onSuccess: () => qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) }),
  });
}

export function useResizeCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { computeId: string; resources: AmpResources }) =>
      resizeCompute(datapackId, clusterId, branchId, vars.computeId, vars.resources),
    onSuccess: () => qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) }),
  });
}

export function useDeleteCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { computeId: string }) =>
      deleteCompute(datapackId, clusterId, branchId, vars.computeId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) }),
  });
}
