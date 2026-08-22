// Domain types for the Meko Controller REST API (the `:8080` control plane).
// These mirror the JSON the controller returns 1:1 — see api_server's
// storage-pools / clusters / branches handlers. The platform-model mapping
// onto AMP's nav is: storage pool = "Shared pool", cluster = "Cluster",
// branch = a timeline/fork of a cluster, user = a "Tenant".

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

/** A branch: a Postgres timeline within a cluster. The default branch is
 *  `main`; forks carry a `parent_branch_id` and the `fork_lsn` they split at. */
export type Branch = {
  branch_id: string;
  cluster_id: string;
  name: string;
  pg_version: number;
  is_default: boolean;
  parent_branch_id: string | null;
  fork_lsn: string | null;
  /** Baseline Postgres settings for every compute on the branch; an
   *  endpoint's own merge over these and win on a shared key. */
  pg_settings?: PgSettings;
  /** How the supervisor holds this branch's write lease. */
  write_token_settings?: WriteTokenSettings;
  created_at: string;
};

/** A cluster — AMP's "Cluster". Created inside a pool; ships with a
 *  default branch (`main`). */
export type Cluster = {
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
  cluster_id: string;
  role: EndpointRole;
  address: string;
  tailnet_address: string;
  /** Current compute reservation (cpu/memory). Fields may be empty for
   *  endpoints created before resource tracking. */
  resources?: EndpointResources;
  /** This endpoint's own stored settings. `pg_settings` merge over the
   *  branch's and win on a shared key; the daemon ones are its alone. */
  pg_settings?: PgSettings;
  pageserver_settings?: DaemonSettings;
  safekeeper_settings?: DaemonSettings;
  created_at: string;
};

/** A write-ahead task. The mutating endpoints (create cluster / branch /
 *  endpoint) are async: they return `202` + one of these, and the work runs in
 *  the controller's task worker. Poll `GET /tasks/{id}` until `status` is
 *  terminal. On a create, the task carries the ids it produced: `cluster_id` is
 *  the cluster, and `target_ref` is the primary resource created (the new branch
 *  for a branch task, the endpoint for an endpoint task). `error` is set on
 *  failure. */
/** Controller build identity from GET /api/v1/version. */
export type Version = {
  version: string;
  git_commit: string;
  build_date: string;
};

export type TaskStatus = "pending" | "running" | "succeeded" | "failed";
export type Task = {
  id: string;
  kind: string;
  status: TaskStatus;
  cluster_id: string | null;
  target_ref: string | null;
  error: string | null;
  /** The final step's result, as opaque JSON (present once the task has run).
   *  A resize_endpoint task carries { in_place_resize, in_place_message }. */
  result?: unknown;
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
export type ClusterList = { clusters: Cluster[]; count: number; total: number };
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
export type CreateClusterInput = { name: string };
/** CPU + memory reserved for the endpoint pod. Each value is a Kubernetes
 *  resource quantity applied as both the request and the limit (Guaranteed QoS).
 *  Omit either field to take its default (2 cores / 4Gi). */
export type EndpointResources = {
  /** CPU cores, e.g. "2" or "1500m". Defaults to "2". */
  cpu?: string;
  /** Memory, e.g. "4Gi" or "512Mi". Defaults to "4Gi". */
  memory?: string;
};

/** disk_type defaults to "network"; "local" is rejected by the controller.
 *  resources default to 2 cores / 4Gi when omitted. */
export type CreateEndpointInput = {
  role?: EndpointRole;
  disk_type?: "network" | "local";
  resources?: EndpointResources;
};

/** Change an existing endpoint's compute reservation. cpu/memory are applied
 *  as request == limit; an omitted field keeps its current value. */
export type ResizeEndpointInput = {
  resources: EndpointResources;
};

/** Flat `{name: value}` Postgres settings. */
export type PgSettings = Record<string, string>;

/** A storage daemon's overrides. Keys may be dotted paths
 *  (`tenant_config.enable_gc`); values are scalars. */
export type DaemonSettings = Record<string, string | number | boolean>;

/** How the supervisor holds a branch's write lease. `renew_seconds` must be
 *  under `ttl_ms` — the gap is how long a failing renewal may be retried
 *  before anything stops writing. */
export type WriteTokenSettings = {
  ttl_ms?: number;
  renew_seconds?: number;
};

/** Settings to replace on a branch. An omitted field is left alone; `{}`
 *  clears that surface. The two are not interchangeable. */
export type UpdateBranchSettingsInput = {
  pg_settings?: PgSettings;
  write_token_settings?: WriteTokenSettings;
};

/** Settings to replace on one endpoint. Same omitted-vs-empty rule. */
export type UpdateEndpointSettingsInput = {
  pg_settings?: PgSettings;
  pageserver_settings?: DaemonSettings;
  safekeeper_settings?: DaemonSettings;
};

/** What one endpoint did with a settings change. `restarted` names the
 *  services it bounced onto the new config — empty when nothing changed for
 *  it. `unreachable` says why it could not be told; the settings are stored
 *  either way and it picks them up when it next starts. */
export type EndpointSettingsOutcome = {
  endpoint_id: string;
  restarted?: string[];
  /** Services that took the change in place. For the compute this means
   *  Postgres reloaded its config and kept its connections. */
  reloaded?: string[];
  /** The Postgres settings that forced a restart — they are fixed at server
   *  start, so naming them explains a cost that would otherwise look
   *  arbitrary. */
  restart_forced_by?: string[];
  /** Settings that are stored but not in force, each with why — a typo and an
   *  unusable value send the operator to different fixes. The endpoint was not
   *  interrupted for any of them. */
  not_applied?: { name: string; reason: string }[];
  unreachable?: string;
};

/** The reply to a settings update: stored, plus what it reached. */
export type SettingsUpdateResult = {
  applied: boolean;
  endpoints?: EndpointSettingsOutcome[];
};

/** The in-place resize outcome reported by the resize task result. */
export type ResizeOutcome = "completed" | "pending" | "infeasible" | "unsupported";

/** UI defaults, mirroring the controller's own 2-core / 4Gi default. */
export const DEFAULT_ENDPOINT_CPU = "2";
export const DEFAULT_ENDPOINT_MEMORY = "4Gi";
export type CreateBranchInput = {
  name: string;
  /** Branch to fork from. Omit to use the cluster's default branch. */
  parent_branch_id?: string;
  /** Ancestor LSN to fork at ("X/X" hex, e.g. "0/16ABCD0"). Omit to use the
   *  parent's latest LSN — but the controller requires it when the parent has
   *  no running endpoint it can read the flush LSN from. */
  fork_point_lsn?: string;
};
