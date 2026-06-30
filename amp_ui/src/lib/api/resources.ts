// Typed resource calls against the Controller API. One function per endpoint;
// react-query hooks in queries.ts compose these. Keeping the raw calls here
// (free of caching concerns) keeps them trivially testable/reusable.

import { apiRequest, LIST_LIMIT } from "./client";
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
  User,
  UserList,
} from "./types";

/* ---- Users (tenants) ---- */

export function listUsers(): Promise<UserList> {
  return apiRequest<UserList>("/users", { query: { limit: LIST_LIMIT } });
}

export function getUser(userId: string): Promise<User> {
  return apiRequest<User>(`/users/${userId}`);
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

export function createProject(
  poolId: string,
  input: CreateProjectInput,
): Promise<Project> {
  return apiRequest<Project>(`/storage-pools/${poolId}/projects`, {
    method: "POST",
    body: input,
  });
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

export function createBranch(
  poolId: string,
  projectId: string,
  input: CreateBranchInput,
): Promise<Branch> {
  return apiRequest<Branch>(
    `/storage-pools/${poolId}/projects/${projectId}/branches`,
    { method: "POST", body: input },
  );
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

export function createEndpoint(
  poolId: string,
  projectId: string,
  branchId: string,
  input: CreateEndpointInput,
): Promise<Endpoint> {
  return apiRequest<Endpoint>(
    `/storage-pools/${poolId}/projects/${projectId}/branches/${branchId}/endpoints`,
    { method: "POST", body: input },
  );
}
