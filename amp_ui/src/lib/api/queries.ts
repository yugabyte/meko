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
  createBranch,
  createProject,
  createStoragePool,
  createUser,
  getVersion,
  listBranches,
  listProjects,
  listStoragePools,
  listTasks,
  listUsers,
} from "./resources";
import type {
  Branch,
  CreateBranchInput,
  Project,
  StoragePool,
  User,
} from "./types";

/** Centralised query keys — every hook + invalidation references these. */
export const qk = {
  version: ["version"] as const,
  currentUser: ["current-user"] as const,
  users: ["users"] as const,
  storagePools: ["storage-pools"] as const,
  projects: (poolId: string) => ["projects", poolId] as const,
  branches: (poolId: string, projectId: string) =>
    ["branches", poolId, projectId] as const,
  endpoints: (poolId: string, projectId: string, branchId: string) =>
    ["endpoints", poolId, projectId, branchId] as const,
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

/* ---- Projects (databases) ---- */

export function useProjects(poolId: string | undefined) {
  return useQuery({
    queryKey: qk.projects(poolId ?? ""),
    enabled: !!poolId,
    queryFn: () => listProjects(poolId!),
    select: (data) => data.projects,
  });
}

/** A project paired with the pool it lives in — the row shape the Fleet
 *  table renders. */
export type FleetRow = { project: Project; pool: StoragePool };

/** Fan out across every pool to assemble the org-wide database fleet. */
export function useFleet(pools: StoragePool[] | undefined) {
  const list = pools ?? [];
  return useQueries({
    queries: list.map((pool) => ({
      queryKey: qk.projects(pool.id),
      queryFn: () => listProjects(pool.id),
      select: (data: { projects: Project[] }) => data.projects,
    })),
    combine: (results) => ({
      rows: results.flatMap((r, i) =>
        (r.data ?? []).map((project) => ({ project, pool: list[i] }) as FleetRow),
      ),
      isLoading: results.some((r) => r.isLoading),
      isError: results.some((r) => r.isError),
    }),
  });
}

export function useCreateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { poolId: string; name: string }) =>
      createProject(input.poolId, { name: input.name }),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({ queryKey: qk.projects(input.poolId) });
    },
  });
}

/* ---- Branches (timelines / forks) ---- */

export function useBranches(
  poolId: string | undefined,
  projectId: string | undefined,
) {
  return useQuery({
    queryKey: qk.branches(poolId ?? "", projectId ?? ""),
    enabled: !!poolId && !!projectId,
    queryFn: () => listBranches(poolId!, projectId!),
    select: (data) => data.branches,
  });
}

export function useCreateBranch() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: {
      poolId: string;
      projectId: string;
      body: CreateBranchInput;
    }): Promise<Branch> =>
      createBranch(input.poolId, input.projectId, input.body),
    onSuccess: (_data, input) => {
      qc.invalidateQueries({
        queryKey: qk.branches(input.poolId, input.projectId),
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
  projectId?: string;
};

/** Paged task history. Keeps the previous page on screen while the next one
 *  loads (smooth paging), but only when just the page changed — a filter change
 *  (status/kind/project) drops the stale rows and shows the loading state. */
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
        prev.projectId === params.projectId;
      return sameFilter ? previousData : undefined;
    },
  });
}
