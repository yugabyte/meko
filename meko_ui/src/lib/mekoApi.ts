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
export const PROVISION_STATUS_CACHE_KEY = "meko:provision-status:v1";
export const ONBOARDING_STATUS_CACHE_KEY = "meko:onboarding-status:v1";
export const ONBOARDING_STATUS_UPDATED_EVENT = "meko:onboarding-status-updated";
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
  id: string;
  name: string;
  account_id?: string;
  llm_model?: string;
  embedding_model?: string;
  created_at?: string;
  updated_at?: string;
};

export type DatapackStatus = {
  id: string;
  name: string;
  llm_model?: string;
  embedding_model?: string;
  created_at?: string;
  agent_count?: number;
  knowledge_base_count?: number;
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

type CreateDatapackPayload = {
  name: string;
  seed_knowledge_base: string;
  embed_model?: string;
  inference_model?: string;
};

export type CreatedDatapack = {
  datapack_id: string;
  agent_id: string;
  name: string;
  mcp_server?: {
    url?: string;
    api_key?: string;
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
  method: "GET" | "POST" | "PATCH" = "GET",
  body?: unknown,
): Promise<T> {
  try {
    const res = await apiClient.request<ApiEnvelope<T>>({
      url: path,
      method,
      data: body,
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
    if (axios.isAxiosError<ApiEnvelope<T>>(err)) {
      const status = err.response?.status;
      const apiMessage = err.response?.data?.error?.message;
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
      throw new Error(
        `${status || 500} ${err.response?.statusText || "Request failed"}`,
      );
    }
    throw err;
  }
}

export async function listDatapacks() {
  return request<Datapack[]>("/datapacks");
}

export async function getDatapackStatus(name: string) {
  return request<DatapackStatus>(`/datapacks/${encodeURIComponent(name)}/status`);
}

export async function listAgents(name: string) {
  return request<Agent[]>(`/datapacks/${encodeURIComponent(name)}/agents`);
}

export async function listKnowledgeBases(name: string) {
  return request<KnowledgeBase[]>(
    `/datapacks/${encodeURIComponent(name)}/knowledge-bases`,
  );
}

export async function createDatapack(payload: CreateDatapackPayload) {
  return request<CreatedDatapack>("/datapacks", "POST", payload);
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

export async function getCurrentUser() {
  return request<CurrentUser>("/auth/me", "GET");
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
