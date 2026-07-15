// Domain types for the Meko Controller REST API (the `:8080` control plane).
// These mirror the JSON the controller returns 1:1 — see api_server's
// storage-pools / projects / branches handlers. The platform-model mapping
// onto AMP's nav is: storage pool = "Shared pool", project = "Database",
// branch = a timeline/fork of a database, user = a "Tenant".

/** A tenant. `is_admin` distinguishes platform admins from regular tenants. */
export type User = {
  id: string;
  is_admin: boolean;
  created_at: string;
};

/** A storage pool — AMP's "Shared pool". Owned by exactly one user. */
export type StoragePool = {
  id: string;
  name: string;
  owner_user_id: string;
  created_at: string;
};

/** A branch: a Postgres timeline within a project. The default branch is
 *  `main`; forks carry a `parent_branch_id` and the `fork_lsn` they split at. */
export type Branch = {
  branch_id: string;
  project_id: string;
  name: string;
  pg_version: number;
  is_default: boolean;
  parent_branch_id: string | null;
  fork_lsn: string | null;
  created_at: string;
};

/** A project — AMP's "Database". Created inside a pool; ships with a
 *  default branch (`main`). */
export type Project = {
  id: string;
  name: string;
  storage_pool_id: string;
  default_branch: Branch;
  created_at: string;
};

/** A compute endpoint bound to a branch. A branch admits at most one
 *  read_write endpoint. `address` is host:port, empty until deployed. */
export type EndpointRole = "read_write" | "read_only";
export type Endpoint = {
  id: string;
  branch_id: string;
  project_id: string;
  role: EndpointRole;
  address: string;
  tailnet_address: string;
  created_at: string;
};

/** A write-ahead task. The mutating endpoints (create project / branch /
 *  endpoint) are async: they return `202` + one of these, and the work runs in
 *  the controller's task worker. Poll `GET /tasks/{id}` until `status` is
 *  terminal. On a create, the task carries the ids it produced: `project_id` is
 *  the project, and `target_ref` is the primary resource created (the new branch
 *  for a branch task, the endpoint for an endpoint task). `error` is set on
 *  failure. */
/** Controller build identity from GET /api/v1/version. */
export type Version = {
  version: string;
  git_commit: string;
  build_date: string;
  /** Version of the bundled storage_controller, when one is running. */
  storage_controller?: string;
};

export type TaskStatus = "pending" | "running" | "succeeded" | "failed";
export type Task = {
  id: string;
  kind: string;
  status: TaskStatus;
  project_id: string | null;
  target_ref: string | null;
  error: string | null;
  created_at: string;
  updated_at: string;
};

/* ---- List-envelope shapes the controller wraps collections in ---- */

export type UserList = { users: User[]; count: number; total: number };
export type StoragePoolList = {
  storage_pools: StoragePool[];
  count: number;
  total: number;
};
export type ProjectList = { projects: Project[]; count: number; total: number };
export type BranchList = { branches: Branch[]; count: number; total: number };
export type EndpointList = {
  endpoints: Endpoint[];
  count: number;
  total: number;
};
export type TaskList = { tasks: Task[]; count: number; total: number };

/* ---- Create payloads ---- */

export type CreateUserInput = { is_admin: boolean };
export type CreateStoragePoolInput = { name: string; owner_user_id: string };
export type CreateProjectInput = { name: string };
/** disk_type defaults to "network"; "local" is rejected by the controller. */
export type CreateEndpointInput = {
  role?: EndpointRole;
  disk_type?: "network" | "local";
};
export type CreateBranchInput = {
  name: string;
  /** Branch to fork from. Omit to use the project's default branch. */
  parent_branch_id?: string;
  /** Ancestor LSN to fork at ("X/X" hex, e.g. "0/16ABCD0"). Omit to use the
   *  parent's latest LSN — but the controller requires it when the parent has
   *  no running endpoint it can read the flush LSN from. */
  fork_point_lsn?: string;
};
