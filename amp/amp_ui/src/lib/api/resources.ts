// Typed resource calls against the Controller API. One function per endpoint;
// react-query hooks in queries.ts compose these. Keeping the raw calls here
// (free of caching concerns) keeps them trivially testable/reusable.

import { apiRequest, ApiError, LIST_LIMIT } from "./client";
import type {
  AddBranchExtensionsInput,
  Branch,
  BranchList,
  CreateBranchInput,
  CreateEndpointInput,
  CreateClusterInput,
  CreateStoragePoolInput,
  CreateUserInput,
  Endpoint,
  EndpointList,
  Cluster,
  ClusterList,
  ResizeEndpointInput,
  ResizeOutcome,
  SettingsUpdateResult,
  StoragePool,
  StoragePoolList,
  Task,
  TaskList,
  UpdateBranchSettingsInput,
  UpdateEndpointSettingsInput,
  User,
  UserList,
  Version,
} from "./types";

/* ---- Tasks (the mutating create endpoints are async: 202 + a Task) ---- */

// Interim fixed interval. A smarter scheme — exponential backoff, transient-error
// tolerance, progress-aware cadence, batch polling, possibly SSE — is tracked in #188.
const TASK_POLL_INTERVAL_MS = 5000;
// Generous: a create_endpoint task runs a Helm deploy (pod start + initdb) that
// can take a few minutes; create_cluster / branch tasks finish in seconds.
const TASK_TIMEOUT_MS = 5 * 60 * 1000;

export function getTask(taskId: string): Promise<Task> {
  return apiRequest<Task>(`/tasks/${taskId}`);
}

/** List tasks, most-recent-first. Unlike the other list fns this one paginates
 *  (the Tasks page pages through history), so limit/offset are caller-supplied;
 *  undefined params are dropped by apiRequest so the controller's defaults apply. */
export function listTasks(params: {
  limit?: number;
  offset?: number;
  status?: string;
  kind?: string;
  clusterId?: string;
}): Promise<TaskList> {
  return apiRequest<TaskList>("/tasks", {
    query: {
      limit: params.limit,
      offset: params.offset,
      status: params.status,
      kind: params.kind,
      cluster_id: params.clusterId,
    },
  });
}

/** Poll a task to a terminal state: resolve with the succeeded Task, or throw an
 *  ApiError carrying `task.error` on failure (so the existing UI error path
 *  surfaces it) or on timeout. */
export async function pollTask(taskId: string): Promise<Task> {
  const deadline = Date.now() + TASK_TIMEOUT_MS;
  for (;;) {
    const task = await getTask(taskId);
    if (task.status === "succeeded") return task;
    if (task.status === "failed") {
      throw new ApiError(task.error ?? `task ${task.kind} failed`, 500);
    }
    if (Date.now() >= deadline) {
      throw new ApiError(
        `task ${taskId} (${task.kind}) did not finish within ${TASK_TIMEOUT_MS / 1000}s`,
        504,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, TASK_POLL_INTERVAL_MS));
  }
}

/* ---- Users (tenants) ---- */

export function listUsers(): Promise<UserList> {
  return apiRequest<UserList>("/users", { query: { limit: LIST_LIMIT } });
}

export function getUser(userId: string): Promise<User> {
  return apiRequest<User>(`/users/${userId}`);
}

/* ---- Meta ---- */

/** Controller build identity (semantic version, git commit, build date). */
export function getVersion(): Promise<Version> {
  return apiRequest<Version>("/version");
}

export function createUser(input: CreateUserInput): Promise<User> {
  return apiRequest<User>("/users", { method: "POST", body: input });
}

/* ---- Storage pools (shared pools) ---- */

export function listStoragePools(): Promise<StoragePoolList> {
  return apiRequest<StoragePoolList>("/storage-pools", {
    query: { limit: LIST_LIMIT },
  });
}

export function createStoragePool(
  input: CreateStoragePoolInput,
): Promise<StoragePool> {
  return apiRequest<StoragePool>("/storage-pools", {
    method: "POST",
    body: input,
  });
}

/* ---- Clusters ---- */

// include_failed defaults to false server-side (the list is scoped to usable
// clusters — what a picker or count wants); pass includeFailed:true — e.g. from
// an operator's fleet view — to also surface failed-creation orphans so they can
// be seen and deleted. undefined is dropped by apiRequest, keeping the server default.
export function listClusters(
  poolId: string,
  opts?: { includeFailed?: boolean },
): Promise<ClusterList> {
  return apiRequest<ClusterList>(`/storage-pools/${poolId}/clusters`, {
    query: { limit: LIST_LIMIT, include_failed: opts?.includeFailed },
  });
}

export function getCluster(poolId: string, clusterId: string): Promise<Cluster> {
  return apiRequest<Cluster>(`/storage-pools/${poolId}/clusters/${clusterId}`);
}

// Async: POST records a create_cluster task; poll it, then read the materialized
// cluster back (the task stamps the new cluster's id as cluster_id).
export async function createCluster(
  poolId: string,
  input: CreateClusterInput,
): Promise<Cluster> {
  const task = await apiRequest<Task>(`/storage-pools/${poolId}/clusters`, {
    method: "POST",
    body: input,
  });
  const done = await pollTask(task.id);
  if (!done.cluster_id) {
    throw new ApiError("create_cluster task reported no cluster id", 500);
  }
  return getCluster(poolId, done.cluster_id);
}

// Async, fire-and-forget: records a create_cluster task with `with_writer` so the
// controller creates the cluster AND its `main`-branch read_write endpoint in ONE
// background task. Returns the Task immediately (no polling): the caller shows a
// toast + deeplinks to Tasks, and the worker runs the whole thing independently
// of this client (so it survives navigating away / refresh). `cpu`/`memory` size
// the writer; empty takes the controller default (2 cores / 4Gi).
export function createClusterWithWriter(
  poolId: string,
  input: { name: string; cpu?: string; memory?: string; extensions?: string[] },
): Promise<Task> {
  const body: Record<string, unknown> = { name: input.name, with_writer: true };
  if (input.cpu || input.memory) {
    body.resources = { cpu: input.cpu || undefined, memory: input.memory || undefined };
  }
  // Seeds the `main` branch's set. Omitted rather than sent empty: [] and absent
  // mean the same to the controller, and absent is the quieter payload.
  if (input.extensions?.length) {
    body.extensions = input.extensions;
  }
  return apiRequest<Task>(`/storage-pools/${poolId}/clusters`, {
    method: "POST",
    body,
  });
}

// Async: DELETE plans a delete_cluster cascade (retires the cluster's endpoints,
// drops its branches, then drops the cluster row); poll to completion so the
// caller only sees success once the whole cascade has finished. 404 up front if
// the pool or cluster is already gone.
export async function deleteCluster(
  poolId: string,
  clusterId: string,
): Promise<void> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}`,
    { method: "DELETE" },
  );
  await pollTask(task.id);
}

/* ---- Branches (timelines / forks) ---- */

export function listBranches(
  poolId: string,
  clusterId: string,
): Promise<BranchList> {
  return apiRequest<BranchList>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches`,
    { query: { limit: LIST_LIMIT } },
  );
}

export function getBranch(
  poolId: string,
  clusterId: string,
  branchId: string,
): Promise<Branch> {
  return apiRequest<Branch>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}`,
  );
}

// Async: POST records a branch task; poll it, then read the new branch back
// (the task stamps the new branch's id as target_ref).
export async function createBranch(
  poolId: string,
  clusterId: string,
  input: CreateBranchInput,
): Promise<Branch> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches`,
    { method: "POST", body: input },
  );
  const done = await pollTask(task.id);
  if (!done.target_ref) {
    throw new ApiError("branch task reported no branch id", 500);
  }
  return getBranch(poolId, clusterId, done.target_ref);
}

// Async: DELETE plans a delete_branch cascade (retires the branch's endpoints,
// then drops the branch row); poll to completion. The controller pre-checks and
// returns 409 if the branch has child branches forked off it (the parent FK is
// RESTRICT), surfaced here as an ApiError(status 409) the UI turns actionable.
export async function deleteBranch(
  poolId: string,
  clusterId: string,
  branchId: string,
): Promise<void> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}`,
    { method: "DELETE" },
  );
  await pollTask(task.id);
}

/* ---- Endpoints (compute bound to a branch) ---- */

export function listEndpoints(
  poolId: string,
  clusterId: string,
  branchId: string,
): Promise<EndpointList> {
  return apiRequest<EndpointList>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints`,
    { query: { limit: LIST_LIMIT } },
  );
}

export function getEndpoint(
  poolId: string,
  clusterId: string,
  branchId: string,
  endpointId: string,
): Promise<Endpoint> {
  return apiRequest<Endpoint>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints/${endpointId}`,
  );
}

// Async: POST records a create_endpoint task (which runs the Helm deploy); poll
// it, then read the deployed endpoint back (target_ref = the new endpoint id).
export async function createEndpoint(
  poolId: string,
  clusterId: string,
  branchId: string,
  input: CreateEndpointInput,
): Promise<Endpoint> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints`,
    { method: "POST", body: input },
  );
  const done = await pollTask(task.id);
  if (!done.target_ref) {
    throw new ApiError("create_endpoint task reported no endpoint id", 500);
  }
  return getEndpoint(poolId, clusterId, branchId, done.target_ref);
}

/** The outcome of a resize: the endpoint at its new size, plus whether the live
 *  pod took the change in place (vs. deferred/unsupported — still durable in the
 *  endpoint config, so it applies on the next controller-driven redeploy). */
export type ResizeResult = {
  endpoint: Endpoint;
  outcome: ResizeOutcome;
  message: string;
};

// Async: POST records a resize_endpoint task (in-place-resize the running pod +
// persist the new size to config); poll it, then read the endpoint back and
// report the in-place outcome from the task result.
export async function resizeEndpoint(
  poolId: string,
  clusterId: string,
  branchId: string,
  endpointId: string,
  input: ResizeEndpointInput,
): Promise<ResizeResult> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints/${endpointId}/resize`,
    { method: "POST", body: input },
  );
  const done = await pollTask(task.id);
  const result = (done.result ?? {}) as {
    in_place_resize?: ResizeOutcome;
    in_place_message?: string;
  };
  const endpoint = await getEndpoint(poolId, clusterId, branchId, endpointId);
  return {
    endpoint,
    outcome: result.in_place_resize ?? "completed",
    message: result.in_place_message ?? "",
  };
}

// Sync: the controller stores the settings, then pushes them to the running
// endpoints. The reply says what was stored (`applied`) and, per endpoint,
// which services it restarted onto the new config or why it could not be told.
//
// Only what the caller names is sent: an omitted surface is left alone and `{}`
// clears it, so the two must not be collapsed on the way out.
export async function updateBranchSettings(
  poolId: string,
  clusterId: string,
  branchId: string,
  input: UpdateBranchSettingsInput,
): Promise<SettingsUpdateResult> {
  return apiRequest<SettingsUpdateResult>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/settings`,
    { method: "PATCH", body: input },
  );
}

// Async, fire-and-forget: records an add_branch_extensions task that unions the
// names into the branch's set and redeploys every endpoint on the branch to
// mount them. Returns the Task immediately — the redeploy interrupts each
// compute's connections, so the caller says so before it happens. Idempotent:
// re-adding an already-enabled extension is a no-op.
export function addBranchExtensions(
  poolId: string,
  clusterId: string,
  branchId: string,
  input: AddBranchExtensionsInput,
): Promise<Task> {
  return apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/extensions`,
    { method: "POST", body: input },
  );
}

export async function updateEndpointSettings(
  poolId: string,
  clusterId: string,
  branchId: string,
  endpointId: string,
  input: UpdateEndpointSettingsInput,
): Promise<SettingsUpdateResult> {
  return apiRequest<SettingsUpdateResult>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints/${endpointId}/settings`,
    { method: "PATCH", body: input },
  );
}

// Async: DELETE records a delete_endpoint task (tears down the Helm release and
// removes the endpoint record — i.e. unprovisions the compute); poll it to
// completion so the caller only sees success once the instance is gone.
export async function deleteEndpoint(
  poolId: string,
  clusterId: string,
  branchId: string,
  endpointId: string,
): Promise<void> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/clusters/${clusterId}/branches/${branchId}/endpoints/${endpointId}`,
    { method: "DELETE" },
  );
  await pollTask(task.id);
}
