// Typed resource calls against the Controller API. One function per endpoint;
// react-query hooks in queries.ts compose these. Keeping the raw calls here
// (free of caching concerns) keeps them trivially testable/reusable.

import { apiRequest, ApiError, LIST_LIMIT } from "./client";
import type {
  Branch,
  BranchList,
  CreateBranchInput,
  CreateEndpointInput,
  CreateProjectInput,
  CreateStoragePoolInput,
  CreateUserInput,
  Endpoint,
  EndpointList,
  Project,
  ProjectList,
  StoragePool,
  StoragePoolList,
  Task,
  TaskList,
  User,
  UserList,
  Version,
} from "./types";

/* ---- Tasks (the mutating create endpoints are async: 202 + a Task) ---- */

// Interim fixed interval. A smarter scheme — exponential backoff, transient-error
// tolerance, progress-aware cadence, batch polling, possibly SSE — is tracked in #188.
const TASK_POLL_INTERVAL_MS = 5000;
// Generous: a create_endpoint task runs a Helm deploy (pod start + initdb) that
// can take a few minutes; create_project / branch tasks finish in seconds.
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
  projectId?: string;
}): Promise<TaskList> {
  return apiRequest<TaskList>("/tasks", {
    query: {
      limit: params.limit,
      offset: params.offset,
      status: params.status,
      kind: params.kind,
      project_id: params.projectId,
    },
  });
}

/** Poll a task to a terminal state: resolve with the succeeded Task, or throw an
 *  ApiError carrying `task.error` on failure (so the existing UI error path
 *  surfaces it) or on timeout. */
async function pollTask(taskId: string): Promise<Task> {
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

/* ---- Projects (databases) ---- */

export function listProjects(poolId: string): Promise<ProjectList> {
  return apiRequest<ProjectList>(`/storage-pools/${poolId}/projects`, {
    query: { limit: LIST_LIMIT },
  });
}

export function getProject(poolId: string, projectId: string): Promise<Project> {
  return apiRequest<Project>(`/storage-pools/${poolId}/projects/${projectId}`);
}

// Async: POST records a create_project task; poll it, then read the materialized
// project back (the task stamps the new project's id as project_id).
export async function createProject(
  poolId: string,
  input: CreateProjectInput,
): Promise<Project> {
  const task = await apiRequest<Task>(`/storage-pools/${poolId}/projects`, {
    method: "POST",
    body: input,
  });
  const done = await pollTask(task.id);
  if (!done.project_id) {
    throw new ApiError("create_project task reported no project id", 500);
  }
  return getProject(poolId, done.project_id);
}

/* ---- Branches (timelines / forks) ---- */

export function listBranches(
  poolId: string,
  projectId: string,
): Promise<BranchList> {
  return apiRequest<BranchList>(
    `/storage-pools/${poolId}/projects/${projectId}/branches`,
    { query: { limit: LIST_LIMIT } },
  );
}

export function getBranch(
  poolId: string,
  projectId: string,
  branchId: string,
): Promise<Branch> {
  return apiRequest<Branch>(
    `/storage-pools/${poolId}/projects/${projectId}/branches/${branchId}`,
  );
}

// Async: POST records a branch task; poll it, then read the new branch back
// (the task stamps the new branch's id as target_ref).
export async function createBranch(
  poolId: string,
  projectId: string,
  input: CreateBranchInput,
): Promise<Branch> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/projects/${projectId}/branches`,
    { method: "POST", body: input },
  );
  const done = await pollTask(task.id);
  if (!done.target_ref) {
    throw new ApiError("branch task reported no branch id", 500);
  }
  return getBranch(poolId, projectId, done.target_ref);
}

/* ---- Endpoints (compute bound to a branch) ---- */

export function listEndpoints(
  poolId: string,
  projectId: string,
  branchId: string,
): Promise<EndpointList> {
  return apiRequest<EndpointList>(
    `/storage-pools/${poolId}/projects/${projectId}/branches/${branchId}/endpoints`,
    { query: { limit: LIST_LIMIT } },
  );
}

export function getEndpoint(
  poolId: string,
  projectId: string,
  branchId: string,
  endpointId: string,
): Promise<Endpoint> {
  return apiRequest<Endpoint>(
    `/storage-pools/${poolId}/projects/${projectId}/branches/${branchId}/endpoints/${endpointId}`,
  );
}

// Async: POST records a create_endpoint task (which runs the Helm deploy); poll
// it, then read the deployed endpoint back (target_ref = the new endpoint id).
export async function createEndpoint(
  poolId: string,
  projectId: string,
  branchId: string,
  input: CreateEndpointInput,
): Promise<Endpoint> {
  const task = await apiRequest<Task>(
    `/storage-pools/${poolId}/projects/${projectId}/branches/${branchId}/endpoints`,
    { method: "POST", body: input },
  );
  const done = await pollTask(task.id);
  if (!done.target_ref) {
    throw new ApiError("create_endpoint task reported no endpoint id", 500);
  }
  return getEndpoint(poolId, projectId, branchId, done.target_ref);
}
