import axios from "axios";
import { dispatchAppEvent } from "@/lib/browserEvents";

type ApiError = {
  code?: string;
  message?: string;
};

type ApiEnvelope<T> = {
  data: T;
  error?: ApiError | null;
};

export const AUTH_EXPIRED_EVENT = "meko:auth-expired";
export const AUTH_LOGIN_SUCCESS_EVENT = "meko:auth-login-success";
export const AUTH_LOGOUT_EVENT = "meko:auth-logout";
export const PROVISION_STATUS_CACHE_KEY = "meko:provision-status:v1";
export const ONBOARDING_STATUS_CACHE_KEY = "meko:onboarding-status:v1";
export const ONBOARDING_STATUS_UPDATED_EVENT = "meko:onboarding-status-updated";
/** Fired after a datapack is deleted or renamed so the sidebar can refresh its list. */
export const DATAPACKS_LIST_UPDATED_EVENT = "meko:datapacks-list-updated";
let authExpiredNotified = false;
const AUTH_PUBLIC_ENDPOINTS = new Set([
  "/auth/me",
  "/auth/login",
  "/auth/signup",
  "/auth/confirm-signup",
]);

function notifyAuthExpiredOnce() {
  if (authExpiredNotified) return;
  authExpiredNotified = true;
  dispatchAppEvent(AUTH_EXPIRED_EVENT);
}

export function resetAuthExpiredState() {
  authExpiredNotified = false;
}

export type Datapack = {
  datapack_id: string;
  datapack_name: string;
  user_id: string;
  universe_id?: string;
  datapack_ysql_connection_string?: string;
  datapack_ysql_username?: string;
  created_at?: string;
  updated_at?: string;
  /** From `datapack_details` / list+describe responses when present. */
  description?: string;
  memory_count?: number;
  knowledge_count?: number;
  learnings_count?: number;
  /** Optional; included when the API aggregates pipeline-style counts. */
  conversations_count?: number;
};

export type Agent = {
  id: string;
  datapack_id: string;
  agent_name: string;
  agent_metadata?: string;
  created_at?: string;
  updated_at?: string;
};

export type KnowledgeBase = {
  id: string;
  datapack_id: string;
  knowledge_base_url: string;
  created_at?: string;
  updated_at?: string;
};

/**
 * Body for POST /datapacks — mirrors `api_server/internal/service.CreateRequest` JSON.
 *
 * - **name** — required (non-empty after trim).
 * - **description** — optional; omit, `null`, or blank if you have none (stored in `datapack_details` when non-empty).
 * - **agent_id** — optional; omit, `null`, or `""` to use the default agent (`meko_agent`). Any other value must already exist for the user (create via `POST /agents` first).
 * - **user_id** — not part of the client contract: the handler always sets it from the authenticated account, so do not send it from the browser.
 */
export type CreateDatapackPayload = {
  name: string;
  user_id: string;
  description?: string | null;
  agent_id?: string | null;
};

/**
 * `data` from POST /datapacks — matches `api_server/internal/service.CreateResponse`.
 */
export type CreatedDatapack = {
  datapack_id: string;
  agent_id: string;
  name: string;
  mcp_user_name: string;
  mcp_server: {
    url: string;
    api_key: string;
  };
  db_access: {
    user_name: string;
    password: string;
    conn_str: string;
  };
};

export type SignupPayload = {
  email: string;
  password: string;
  first_name: string;
  last_name: string;
};

export type SignupResponse = {
  account_id: string;
  email: string;
  message: string;
};

export type ConfirmSignupPayload = {
  email: string;
  code: string;
};

export type LoginPayload = {
  email: string;
  password: string;
};

export type LoginResponse = {
  access_token: string;
  id_token: string;
  refresh_token: string;
  expires_in: number;
};

export type CurrentUser = {
  account_id: string;
  email: string;
  first_name: string;
  last_name: string;
  has_completed_onboarding: boolean;
};

export type LangfuseProjectKeys = {
  project_id?: string;
  project_name?: string;
  public_key: string;
  secret_key: string;
};

export type ProvisionStatus = {
  datapack_id: string;
  datapack_name: string;
  mcp_server_url: string;
  mcp_api_key: string;
  db_user_name: string;
  db_password: string;
  db_conn_str: string;
};

const API_BASE = (import.meta.env.VITE_MEKO_API_BASE_URL || "/api/v1").replace(
  /\/$/,
  "",
);
const PROVISION_STATUS_PATH = "/internal/provision-status";

const apiClient = axios.create({
  baseURL: API_BASE,
  withCredentials: true,
  headers: {
    "Content-Type": "application/json",
  },
});

async function request<T>(
  path: string,
  method: "GET" | "POST" | "PATCH" | "DELETE" = "GET",
  body?: unknown,
  options: { signal?: AbortSignal } = {},
): Promise<T> {
  try {
    const res = await apiClient.request<ApiEnvelope<T>>({
      url: path,
      method,
      data: method === "GET" ? undefined : body,
      signal: options.signal,
    });

    const payload = res.data;
    if (payload?.error) {
      throw new Error(payload.error.message || "API request failed");
    }
    if (path === "/auth/login" || path === "/auth/me") {
      resetAuthExpiredState();
    }
    return payload.data;
  } catch (err) {
    if (axios.isCancel(err)) {
      throw err;
    }
    if (axios.isAxiosError<ApiEnvelope<T>>(err)) {
      if (!err.response) {
        if (err.code === "ECONNABORTED" || /timeout/i.test(err.message || "")) {
          throw new Error("Request timed out. Please try again.");
        }
        throw new Error("Network error. Unable to reach the server.");
      }
      const status = err.response.status;
      const apiMessage = err.response.data?.error?.message;
      if (status === 401) {
        if (AUTH_PUBLIC_ENDPOINTS.has(path)) {
          throw new Error(apiMessage || "Unauthorized");
        }
        notifyAuthExpiredOnce();
        throw new Error(apiMessage || "Session expired. Please sign in again.");
      }
      if (apiMessage) {
        throw new Error(apiMessage);
      }
      throw new Error(`${status} ${err.response.statusText || "Request failed"}`);
    }
    throw err;
  }
}

/**
 * Maps errors from {@link request} (and raw axios failures) to a string safe to show in UI.
 * Prefer this over `err instanceof Error ? err.message : "…"` so network and HTTP cases read clearly.
 */
export function getApiErrorMessage(err: unknown, fallback = "Something went wrong. Please try again."): string {
  if (axios.isCancel(err)) {
    return "Request was cancelled.";
  }
  if (axios.isAxiosError(err)) {
    const data = err.response?.data as ApiEnvelope<unknown> | undefined;
    const apiMessage = data?.error?.message;
    if (typeof apiMessage === "string" && apiMessage.trim()) {
      return apiMessage.trim();
    }
    if (!err.response) {
      if (err.code === "ECONNABORTED" || /timeout/i.test(err.message || "")) {
        return "Request timed out. Please try again.";
      }
      return "Network error. Unable to reach the server.";
    }
    const status = err.response.status;
    const text = err.response.statusText || "Request failed";
    if (status === 404) {
      return `Not found (${text}).`;
    }
    if (status === 403) {
      return "You don't have permission to do that.";
    }
    if (status >= 500) {
      return "Server error. Please try again later.";
    }
    return `${status} ${text}`.trim();
  }
  if (err instanceof Error && err.message.trim()) {
    return err.message.trim();
  }
  if (typeof err === "string" && err.trim()) {
    return err.trim();
  }
  return fallback;
}

export async function listDatapacks() {
  return request<Datapack[]>("/datapacks");
}

/** GET /datapacks/:name — full datapack row (includes `description` when set). */
export async function getDatapack(name: string, options: { signal?: AbortSignal } = {}) {
  return request<Datapack>(`/datapacks/${encodeURIComponent(name)}`, "GET", undefined, options);
}

export async function listAgents(name: string) {
  return request<Agent[]>(`/datapacks/${encodeURIComponent(name)}/agents`);
}

export async function listKnowledgeBases(
  name: string,
  options: { signal?: AbortSignal } = {},
) {
  return request<KnowledgeBase[]>(
    `/datapacks/${encodeURIComponent(name)}/knowledge-bases`,
    "GET",
    undefined,
    options,
  );
}

export async function createDatapack(payload: CreateDatapackPayload) {
  const body: Record<string, unknown> = { name: payload.name.trim() };
  if (payload.description != null && String(payload.description).trim() !== "") {
    body.description = String(payload.description).trim();
  }
  body.user_id = payload.user_id;
  body.agent_id = payload.agent_id ?? null;
  return request<CreatedDatapack>("/datapacks", "POST", body);
}

/**
 * Body for PATCH /datapacks/:name — matches `api_server/internal/service.UpdateRequest` JSON tags.
 * Only send fields you want to change; omitted keys are left unchanged on the server.
 */
export type UpdateDatapackPayload = {
  name?: string;
  description?: string;
  datapack_ysql_connection_string?: string;
};

/** `data` from PATCH /datapacks/:name — matches handler `ok(..., map[string]string{message})`. */
export type UpdateDatapackResponse = {
  message: string;
};

/** `data` from DELETE /datapacks/:name. */
export type DeleteDatapackResponse = {
  message: string;
};

export async function updateDatapack(name: string, payload: UpdateDatapackPayload) {
  return request<UpdateDatapackResponse>(
    `/datapacks/${encodeURIComponent(name)}`,
    "PATCH",
    payload,
  );
}

export async function deleteDatapack(name: string) {
  return request<DeleteDatapackResponse>(`/datapacks/${encodeURIComponent(name)}`, "DELETE");
}

export async function signup(payload: SignupPayload) {
  return request<SignupResponse>("/auth/signup", "POST", payload);
}

export async function confirmSignup(payload: ConfirmSignupPayload) {
  return request<{ message: string }>("/auth/confirm-signup", "POST", payload);
}

export async function login(payload: LoginPayload) {
  const data = await request<LoginResponse>("/auth/login", "POST", payload);
  dispatchAppEvent(AUTH_LOGIN_SUCCESS_EVENT);
  return data;
}

export async function logout() {
  return request<{ message: string }>("/auth/logout", "POST");
}

export async function getCurrentUser(options: { signal?: AbortSignal } = {}) {
  return request<CurrentUser>("/auth/me", "GET", undefined, options);
}

export async function updateOnboardingStatus(hasCompletedOnboarding: boolean) {
  return request<{ message: string; has_completed_onboarding: boolean }>(
    "/auth/me",
    "PATCH",
    { has_completed_onboarding: hasCompletedOnboarding },
  );
}

export async function bootstrapObserveSession() {
  return request<{ message: string; org_id?: string }>("/observe/session/bootstrap", "POST");
}

export async function ensureLangfuseProjectKeys(datapackName: string) {
  return request<LangfuseProjectKeys>(
    `/datapacks/${encodeURIComponent(datapackName)}/langfuse/project-keys`,
    "POST",
  );
}

export async function getProvisionStatus() {
  return request<ProvisionStatus>(PROVISION_STATUS_PATH, "GET");
}

// --- Knowledge Base file upload & RAG APIs ---

export async function requestUploadURL(
  datapackName: string,
  filename: string,
  contentType: string,
  size: number,
) {
  return request<{ url: string; key: string }>(
    `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/upload-url`,
    "POST",
    { filename, content_type: contentType, size },
  );
}

export async function confirmUpload(
  datapackName: string,
  key: string,
  filename: string,
  size: number,
) {
  return request<{ message: string; key: string }>(
    `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/upload-complete`,
    "POST",
    { key, filename, size },
  );
}

export async function createKnowledgeBase(datapackName: string) {
  return request<{
    source_id: string;
    index_name: string;
    source_uri: string;
    status: string;
  }>(
    `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/create`,
    "POST",
  );
}

export type SourceStatus = {
  source_id: string;
  source_uri: string;
  status: string;
};

export type PipelineDetail = {
  index_name: string;
  source_uri: string;
  document_name: string;
  document_status: string;
  chunks_processed: number | null;
};

export type KBPipelineStatus = {
  index_name: string;
  source_statuses: SourceStatus[] | null;
  pipeline_details: PipelineDetail[] | null;
};

export async function getKBStatus(datapackName: string) {
  return request<KBPipelineStatus>(
    `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/status`,
  );
}

// --- Memories + tenant-KB promotion APIs ---

export type MemoryPayload = {
  data?: string;
  user_id?: string;
  agent_id?: string;
  datapack_id?: string;
  [key: string]: unknown;
};

export type Memory = {
  id: string;
  payload: MemoryPayload;
};

/**
 * Filter for GET /api/v1/memories.
 *
 * Identifier semantics (server-side contract):
 * - `user_id`:     Meko `account_id` of the currently logged-in user
 *                  (`CurrentUser.account_id`, == `meko_system.account_registry.id`).
 *                  The backend filters mem0 rows on `payload->>'user_id'` with
 *                  this value, and every memory SDKs write now stamps the
 *                  account_id into `payload.user_id`. Pass `""` to skip the
 *                  row-level filter and return memories for every account
 *                  under this datapack (admin/debug use).
 * - `datapack_id`: Datapack UUID (`meko_system.datapack_registry.id`).
 *                  Required — picks which tenant DB to connect to.
 * - `agent_id`:    Agent name. When set, only that agent's
 *                  `{agent_id}_mem0_collection` table is read; when empty,
 *                  every `*_mem0_collection` table is unioned.
 * - `free_user`:   If true, skip vector similarity ranking (plain SELECT
 *                  ordered by id). Use for free-tier users to avoid the
 *                  embedding/LLM summarization cost.
 * - `page`:        1-based page number. Defaults to 1 on the server.
 * - `page_size`:   Items per page. Defaults to 20, capped at 100 by the server.
 */
export type MemoryListFilter = {
  user_id: string;
  datapack_id: string;
  agent_id?: string;
  free_user?: boolean;
  page?: number;
  page_size?: number;
};

export type MemoryListResponse = {
  criteria: MemoryListFilter;
  memories: Memory[];
  top_k: number;
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
};

export async function listMemories(
  params: MemoryListFilter,
  options: { signal?: AbortSignal } = {},
) {
  const qs = new URLSearchParams();
  qs.set("user_id", params.user_id);
  qs.set("datapack_id", params.datapack_id);
  if (params.agent_id) qs.set("agent_id", params.agent_id);
  if (params.free_user) qs.set("free_user", "true");
  if (params.page && params.page > 0) qs.set("page", String(params.page));
  if (params.page_size && params.page_size > 0)
    qs.set("page_size", String(params.page_size));
  return request<MemoryListResponse>(`/memories?${qs.toString()}`, "GET", undefined, options);
}

/**
 * Body for POST /api/v1/tenant-kb/sync.
 *
 * Identifier semantics:
 * - `user_id`:     Meko `account_id` of the logged-in user (same semantics
 *                  as `MemoryListFilter.user_id`). The backend both filters
 *                  the mem0 fetch on `payload->>'user_id' = user_id` and
 *                  writes this value into `knowledge_base.user_id`.
 * - `datapack_id`: Datapack UUID; routes the sync to the correct tenant DB
 *                  and is written into `knowledge_base.datapack_id`.
 * - `agent_id`:    Optional; when set, narrows the mem0 source table to
 *                  `{agent_id}_mem0_collection`.
 * - `memory_ids`:  IDs from `{agent}_mem0_collection.id` that should be
 *                  upserted into `knowledge_base`. Rows whose source
 *                  `memory_id` already exists (matched via
 *                  `metadata_filters->>'memory_id'`) are UPDATEd; others
 *                  are INSERTed. Rows missing from mem0 or with NULL
 *                  vectors are reported in `not_found_ids`.
 */
export type TenantKBSyncRequest = {
  user_id: string;
  datapack_id: string;
  agent_id?: string;
  memory_ids: string[];
};

export type TenantKBSyncResult = {
  inserted_ids: string[];
  updated_ids: string[];
  not_found_ids: string[];
};

export async function syncMemoriesToKB(payload: TenantKBSyncRequest) {
  return request<TenantKBSyncResult>("/tenant-kb/sync", "POST", payload);
}

