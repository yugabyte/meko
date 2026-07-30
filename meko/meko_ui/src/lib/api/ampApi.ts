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

/** Compute sizing reserved for an endpoint (Kubernetes quantities): cpu e.g.
 *  "2"/"1500m", memory e.g. "4Gi". */
export type AmpResources = { cpu?: string; memory?: string };

/** A compute instance bound to a branch (what a client connects to). address is
 *  host:port, empty until deployed. */
export type AmpCompute = {
  id: string;
  project_id: string;
  branch_id: string;
  role: string;
  address: string;
  /** Tailscale (host:port) address; empty unless the endpoint is tailnet-exposed. */
  tailnet_address?: string;
  /** Current compute reservation; absent if the controller omits it. */
  resources?: AmpResources;
  created_at: string;
};

/** Whether an in-place resize applied to the running pod. */
export type AmpResizeOutcome = "completed" | "pending" | "infeasible" | "unsupported";

/** Result of a resize: the endpoint at its new size plus the in-place outcome. */
export type AmpResizeResult = {
  endpoint: AmpCompute;
  outcome: AmpResizeOutcome;
  message?: string;
};

function clusterRoute(datapackId: string, clusterId: string): string {
  return `${clustersRoute(datapackId)}/${encodeURIComponent(clusterId)}`;
}

function branchComputeRoute(datapackId: string, clusterId: string, branchId: string): string {
  return `${clusterRoute(datapackId, clusterId)}/branches/${encodeURIComponent(branchId)}/compute`;
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
  return request<AmpCompute[]>(branchComputeRoute(datapackId, clusterId, branchId), "GET", undefined, options);
}

/** POST to provision a compute instance on a branch. `cpu`/`memory` are
 *  optional Kubernetes quantities (e.g. "2", "4Gi") mirroring AMP's endpoint
 *  `resources`; omit either to take AMP's defaults (2 cores / 4Gi). */
export async function createCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  role: "read_write" | "read_only",
  resources: AmpResources = {},
) {
  const body: Record<string, unknown> = { role };
  if (resources.cpu?.trim()) body.cpu = resources.cpu.trim();
  if (resources.memory?.trim()) body.memory = resources.memory.trim();
  return request<AmpCompute>(branchComputeRoute(datapackId, clusterId, branchId), "POST", body);
}

/** POST to resize a compute instance in place. At least one of cpu/memory must
 *  be set; empty fields are left unchanged. Returns the endpoint at its new
 *  size plus whether the live resize applied. */
export async function resizeCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  computeId: string,
  resources: AmpResources,
) {
  const body: Record<string, unknown> = {};
  if (resources.cpu?.trim()) body.cpu = resources.cpu.trim();
  if (resources.memory?.trim()) body.memory = resources.memory.trim();
  return request<AmpResizeResult>(
    `${branchComputeRoute(datapackId, clusterId, branchId)}/${encodeURIComponent(computeId)}/resize`,
    "POST",
    body,
  );
}

/** DELETE (unprovision) a compute instance on a branch. */
export async function deleteCompute(
  datapackId: string,
  clusterId: string,
  branchId: string,
  computeId: string,
) {
  return request<{ id: string; deleted: boolean }>(
    `${branchComputeRoute(datapackId, clusterId, branchId)}/${encodeURIComponent(computeId)}`,
    "DELETE",
  );
}
