// AMP cluster API calls, kept separate from the core mekoApi surface. A
// "cluster" is Meko's user-facing name for an AMP project (a Postgres
// database). Gated by SHOW_PG_AMP at the call sites; reuses mekoApi's `request`
// (envelope unwrap + error handling) and the standard Meko API base.
//
// NOTE: response field names (project_id, storage_pool_id, …) mirror the AMP
// controller payload that api_server passes through unchanged, so they keep the
// AMP terminology even though the Meko-facing concept is a "cluster".

import { request } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

function clustersRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/clusters`;
}

/** A branch (Postgres timeline) of an AMP cluster; `main` is the default. */
export type AmpBranch = {
  branch_id: string;
  project_id: string;
  name: string;
  pg_version: number;
  is_default: boolean;
  fork_lsn?: string;
  created_at: string;
};

/** An AMP cluster (a Postgres database / AMP project) inside the datapack's shared pool. */
export type AmpCluster = {
  id: string;
  name: string;
  storage_pool_id: string;
  default_branch: AmpBranch;
  created_at: string;
};

/** GET /datapacks/:id/clusters — the datapack's AMP clusters ([] if none). */
export async function listClusters(datapackId: string, options: RouteOptions = {}) {
  return request<AmpCluster[]>(clustersRoute(datapackId), "GET", undefined, options);
}

/** POST /datapacks/:id/clusters — create a cluster (provisions the shared
 *  pool on first use). */
export async function createCluster(
  datapackId: string,
  payload: { name: string; pg_version?: number },
) {
  const body: Record<string, unknown> = { name: payload.name.trim() };
  if (payload.pg_version != null) body.pg_version = payload.pg_version;
  return request<AmpCluster>(clustersRoute(datapackId), "POST", body);
}

/** A compute instance bound to a branch (what a client connects to). address is
 *  host:port, empty until deployed. */
export type AmpCompute = {
  id: string;
  project_id: string;
  branch_id: string;
  role: string;
  address: string;
  created_at: string;
};

function clusterRoute(datapackId: string, clusterId: string): string {
  return `${clustersRoute(datapackId)}/${encodeURIComponent(clusterId)}`;
}

/** GET a cluster's branch tree. */
export async function listBranches(
  datapackId: string,
  clusterId: string,
  options: RouteOptions = {},
) {
  return request<AmpBranch[]>(`${clusterRoute(datapackId, clusterId)}/branches`, "GET", undefined, options);
}

/** POST a new branch (fork). fork_point_lsn is required when the parent has no
 *  running compute for the controller to read the latest LSN from. */
export async function createBranch(
  datapackId: string,
  clusterId: string,
  payload: { name: string; parent_branch_id?: string; fork_point_lsn?: string },
) {
  const body: Record<string, unknown> = { name: payload.name.trim() };
  if (payload.parent_branch_id) body.parent_branch_id = payload.parent_branch_id;
  if (payload.fork_point_lsn?.trim()) body.fork_point_lsn = payload.fork_point_lsn.trim();
  return request<AmpBranch>(`${clusterRoute(datapackId, clusterId)}/branches`, "POST", body);
}

/** GET the compute instances on a branch. */
export async function listCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  options: RouteOptions = {},
) {
  return request<AmpCompute[]>(
    `${clusterRoute(datapackId, clusterId)}/branches/${encodeURIComponent(branchId)}/compute`,
    "GET",
    undefined,
    options,
  );
}

/** POST to provision a compute instance on a branch. */
export async function createCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  role: "read_write" | "read_only",
) {
  return request<AmpCompute>(
    `${clusterRoute(datapackId, clusterId)}/branches/${encodeURIComponent(branchId)}/compute`,
    "POST",
    { role },
  );
}
