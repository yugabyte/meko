// AMP (Postgres databases) API calls, kept separate from the core mekoApi
// surface. Gated by SHOW_PG_AMP at the call sites; reuses mekoApi's `request`
// (envelope unwrap + error handling) and the standard Meko API base.

import { request } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

function databasesRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/databases`;
}

/** A branch (Postgres timeline) of an AMP database; `main` is the default. */
export type AmpBranch = {
  branch_id: string;
  project_id: string;
  name: string;
  pg_version: number;
  is_default: boolean;
  fork_lsn?: string;
  created_at: string;
};

/** An AMP database (project) inside the datapack's shared pool. */
export type AmpDatabase = {
  id: string;
  name: string;
  storage_pool_id: string;
  default_branch: AmpBranch;
  created_at: string;
};

/** GET /datapacks/:id/databases — the datapack's AMP databases ([] if none). */
export async function listDatabases(datapackId: string, options: RouteOptions = {}) {
  return request<AmpDatabase[]>(databasesRoute(datapackId), "GET", undefined, options);
}

/** POST /datapacks/:id/databases — create a database (provisions the shared
 *  pool on first use). */
export async function createDatabase(
  datapackId: string,
  payload: { name: string; pg_version?: number },
) {
  const body: Record<string, unknown> = { name: payload.name.trim() };
  if (payload.pg_version != null) body.pg_version = payload.pg_version;
  return request<AmpDatabase>(databasesRoute(datapackId), "POST", body);
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

function databaseRoute(datapackId: string, projectId: string): string {
  return `${databasesRoute(datapackId)}/${encodeURIComponent(projectId)}`;
}

/** GET a database's branch tree. */
export async function listBranches(
  datapackId: string,
  projectId: string,
  options: RouteOptions = {},
) {
  return request<AmpBranch[]>(`${databaseRoute(datapackId, projectId)}/branches`, "GET", undefined, options);
}

/** POST a new branch (fork). fork_point_lsn is required when the parent has no
 *  running compute for the controller to read the latest LSN from. */
export async function createBranch(
  datapackId: string,
  projectId: string,
  payload: { name: string; parent_branch_id?: string; fork_point_lsn?: string },
) {
  const body: Record<string, unknown> = { name: payload.name.trim() };
  if (payload.parent_branch_id) body.parent_branch_id = payload.parent_branch_id;
  if (payload.fork_point_lsn?.trim()) body.fork_point_lsn = payload.fork_point_lsn.trim();
  return request<AmpBranch>(`${databaseRoute(datapackId, projectId)}/branches`, "POST", body);
}

/** GET the compute instances on a branch. */
export async function listCompute(
  datapackId: string,
  projectId: string,
  branchId: string,
  options: RouteOptions = {},
) {
  return request<AmpCompute[]>(
    `${databaseRoute(datapackId, projectId)}/branches/${encodeURIComponent(branchId)}/compute`,
    "GET",
    undefined,
    options,
  );
}

/** POST to provision a compute instance on a branch. */
export async function createCompute(
  datapackId: string,
  projectId: string,
  branchId: string,
  role: "read_write" | "read_only",
) {
  return request<AmpCompute>(
    `${databaseRoute(datapackId, projectId)}/branches/${encodeURIComponent(branchId)}/compute`,
    "POST",
    { role },
  );
}
