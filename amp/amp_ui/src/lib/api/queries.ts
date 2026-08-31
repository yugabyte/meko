// react-query hooks over the Controller API: one query/mutation per resource,
// plus the identity bootstrap. Query keys live in `qk` so invalidations stay
// consistent. Mutations invalidate the lists they affect so the UI stays live
// after a create.

import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  addBranchExtensions,
  createBranch,
  createCluster,
  createClusterWithWriter,
  createStoragePool,
  createUser,
  deleteBranch,
  deleteCluster,
  getVersion,
  listBranches,
  listClusters,
  listStoragePools,
  listTasks,
  listUsers,
  pollTask,
  updateBranchSettings,
} from "./resources";
import type {
  Branch,
  CreateBranchInput,
  Cluster,
  SettingsUpdateResult,
  StoragePool,
  Task,
  UpdateBranchSettingsInput,
  User,
} from "./types";

/** Centralised query keys — every hook + invalidation references these. */
export const qk = {
  version: ["version"] as const,
  currentUser: ["current-user"] as const,
  users: ["users"] as const,
  storagePools: ["storage-pools"] as const,
  clusters: (poolId: string) => ["clusters", poolId] as const,
  branches: (poolId: string, clusterId: string) =>
    ["branches", poolId, clusterId] as const,
  endpoints: (poolId: string, clusterId: string, branchId: string) =>
    ["endpoints", poolId, clusterId, branchId] as const,
  tasks: (params: TaskListParams) => ["tasks", params] as const,
};

/** The controller's build identity (GET /version). Effectively static for the
 *  life of a controller pod, so it never goes stale. */
export function useVersion() {
  return useQuery({
    queryKey: qk.version,
    queryFn: getVersion,
    staleTime: Infinity,
  });
}

/* ====================================================================
 * Identity bootstrap
 * ====================================================================
 * AMP's login is still a stub, so we map "the current user" onto the
 * controller's user list: take users[0], or auto-create one if none
 * exist. Every owner-scoped action (pool ownership, etc.) hangs off this.
 */
export function useCurrentUser() {
  return useQuery({
    queryKey: qk.currentUser,
    staleTime: Infinity,
    queryFn: async (): Promise<User> => {
      const { users } = await listUsers();
      if (users.length > 0) return users[0];
      return createUser({ is_admin: false });
    },
  });
}

/* ---- Users (tenants) ---- */

export function useUsers() {
  return useQuery({
    queryKey: qk.users,
    queryFn: listUsers,
    select: (data) => data.users,
  });
}

export function useCreateUser() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (isAdmin: boolean) => createUser({ is_admin: isAdmin }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.users });
    },
  });
}

/* ---- Storage pools (shared pools) ---- */

export function useStoragePools() {
  return useQuery({
    queryKey: qk.storagePools,
    queryFn: listStoragePools,
    select: (data) => data.storage_pools,
  });
}

export function useCreateStoragePool() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { name: string; ownerUserId: string }) =>
      createStoragePool({ name: input.name, owner_user_id: input.ownerUserId }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.storagePools });
    },
  });
}

/* ---- Clusters ---- */

// The cluster picker lists usable clusters only — the server default hides
// failed-creation orphans. The key carries the include-failed flag so the
// hidden-failed and show-all caches stay distinct (both share the qk.clusters
// prefix, which invalidations still match).
export function useClusters(poolId: string | undefined) {
  return useQuery({
    queryKey: [...qk.clusters(poolId ?? ""), false] as const,
    enabled: !!poolId,
    queryFn: () => listClusters(poolId!),
    select: (data) => data.clusters,
  });
}

/** A cluster paired with the pool it lives in — the row shape the Fleet
 *  table renders. */
export type FleetRow = { cluster: Cluster; pool: StoragePool };

/** Fan out across every pool to assemble the org-wide cluster fleet.
 *  includeFailed surfaces failed-creation orphans (an operator's fleet view, so
 *  they can be seen and deleted); the default keeps counts/pickers clean. */
export function useFleet(
  pools: StoragePool[] | undefined,
  includeFailed = false,
) {
  const list = pools ?? [];
  return useQueries({
    queries: list.map((pool) => ({
      // Key carries the flag so the show-all fleet cache stays distinct from the
      // usable-only cache the picker/counts use.
      queryKey: [...qk.clusters(pool.id), includeFailed] as const,
      queryFn: () => listClusters(pool.id, { includeFailed }),
      select: (data: { clusters: Cluster[] }) => data.clusters,
    })),
    combine: (results) => ({
      rows: results.flatMap((r, i) =>
        (r.data ?? []).map((cluster) => ({ cluster, pool: list[i] }) as FleetRow),
      ),
      isLoading: results.some((r) => r.isLoading),
      isError: results.some((r) => r.isError),
    }),
  });
}

export function useCreateCluster() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { poolId: string; name: string }) =>
      createCluster(input.poolId, { name: input.name }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({ queryKey: qk.clusters(input.poolId) });
    },
  });
}

// Composite "create cluster": fires ONE create_cluster(with_writer) task that
// creates the cluster + its main-branch write endpoint in the background, and
// returns the Task immediately (no polling). The caller toasts + deeplinks to
// Tasks; the fleet refetches when the task lands.
export function useCreateClusterWithWriter() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      name: string;
      cpu?: string;
      memory?: string;
      engine?: string;
      extensions?: string[];
    }) =>
      createClusterWithWriter(input.poolId, {
        name: input.name,
        cpu: input.cpu,
        memory: input.memory,
        engine: input.engine,
        extensions: input.extensions,
      }),
    onSuccess: (_task, input) => {
      qc.invalidateQueries({ queryKey: qk.clusters(input.poolId) });
    },
  });
}

// Delete a cluster: the controller cascades through branches to endpoints, so
// the whole subtree is stale afterwards — invalidate the cluster list AND the
// cluster's branches and every branch's endpoints (an "endpoints" prefix match),
// not just qk.clusters.
export function useDeleteCluster() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { poolId: string; clusterId: string }) =>
      deleteCluster(input.poolId, input.clusterId),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({ queryKey: qk.clusters(input.poolId) });
      qc.invalidateQueries({ queryKey: qk.branches(input.poolId, input.clusterId) });
      // Prefix of qk.endpoints(pool, cluster, *) — matches every branch's endpoints.
      qc.invalidateQueries({ queryKey: ["endpoints", input.poolId, input.clusterId] });
    },
  });
}

/* ---- Branches (timelines / forks) ---- */

export function useBranches(
  poolId: string | undefined,
  clusterId: string | undefined,
) {
  return useQuery({
    queryKey: qk.branches(poolId ?? "", clusterId ?? ""),
    enabled: !!poolId && !!clusterId,
    queryFn: () => listBranches(poolId!, clusterId!),
    select: (data) => data.branches,
  });
}

export function useCreateBranch() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      body: CreateBranchInput;
    }): Promise<Branch> =>
      createBranch(input.poolId, input.clusterId, input.body),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.branches(input.poolId, input.clusterId),
      });
    },
  });
}

/** Enable extensions on a branch.
 *
 * The controller unions them into the branch's set and redeploys every endpoint
 * on the branch to mount the new ImageVolumes, so the whole subtree goes stale:
 * the branch list carries the set, and each endpoint is replaced (the redeploy
 * recreates the pod, so its address changes). The redeploy drops connections,
 * which the caller warns about before calling.
 *
 * The POST returns 202 as soon as the task is *recorded*, before any of that has
 * happened, so invalidating there would only re-read pre-task state and nothing
 * would refetch again: the badges would miss the names just added, `enabled`
 * would leave them togglable in the picker, and each endpoint's cached address
 * would still be the pre-redeploy pod IP. Wait for the task to finish, then
 * invalidate.
 */
export function useAddBranchExtensions() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      extensions: string[];
    }): Promise<Task> =>
      addBranchExtensions(input.poolId, input.clusterId, input.branchId, {
        extensions: input.extensions,
      }),
    onSuccess: (task, input) => {
      // Deliberately not awaited: the dialog closes on the 202 rather than
      // blocking for a redeploy of every endpoint on the branch. A failed or
      // timed-out task still invalidates — it may have applied part of the set.
      void pollTask(task.id)
        .catch(() => undefined)
        .then(() => {
          qc.invalidateQueries({ queryKey: qk.branches(input.poolId, input.clusterId) });
          // Prefix of qk.endpoints(pool, cluster, *) — every endpoint on the
          // branch is redeployed, so each one's address may have changed.
          qc.invalidateQueries({ queryKey: ["endpoints", input.poolId, input.clusterId] });
        });
    },
  });
}

/** Update a branch's settings.
 *
 * Branch-wide, so the controller tells every endpoint on the branch and the
 * result carries one outcome each. A `pg_settings` change restarts each
 * endpoint's compute, which interrupts its Postgres connections — the caller is
 * responsible for saying so before it happens.
 */
export function useUpdateBranchSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      clusterId: string;
      branchId: string;
      settings: UpdateBranchSettingsInput;
    }): Promise<SettingsUpdateResult> =>
      updateBranchSettings(input.poolId, input.clusterId, input.branchId, input.settings),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.branches(input.poolId, input.clusterId),
      });
    },
  });
}

// Delete a branch: cascades to the branch's endpoints, so invalidate the
// cluster's branch list and this branch's endpoints. (A 409 from a branch that
// still has child branches rejects before any cascade — the caller surfaces it.)
export function useDeleteBranch() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { poolId: string; clusterId: string; branchId: string }) =>
      deleteBranch(input.poolId, input.clusterId, input.branchId),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.branches(input.poolId, input.clusterId),
      });
      qc.invalidateQueries({
        queryKey: qk.endpoints(input.poolId, input.clusterId, input.branchId),
      });
    },
  });
}

// Endpoint hooks (useBranchEndpoints / useCreateEndpoint) live in
// src/hooks/useEndpoints.ts; they reuse `qk.endpoints` above.

/* ---- Tasks (async operation history) ---- */

export type TaskListParams = {
  limit?: number;
  offset?: number;
  status?: string;
  kind?: string;
  clusterId?: string;
};

/** Paged task history. Keeps the previous page on screen while the next one
 *  loads (smooth paging), but only when just the page changed — a filter change
 *  (status/kind/cluster) drops the stale rows and shows the loading state. */
export function useTasks(params: TaskListParams) {
  return useQuery({
    queryKey: qk.tasks(params),
    queryFn: () => listTasks(params),
    placeholderData: (previousData, previousQuery) => {
      const prev = previousQuery?.queryKey[1] as TaskListParams | undefined;
      const sameFilter =
        prev &&
        prev.status === params.status &&
        prev.kind === params.kind &&
        prev.clusterId === params.clusterId;
      return sameFilter ? previousData : undefined;
    },
  });
}
