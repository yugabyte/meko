// TanStack Query hooks for the AMP cluster / branch / compute surface. Queries
// are the single cache source (no per-component useState lists); each mutation
// invalidates the exact query key it affects so dependent lists refetch on
// their own — no manual reload tokens or onChanged callbacks. All HTTP still
// goes through ampApi. Gated by SHOW_PG_AMP at the call sites.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ampCoreBudget,
  ampCoreLimitMessage,
  ampCreateBranch,
  ampCreateCluster,
  ampCreateCompute,
  ampDeleteCluster,
  ampDeleteCompute,
  ampListBranches,
  ampListClusters,
  ampListCompute,
  ampResizeCompute,
  type AmpBranch,
  type AmpCluster,
  type AmpCompute,
  type AmpCoreBudget,
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
  // The core budget is per-USER (summed across every datapack they own), not
  // per-datapack — so it is keyed WITHOUT a datapack id. Any datapack's route
  // serves the same value, and one cache entry is shared across datapacks, so a
  // create in datapack A also refreshes datapack B's greyed-button state.
  coreBudget: () => ["amp", "core-budget"] as const,
};

// A freshly created cluster/branch/compute should appear on the next nav; the
// mutations invalidate for instant feedback, so a short window is plenty.
const STALE = 30_000;

export function useClusters(datapackId: string, options?: { enabled?: boolean }) {
  return useQuery<AmpCluster[]>({
    queryKey: ampKeys.clusters(datapackId),
    queryFn: ({ signal }) => ampListClusters(datapackId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

// The user's AMP vCPU budget vs their tier cap. `at_limit` greys the create
// button; `used_cores`/`max_cores` give the compute forms their remaining-vCPU
// default sizing (see defaultCpuForBudget). Keyed per-user (datapack id used
// only to pick a route), so a mutation in any datapack invalidates the one
// shared entry. Invalidated on cluster/compute mutations to reflect freshly
// reserved cores.
export function useAmpCoreBudget(datapackId: string, options?: { enabled?: boolean }) {
  return useQuery<AmpCoreBudget>({
    queryKey: ampKeys.coreBudget(),
    queryFn: ({ signal }) => ampCoreBudget(datapackId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

// Composite create: ONE background task (project + main branch + writer) that
// api_server polls to completion; returns the created cluster. Invalidates the
// list so the new cluster shows immediately.
export function useCreateCluster(datapackId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: {
      name: string;
      cpu?: string;
      memory?: string;
      password?: string;
      engine?: string;
      extensions?: string[];
    }) => ampCreateCluster(datapackId, payload),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ampKeys.clusters(datapackId) });
      qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
    onError: (e) => {
      // A core-cap 402 means the cached budget let the request through while
      // stale; refresh it so the limit banner's "X / Y vCPU used" line is right.
      if (ampCoreLimitMessage(e)) qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
  });
}

export function useDeleteCluster(datapackId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { clusterId: string }) => ampDeleteCluster(datapackId, vars.clusterId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ampKeys.clusters(datapackId) });
      qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
  });
}

export function useBranches(
  datapackId: string,
  clusterId: string,
  options?: { enabled?: boolean },
) {
  return useQuery<AmpBranch[]>({
    queryKey: ampKeys.branches(datapackId, clusterId),
    queryFn: ({ signal }) => ampListBranches(datapackId, clusterId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

export function useCreateBranch(datapackId: string, clusterId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (payload: { name: string; parent_branch_id?: string; fork_point_lsn?: string }) =>
      ampCreateBranch(datapackId, clusterId, payload),
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
    queryFn: ({ signal }) => ampListCompute(datapackId, clusterId, branchId, { signal }),
    staleTime: STALE,
    enabled: options?.enabled ?? true,
  });
}

export function useCreateCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { role: "read_write" | "read_only"; resources?: AmpResources }) =>
      ampCreateCompute(datapackId, clusterId, branchId, vars.role, vars.resources ?? {}),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) });
      qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
    onError: (e) => {
      // Refresh the budget on a core-cap 402 so the limit banner's vCPU metric
      // isn't the stale numbers that let the request through.
      if (ampCoreLimitMessage(e)) qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
  });
}

export function useResizeCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { computeId: string; resources: AmpResources }) =>
      ampResizeCompute(datapackId, clusterId, branchId, vars.computeId, vars.resources),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) });
      qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
  });
}

export function useDeleteCompute(datapackId: string, clusterId: string, branchId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (vars: { computeId: string }) =>
      ampDeleteCompute(datapackId, clusterId, branchId, vars.computeId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ampKeys.compute(datapackId, clusterId, branchId) });
      qc.invalidateQueries({ queryKey: ampKeys.coreBudget() });
    },
  });
}
