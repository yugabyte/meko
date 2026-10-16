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
  /** Externalized extensions enabled on this branch, by catalog name. Every
   *  endpoint on the branch mounts them and a fork inherits the set. */
  extensions?: string[];
  /** Whether a read-write compute created here must carry a password: true
   *  only on a root branch whose cluster has none yet. A fork uses its parent's
   *  password as of the fork point. Absent from controllers that predate it,
   *  which required one on every writer. */
  password_required?: boolean;
  created_at: string;
};

/** A cluster — AMP's "Cluster". Created inside a pool; ships with a
 *  default branch (`main`). */
/** One entry of the controller's extension catalog (GET /extensions). */
export type Extension = {
  /** Catalog name — what a branch opts in by, and the mount subdir. */
  name: string;
  /** The image actually mounted. Derived for a built-in; as registered otherwise. */
  image: string;
  version?: string;
  description?: string;
  /** Shared-library basename when it differs from `name`. */
  library?: string;
  /** Needs shared_preload_libraries, so enabling it restarts the postmaster. */
  preload: boolean;
  /** Shipped with this controller and built by deploy/extensions. A built-in
   *  cannot be replaced or removed; everything else was registered by an admin. */
  builtin: boolean;
  layout?: ExtensionLayout;
  abi_extra_exempt?: string[];
};

/** Where a registered image keeps its pieces, relative to /extensions/<name>. */
export type ExtensionLayout = {
  share_path?: string;
  lib_path?: string;
  library_path?: string[];
  env_paths?: Record<string, string>;
};

export type ExtensionList = {
  extensions: Extension[];
  total: number;
  count: number;
};

/** Body of PUT /extensions/{name}. The name travels in the path. */
export type RegisterExtensionInput = {
  image: string;
  version?: string;
  description?: string;
  library?: string;
  preload?: boolean;
  layout?: ExtensionLayout;
  abi_extra_exempt?: string[];
};

export type Cluster = {
  id: string;
  name: string;
  storage_pool_id: string;
  default_branch: Branch;
  /** The engine the cluster was provisioned as ("postgres", "aidb", "oracle",
   *  "sqlserver", "documentdb").
   *  Provenance: it named the extensions seeded at create and is not
   *  re-derived when a branch adds more. */
  engine?: string;
  /** The default branch's extension set, surfaced as the cluster's. */
  extensions?: string[];
  /** Whether the cluster still needs its password, which its default branch's
   *  first read-write compute sets. */
  password_required?: boolean;
  created_at: string;
};

/** A compute endpoint bound to a branch. A branch admits at most one
 *  read_write endpoint. `address` is host:port, empty until deployed. */
export type EndpointRole = "read_write" | "read_only";
/** How clients reach an endpoint's Postgres port. `cluster_ip` is in-cluster
 *  (plus the tailnet) only; the `nlb_*` modes add a Postgres-only Service behind
 *  the cloud's L4 load balancer — internal (reachable from the surrounding
 *  VPC/VNet) or internet-facing. Works on EKS, GKE and AKS; the controller
 *  renders the right provider's annotations.
 *
 *  The `ingress_*` pair gives those same two exposures from a SHARED load
 *  balancer — a port on an ingress shard — so creating one provisions nothing in
 *  the cloud. They are refused with 400 unless the deployment has ingress
 *  enabled, and they take no `allowed_cidrs`: the load balancer is shared, so a
 *  per-endpoint allowlist cannot be a per-Service field, and the middleware that
 *  will do it is not built yet. */
export type EndpointConnectivityMode =
  | "cluster_ip"
  | "ingress_internal"
  | "ingress_external"
  | "nlb_internal"
  | "nlb_external";
export type EndpointConnectivity = {
  mode?: EndpointConnectivityMode;
  /** Sources allowed to reach the load balancer, as CIDRs. Empty is
   *  unrestricted — on `nlb_external` that means the public internet. */
  allowed_cidrs?: string[];
};
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
  /** Client reachability as stored. Absent for endpoints created before this
   *  was configurable, which behave as `cluster_ip`. */
  connectivity?: EndpointConnectivity;
  /** MongoDB wire-protocol address (host:port) for this endpoint's documentdb
   *  gateway. Present only when the branch has the documentdb extension — the
   *  gateway is a sidecar deployed with the compute. The port is the gateway's
   *  own default (10260), not MongoDB's 27017. It serves the compute's
   *  certificate, so the endpoint's CA bundle decides how a client verifies it
   *  (lib/endpointTls.ts). */
  documentdb_address?: string;
  /** This endpoint's own stored settings. `pg_settings` merge over the
   *  branch's and win on a shared key; the daemon ones are its alone. */
  pg_settings?: PgSettings;
  pageserver_settings?: DaemonSettings;
  safekeeper_settings?: DaemonSettings;
  /** The pod's scheduling rules as stored; absent when none. */
  tolerations?: Toleration[];
  topology_spread_constraints?: TopologySpreadConstraint[];
  created_at: string;
};

/** Kubernetes core/v1 `Toleration`, field names verbatim. */
export type TolerationOperator = "Exists" | "Equal";
export type TaintEffect = "NoSchedule" | "PreferNoSchedule" | "NoExecute";
export type Toleration = {
  key?: string;
  operator?: TolerationOperator;
  value?: string;
  effect?: TaintEffect;
  tolerationSeconds?: number;
};

/** Kubernetes meta/v1 `LabelSelector`. */
export type LabelSelectorOperator = "In" | "NotIn" | "Exists" | "DoesNotExist";
export type LabelSelectorRequirement = {
  key: string;
  operator: LabelSelectorOperator;
  values?: string[];
};
export type LabelSelector = {
  matchLabels?: Record<string, string>;
  matchExpressions?: LabelSelectorRequirement[];
};

/** Kubernetes core/v1 `TopologySpreadConstraint`, field names verbatim.
 *  `labelSelector` is required and must select something: each endpoint is a
 *  single-pod Deployment, so an empty selector would constrain nothing and the
 *  controller refuses it. */
export type WhenUnsatisfiable = "DoNotSchedule" | "ScheduleAnyway";
export type TopologySpreadConstraint = {
  maxSkew: number;
  topologyKey: string;
  whenUnsatisfiable: WhenUnsatisfiable;
  labelSelector: LabelSelector;
  minDomains?: number;
  nodeAffinityPolicy?: "Honor" | "Ignore";
  nodeTaintsPolicy?: "Honor" | "Ignore";
  matchLabelKeys?: string[];
};

/** Replace an endpoint's scheduling lists. An omitted list is left alone; `[]`
 *  clears it. Applying it recreates the pod. */
export type UpdateEndpointSchedulingInput = {
  tolerations?: Toleration[];
  topology_spread_constraints?: TopologySpreadConstraint[];
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
/** What a client needs to verify an endpoint's server certificate.
 *
 *  `required: false` is a real answer, not an error: an endpoint whose
 *  certificate comes from a public CA needs no bundle, and so does one with TLS
 *  off. Show that rather than an empty download. */
export type EndpointCaBundle = {
  required: boolean;
  /** Present only on an endpoint with a documentdb_address: whether its gateway
   *  serves the compute's certificate (false: it self-signs). Absent from a
   *  controller that predates it. */
  documentdb_verifiable?: boolean;
  /** PEM. Empty when `required` is false. */
  ca_crt?: string;
  /** The name to connect to for sslmode=verify-full — the certificate covers
   *  this one, and another address for the same endpoint may not be in it. */
  verify_name?: string;
};

export type EndpointList = {
  endpoints: Endpoint[];
  count: number;
  total: number;
};
export type TaskList = { tasks: Task[]; count: number; total: number };

/* ---- Create payloads ---- */

export type CreateUserInput = { is_admin: boolean };
export type CreateStoragePoolInput = { name: string; owner_user_id: string };
export type UpdateStoragePoolInput = { name: string };
export type CreateClusterInput = { name: string; extensions?: string[] };
/** Unioned into the branch's set; the controller redeploys every endpoint on
 *  the branch to mount them. At least one name. */
export type AddBranchExtensionsInput = { extensions: string[] };
/** CPU + memory reserved for the endpoint pod. Each value is a Kubernetes
 *  resource quantity applied as both the request and the limit (Guaranteed QoS). */
export type EndpointResources = {
  /** CPU cores, e.g. "2" or "1500m". Defaults to "2". */
  cpu?: string;
  /** Memory, e.g. "16Gi" or "512Mi", with an explicit Mi-or-larger unit.
   *  Omitted, it follows `cpu` at the control plane's memory-per-vCPU ratio, so
   *  naming only `cpu` is enough. Less memory per core than that ratio is a 400
   *  naming the minimum. The ratio is per deployment, so the control plane is
   *  what decides it, not this client. */
  memory?: string;
};

/** disk_type defaults to "network"; "local" is rejected by the controller.
 *  resources default to 2 cores, with memory following cpu at the control
 *  plane's memory-per-vCPU ratio. */
export type CreateEndpointInput = {
  role?: EndpointRole;
  disk_type?: "network" | "local";
  resources?: EndpointResources;
  /** Password for the compute's default user (`cloud_admin`). REQUIRED for a
   *  read_write endpoint — the controller mints none — and refused on a
   *  read_only one, which shares its branch's roles. */
  password?: string;
  tolerations?: Toleration[];
  topology_spread_constraints?: TopologySpreadConstraint[];
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

/** UI defaults, mirroring the controller's own: 2 cores, and the memory those
 *  cores are owed at the 8 GiB per vCPU it holds every endpoint to. Sending
 *  less than that ratio is a 400, so these are not free-form suggestions —
 *  a pre-filled value under the floor makes the default action fail. */
export const DEFAULT_ENDPOINT_CPU = "2";
export const DEFAULT_ENDPOINT_MEMORY = "16Gi";
/** Matches the controller/chart default: in-cluster only, no load balancer. */
export const DEFAULT_ENDPOINT_CONNECTIVITY: EndpointConnectivityMode = "cluster_ip";

export type CreateBranchInput = {
  name: string;
  /** Branch to fork from. Omit to use the cluster's default branch. */
  parent_branch_id?: string;
  /** Ancestor LSN to fork at ("X/X" hex, e.g. "0/16ABCD0"). Omit to use the
   *  parent's latest LSN — but the controller requires it when the parent has
   *  no running endpoint it can read the flush LSN from. */
  fork_point_lsn?: string;
};
