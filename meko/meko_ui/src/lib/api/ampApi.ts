// AMP cluster API calls, kept separate from the core mekoApi surface. A cluster
// is an AMP database (its default branch is `main`). Every function + route is
// `amp`-prefixed so it's clear at the call site (and on the wire,
// /datapacks/:id/amp/...) that this is the AMP surface.
// Gated by SHOW_PG_AMP; reuses mekoApi's `request` (envelope unwrap + errors).
//
// NOTE: response field names (cluster_id, storage_pool_id, …) mirror the AMP
// controller payload that api_server passes through unchanged.

import { request } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

function clustersRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/amp/clusters`;
}

/** A branch (Postgres timeline) of an AMP cluster; `main` is the default. */
export type AmpBranch = {
  branch_id: string;
  cluster_id: string;
  name: string;
  pg_version: number;
  is_default: boolean;
  fork_lsn?: string;
  created_at: string;
};

/** An AMP cluster (a Postgres database) inside the datapack's shared pool. */
export type AmpCluster = {
  id: string;
  name: string;
  storage_pool_id: string;
  default_branch: AmpBranch;
  created_at: string;
};

function clusterRoute(datapackId: string, clusterId: string): string {
  return `${clustersRoute(datapackId)}/${encodeURIComponent(clusterId)}`;
}

/** GET /datapacks/:id/amp/clusters — the datapack's AMP clusters ([] if none). */
export async function ampListClusters(datapackId: string, options: RouteOptions = {}) {
  return request<AmpCluster[]>(clustersRoute(datapackId), "GET", undefined, options);
}

/** POST /datapacks/:id/amp/clusters — create a cluster and its `main`-branch
 *  read_write endpoint in ONE background task (with_writer). api_server polls the
 *  task to completion (initial cluster + writer provisions in well under a
 *  minute) and returns the materialized cluster. `cpu`/`memory` size the writer;
 *  omit either for AMP's default (2 cores / 4Gi). */
export async function ampCreateCluster(
  datapackId: string,
  payload: {
    name: string;
    cpu?: string;
    memory?: string;
    pg_version?: number;
    // Optional engine ("postgres" default, "aidb", "oracle") and catalog
    // extensions to seed on the default branch. Both are opt-in and composable;
    // sent only when set so a plain Postgres cluster keeps the same payload.
    engine?: string;
    extensions?: string[];
  },
) {
  const body: Record<string, unknown> = { name: payload.name.trim(), with_writer: true };
  if (payload.cpu?.trim()) body.cpu = payload.cpu.trim();
  if (payload.memory?.trim()) body.memory = payload.memory.trim();
  if (payload.pg_version != null) body.pg_version = payload.pg_version;
  if (payload.engine?.trim() && payload.engine.trim() !== "postgres")
    body.engine = payload.engine.trim();
  const extensions = (payload.extensions ?? []).map((e) => e.trim()).filter(Boolean);
  if (extensions.length > 0) body.extensions = extensions;
  return request<AmpCluster>(clustersRoute(datapackId), "POST", body);
}

/** DELETE /datapacks/:id/amp/clusters/:id — delete a cluster, cascading to its
 *  branches + compute. Blocks server-side until the AMP task completes. */
export async function ampDeleteCluster(datapackId: string, clusterId: string) {
  return request<{ id: string; deleted: boolean }>(clusterRoute(datapackId, clusterId), "DELETE");
}

/** Compute sizing reserved for an endpoint (Kubernetes quantities): cpu e.g.
 *  "2"/"1500m", memory e.g. "4Gi". */
export type AmpResources = { cpu?: string; memory?: string };

/** A compute instance bound to a branch (what a client connects to). address is
 *  host:port, empty until deployed. */
export type AmpCompute = {
  id: string;
  cluster_id: string;
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

function branchComputeRoute(datapackId: string, clusterId: string, branchId: string): string {
  return `${clusterRoute(datapackId, clusterId)}/branches/${encodeURIComponent(branchId)}/compute`;
}

/** GET a cluster's branch tree. */
export async function ampListBranches(
  datapackId: string,
  clusterId: string,
  options: RouteOptions = {},
) {
  return request<AmpBranch[]>(`${clusterRoute(datapackId, clusterId)}/branches`, "GET", undefined, options);
}

/** POST a new branch (fork). fork_point_lsn is required when the parent has no
 *  running compute for the controller to read the latest LSN from. */
export async function ampCreateBranch(
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
export async function ampListCompute(
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
export async function ampCreateCompute(
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
export async function ampResizeCompute(
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
export async function ampDeleteCompute(
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
