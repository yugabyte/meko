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
export const PROVISION_STATUS_CACHE_KEY = "meko:provision-status:v2";
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

export type DatapackConversation = {
  id: string;
  agent_id?: string;
  started_at: string;
  /** Verbatim user prompt of the first message in the session. Empty when
   *  the session has no messages yet — the UI renders a styled
   *  placeholder for that case rather than relying on a server-side
   *  string. */
  first_message: string;
  messages: number;
};

export type ListDatapackConversationsResponse = {
  datapack_id: string;
  datapack_name: string;
  limit: number;
  offset: number;
  conversations: DatapackConversation[];
};

/**
 * List Langfuse sessions (= conversations) for a datapack, newest first.
 * Returns 503 if the api server isn't wired to a Langfuse Postgres.
 */
export async function listDatapackConversations(
  datapackName: string,
  params: { limit?: number; offset?: number } = {},
  options: { signal?: AbortSignal } = {},
) {
  const qs = new URLSearchParams();
  if (params.limit !== undefined) qs.set("limit", String(params.limit));
  if (params.offset !== undefined) qs.set("offset", String(params.offset));
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<ListDatapackConversationsResponse>(
    `/datapacks/${encodeURIComponent(datapackName)}/conversations${suffix}`,
    "GET",
    undefined,
    options,
  );
}

export type DatapackConversationCount = {
  datapack_id: string;
  datapack_name: string;
  count: number;
};

/**
 * Total number of distinct Langfuse sessions (= conversations) for a
 * datapack. Returns 503 if the api server isn't wired to a Langfuse
 * Postgres.
 */
export async function getDatapackConversationCount(
  datapackName: string,
  options: { signal?: AbortSignal } = {},
) {
  return request<DatapackConversationCount>(
    `/datapacks/${encodeURIComponent(datapackName)}/conversations/count`,
    "GET",
    undefined,
    options,
  );
}

export async function ensureLangfuseProjectKeys(datapackName: string) {
  return request<LangfuseProjectKeys>(
    `/datapacks/${encodeURIComponent(datapackName)}/langfuse/project-keys`,
    "POST",
  );
}

/**
 * Per-datapack provisioning details (ysql conn string + username +
 * password, plus the account-level MCP URL/key for convenience).
 * Backed by GET /datapacks/:name/provision-status; the API refuses
 * if the caller doesn't own this datapack (RequireDatapackOwnership).
 */
export async function getDatapackProvisionStatus(
  datapackName: string,
  options: { signal?: AbortSignal } = {},
) {
  return request<ProvisionStatus>(
    `/datapacks/${encodeURIComponent(datapackName)}/provision-status`,
    "GET",
    undefined,
    options,
  );
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

/** One row from GET /datapacks/:name/knowledge-bases/documents (`data` array). */
export type KnowledgeBaseDocument = {
  document_name: string;
  status: string;
};

const KB_DOCS_MOCK_ON =
  import.meta.env.VITE_MOCK_KB_DOCUMENTS === "true" ||
  import.meta.env.VITE_MOCK_KB_DOCUMENTS === "1";
const KB_DOCS_MOCK_OFF = import.meta.env.VITE_MOCK_KB_DOCUMENTS === "false";

const MOCK_KB_DOCUMENTS_SAMPLE: KnowledgeBaseDocument[] = [
  { document_name: "policies/refunds.pdf", status: "QUEUED" },
  { document_name: "policies/shipping.pdf", status: "PROCESSING" },
  { document_name: "faq.md", status: "COMPLETED" },
  { document_name: "legacy-notes.txt", status: "FAILED" },
];

function useMockKnowledgeBaseDocuments(): boolean {
  if (KB_DOCS_MOCK_OFF) return false;
  if (KB_DOCS_MOCK_ON) return true;
  // Local UI work without the GET handler yet; set VITE_MOCK_KB_DOCUMENTS=false to hit the real API in dev.
  return import.meta.env.DEV;
}

/**
 * GET /datapacks/:name/knowledge-bases/documents — `data`: `{ document_name, status }[]`.
 * - Dev: returns {@link MOCK_KB_DOCUMENTS_SAMPLE} unless `VITE_MOCK_KB_DOCUMENTS=false`.
 * - Prod: calls the API unless `VITE_MOCK_KB_DOCUMENTS=true`.
 */
export async function getKnowledgeBaseDocuments(
  datapackName: string,
  options: { signal?: AbortSignal } = {},
): Promise<KnowledgeBaseDocument[]> {
  // if (useMockKnowledgeBaseDocuments()) {
  //   return Promise.resolve([...MOCK_KB_DOCUMENTS_SAMPLE]);
  // }
  return request<KnowledgeBaseDocument[]>(
    `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/documents`,
    "GET",
    undefined,
    options,
  );
}

// --- Memories + tenant-KB promotion APIs ---

/** Tier sent as `user_type` on GET /memories. */
export type MemoryUserType = "free" | "pro" | "enterprise";

/** Default tier when callers omit `user_type` (product default). */
export const MEMORY_LIST_DEFAULT_USER_TYPE: MemoryUserType = "pro";

/**
 * One row in GET /memories `data.memories` (same shape used in UI + promoted cache).
 */
export type Memory = {
  id: string;
  data: string;
  agent_id?: string;
};

/**
 * GET /memories query params — only these keys are sent:
 * `datapack_id`, `user_id`, `user_type`, `page`, `page_size`.
 *
 * `user_type` defaults to {@link MEMORY_LIST_DEFAULT_USER_TYPE} when omitted.
 */
export type MemoryListFilter = {
  datapack_id: string;
  user_id: string;
  user_type?: MemoryUserType;
  page: number;
  page_size: number;
};

/** Echoed inside GET /memories `data.criteria` (shape may grow server-side). */
export type MemoryListCriteria = {
  datapack_id?: string;
  page?: number;
  page_size?: number;
};

/**
 * GET /memories `data` payload. The HTTP envelope `{ data, error }` is handled by {@link request},
 * which returns this object as `T` (i.e. `data` only).
 */
export type MemoryListResponse = {
  criteria: MemoryListCriteria;
  memories: Memory[];
  top_k: number;
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
};

const MEMORIES_MOCK_ON =
  import.meta.env.VITE_MOCK_MEMORIES === "true" || import.meta.env.VITE_MOCK_MEMORIES === "1";
const MEMORIES_MOCK_OFF = import.meta.env.VITE_MOCK_MEMORIES === "false";

function useMockMemoriesList(): boolean {
  if (MEMORIES_MOCK_OFF) return false;
  if (MEMORIES_MOCK_ON) return true;
  return import.meta.env.DEV;
}

/** Fixed rows for local UI when {@link useMockMemoriesList} is active. */
const MOCK_MEMORIES_ALL: Memory[] = [
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e101", data: "Sample memory text number 1", agent_id: "agent_alpha" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e102", data: "Sample memory text number 2", agent_id: "agent_beta" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e103", data: "Sample memory text number 3", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e104", data: "Sample memory text number 4" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e105", data: "Sample memory text number 5", agent_id: "agent_alpha" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e106", data: "Sample memory text number 6", agent_id: "agent_beta" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e107", data: "Sample memory text number 7", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e108", data: "Sample memory text number 8", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e109", data: "Sample memory text number 9", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e110", data: "Sample memory text number 10", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e111", data: "Sample memory text number 11", agent_id: "agent_gamma" },
  { id: "122405bb-5ff9-4030-9b72-fcb996d1e112", data: "Sample memory text number 12", agent_id: "agent_gamma" },
];

function buildMockMemoryListResponse(params: MemoryListFilter): MemoryListResponse {
  const total = MOCK_MEMORIES_ALL.length;
  const page = Math.max(1, params.page);
  const pageSize = Math.max(1, params.page_size);
  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const safePage = Math.min(page, totalPages);
  const start = (safePage - 1) * pageSize;
  const memories = MOCK_MEMORIES_ALL.slice(start, start + pageSize);
  return {
    criteria: {
      datapack_id: params.datapack_id,
      page: safePage,
      page_size: pageSize,
    },
    memories,
    top_k: 0,
    total,
    page: safePage,
    page_size: pageSize,
    total_pages: totalPages,
  };
}

/**
 * GET /memories. In dev, returns {@link buildMockMemoryListResponse} unless `VITE_MOCK_MEMORIES=false`;
 * set `VITE_MOCK_MEMORIES=true` in any mode to force mock; omit both to use the real API in production.
 */
export async function listMemories(
  params: MemoryListFilter,
  options: { signal?: AbortSignal } = {},
): Promise<MemoryListResponse> {
  // if (useMockMemoriesList()) {
  //   return Promise.resolve(buildMockMemoryListResponse(params));
  // }
  const qs = new URLSearchParams();
  qs.set("datapack_id", params.datapack_id);
  qs.set("user_id", params.user_id);
  qs.set("user_type", params.user_type ?? MEMORY_LIST_DEFAULT_USER_TYPE);
  qs.set("page", String(params.page));
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

