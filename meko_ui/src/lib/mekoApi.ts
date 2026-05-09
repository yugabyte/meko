import axios from "axios";
import type { DatapackShareGrant } from "./datapackShareGrants";
import { dispatchAppEvent } from "@/lib/browserEvents";

type ApiError = {
  code?: string;
  message?: string;
};

/**
 * Thrown by {@link request} when the API returns a structured `error` object
 * (HTTP 4xx/5xx with `{ error: { code, message } }`). Lets auth UI branch on
 * codes such as {@link API_ERROR_CODE_USER_NOT_CONFIRMED} without parsing strings.
 */
export class MekoApiRequestError extends Error {
  readonly code?: string;
  readonly httpStatus?: number;

  constructor(message: string, opts?: { code?: string; httpStatus?: number }) {
    super(message);
    this.name = "MekoApiRequestError";
    this.code = opts?.code;
    this.httpStatus = opts?.httpStatus;
  }
}

export function isMekoApiRequestError(err: unknown): err is MekoApiRequestError {
  return err instanceof MekoApiRequestError;
}

/** Cognito sign-in blocked until email verified; client may offer resend + confirm flow. */
export const API_ERROR_CODE_USER_NOT_CONFIRMED = "USER_NOT_CONFIRMED" as const;

/**
 * Returned from POST /auth/resend-confirmation when the email is already
 * verified. UI should treat this as success-with-nudge (close the resend
 * recovery panel and prompt the user to sign in) rather than a red error.
 */
export const API_ERROR_CODE_USER_ALREADY_CONFIRMED = "USER_ALREADY_CONFIRMED" as const;

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
  /** Same-session pre-login; triggers Cognito ResendConfirmationCode. */
  "/auth/resend-confirmation",
  /** Cognito ForgotPassword / ConfirmForgotPassword (no session). */
  "/auth/forgot-password",
  "/auth/forgot-password/confirm",
  /** Completes NEW_PASSWORD_REQUIRED after /auth/login challenge (cookies set on success). */
  "/auth/complete-new-password",
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
  datapack_owner_name?: string;
  /** Owner account email from `users.user_email` (list + describe). */
  datapack_owner_email_id?: string;
  /**
   * From `GET /datapacks`: `"owner"` for datapacks you created; invite grant (`viewer` | `contributor` | `maintainer`)
   * for datapacks shared with you.
   */
  grant?: DatapackShareGrant | null;
};

/** Linked agents from `GET /datapacks/:name/agents` (matches `models.Agent` JSON). */
export type Agent = {
  agent_id: string;
  user_id?: string;
  agent_details?: unknown;
  created_at?: string;
  updated_at?: string;
};

/** Knowledge sources from `GET /datapacks/:name/knowledge-bases` (matches `models.KnowledgeBase` JSON). */
export type KnowledgeBase = {
  rag_source_id?: string;
  datapack_id?: string;
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

export type AuthSessionTokens = {
  access_token: string;
  id_token: string;
  refresh_token: string;
  expires_in: number;
};


/** Cognito challenge from POST /auth/login when the user must set a new password first. */
export const AUTH_CHALLENGE_NEW_PASSWORD_REQUIRED = "NEW_PASSWORD_REQUIRED" as const;
export type LoginNewPasswordChallengeResponse = {
  challenge: typeof AUTH_CHALLENGE_NEW_PASSWORD_REQUIRED;
  session: string;
  email: string;
};

export type LoginResponse = AuthSessionTokens | LoginNewPasswordChallengeResponse;

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

export function isNewPasswordChallengeResponse(
  data: LoginResponse,
): data is LoginNewPasswordChallengeResponse {
  return (
    "challenge" in data &&
    data.challenge === AUTH_CHALLENGE_NEW_PASSWORD_REQUIRED &&
    typeof data.session === "string" &&
    data.session.trim().length > 0 &&
    typeof data.email === "string" &&
    data.email.trim().length > 0
  );
}

export type CompleteNewPasswordPayload = {
  email: string;
  session: string;
  new_password: string;
  confirm_password: string;
  first_name: string;
  last_name: string;
};

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
      throw new MekoApiRequestError(payload.error.message || "API request failed", {
        code: payload.error.code,
        httpStatus: res.status,
      });
    }
    if (
      path === "/auth/login" ||
      path === "/auth/me" ||
      path === "/auth/complete-new-password"
    ) {
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
      const apiErr = err.response.data?.error;
      const apiMessage = apiErr?.message;
      if (status === 401) {
        if (AUTH_PUBLIC_ENDPOINTS.has(path)) {
          throw new MekoApiRequestError(apiMessage || "Unauthorized", {
            code: apiErr?.code,
            httpStatus: status,
          });
        }
        notifyAuthExpiredOnce();
        throw new MekoApiRequestError(apiMessage || "Session expired. Please sign in again.", {
          code: apiErr?.code,
          httpStatus: status,
        });
      }
      if (apiMessage) {
        throw new MekoApiRequestError(apiMessage, {
          code: apiErr?.code,
          httpStatus: status,
        });
      }
      throw new MekoApiRequestError(`${status} ${err.response.statusText || "Request failed"}`, {
        httpStatus: status,
      });
    }
    throw err;
  }
}

/**
 * Maps errors from {@link request} (and raw axios failures) to a string safe to show in UI.
 * Prefer this over `err instanceof Error ? err.message : "…"` so network and HTTP cases read clearly.
 */
export function getApiErrorMessage(err: unknown, fallback = "Something went wrong. Please try again."): string {
  if (isMekoApiRequestError(err)) {
    return err.message.trim() || fallback;
  }
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

type DatapackRouteOptions = { signal?: AbortSignal; datapackId?: string };

function withDatapackID(path: string, datapackId?: string): string {
  const id = (datapackId ?? "").trim();
  if (!id) return path;
  const sep = path.includes("?") ? "&" : "?";
  return `${path}${sep}datapack_id=${encodeURIComponent(id)}`;
}

/** GET /datapacks/:name — full datapack row (includes `description` when set). */
export async function getDatapack(name: string, options: DatapackRouteOptions = {}) {
  return request<Datapack>(
    withDatapackID(`/datapacks/${encodeURIComponent(name)}`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

export async function listAgents(name: string, options: DatapackRouteOptions = {}) {
  return request<Agent[]>(
    withDatapackID(`/datapacks/${encodeURIComponent(name)}/agents`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

export async function listKnowledgeBases(
  name: string,
  options: DatapackRouteOptions = {},
) {
  return request<KnowledgeBase[]>(
    withDatapackID(`/datapacks/${encodeURIComponent(name)}/knowledge-bases`, options.datapackId),
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

export async function updateDatapack(
  name: string,
  payload: UpdateDatapackPayload,
  options: DatapackRouteOptions = {},
) {
  return request<UpdateDatapackResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(name)}`, options.datapackId),
    "PATCH",
    payload,
    options,
  );
}

export async function deleteDatapack(name: string, options: DatapackRouteOptions = {}) {
  return request<DeleteDatapackResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(name)}`, options.datapackId),
    "DELETE",
    undefined,
    options,
  );
}

// --- Datapack share / invites (paths are under `API_BASE`, typically `/api/v1`) ---

export type { DatapackShareGrant };
export {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANTS,
  isDatapackShareGrant,
} from "./datapackShareGrants";

/** Recipient action on a pending invite. */
export type DatapackInviteUserAction = "accept" | "decline";

/** Body for POST /datapacks/:name/share */
export type InviteDatapackSharePayload = {
  recipient_email: string;
  grant: DatapackShareGrant;
};

/** `data` from POST /datapacks/:name/share */
export type InviteDatapackShareResponse = {
  status: string;
  invite_id: string;
  recipient_email: string;
  grant: DatapackShareGrant;
};

/**
 * Invite someone to a datapack.
 * POST /datapacks/:name/share
 */
export async function inviteDatapackShare(
  datapackName: string,
  payload: InviteDatapackSharePayload,
  options: DatapackRouteOptions = {},
) {
  return request<InviteDatapackShareResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/share`, options.datapackId),
    "POST",
    payload,
    options,
  );
}

/** One row from GET /datapacks/incoming-shares */
export type DatapackIncomingShare = {
  invite_id: string;
  owner_user_id: string;
  owner_name: string;
  grant: DatapackShareGrant;
  datapack_name: string;
  datapack_id: string;
};

/**
 * Invites for the signed-in recipient (pending shares to accept or decline).
 * GET /datapacks/incoming-shares — `data` is an array (empty when none).
 */
export async function listDatapackIncomingShares(options: { signal?: AbortSignal } = {}) {
  return request<DatapackIncomingShare[]>("/datapacks/incoming-shares", "GET", undefined, options);
}

/** Body for POST /datapacks/invite-action */
export type DatapackInviteActionPayload = {
  invite_id: string;
  /** Same value as {@link CurrentUser.account_id} for the signed-in recipient. */
  recipient_user_id: string;
  user_action: DatapackInviteUserAction;
};

/**
 * Accept or decline a datapack share invite.
 * POST /datapacks/invite-action
 */
export async function postDatapackInviteAction(payload: DatapackInviteActionPayload) {
  return request<unknown>("/datapacks/invite-action", "POST", payload);
}

/** Active member on an outgoing share listing */
export type DatapackOutgoingMember = {
  user_id: string;
  is_owner: boolean;
  user_name: string;
  user_email_id: string;
  grant: DatapackShareGrant;
};

/** Pending invite on an outgoing share listing */
export type DatapackOutgoingPendingInvite = {
  invite_id: string;
  grant: DatapackShareGrant;
  recipient_user_id?: string | null;
  recipient_user_name?: string | null;
  recipient_email_id: string;
};

/** `data` from GET /datapacks/:name/outgoing-shares (pending invites only; owner or maintainer). */
export type DatapackOutgoingSharesResponse = {
  pending_invites: DatapackOutgoingPendingInvite[];
};

/** `data` from GET /datapacks/:name/member-list (accepted members; any datapack access). */
export type DatapackMembersResponse = {
  members: DatapackOutgoingMember[];
};

/**
 * List pending outgoing invites for a datapack.
 * Owner or maintainer only (403 for viewer/contributor).
 * GET /datapacks/:name/outgoing-shares
 */
export async function listDatapackOutgoingShares(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<DatapackOutgoingSharesResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/outgoing-shares`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

/**
 * List accepted members for a datapack (including owner row).
 * Any grant with datapack access. GET /datapacks/:name/member-list
 */
export async function listDatapackMembers(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<DatapackMembersResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/member-list`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

/** Combined members + pending invites for share UI (from member-list + outgoing-shares). */
export type DatapackShareListsResponse = {
  members: DatapackOutgoingMember[];
  pending_invites: DatapackOutgoingPendingInvite[];
};

/**
 * Resend a pending invite (owner).
 * POST /datapacks/:name/invite/:inviteId/resend
 */
export async function resendDatapackInvite(
  datapackName: string,
  inviteId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/invite/${encodeURIComponent(inviteId)}/resend`,
      options.datapackId,
    ),
    "POST",
    undefined,
    options,
  );
}

/**
 * Revoke a pending invite before acceptance (owner).
 * DELETE /datapacks/:name/invite/:inviteId
 */
export async function revokeDatapackInvite(
  datapackName: string,
  inviteId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/invite/${encodeURIComponent(inviteId)}`,
      options.datapackId,
    ),
    "DELETE",
    undefined,
    options,
  );
}

/** Body for PATCH /datapacks/:name/share/:shareUserId */
export type UpdateDatapackMemberGrantPayload = {
  grant: DatapackShareGrant;
};

/**
 * TODO: This is a stretch goal for the MVP.
 * Update an active member’s grant (owner).
 * PATCH /datapacks/:name/share/:shareUserId
 */
export async function updateDatapackMemberGrant(
  datapackName: string,
  shareUserId: string,
  payload: UpdateDatapackMemberGrantPayload,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/share/${encodeURIComponent(shareUserId)}`,
      options.datapackId,
    ),
    "PATCH",
    payload,
    options,
  );
}

/**
 * TODO: This is a stretch goal for the MVP.
 * Remove a member’s access / delete active share (owner).
 * DELETE /datapacks/:name/share/:shareUserId
 */
export async function removeDatapackMemberShare(
  datapackName: string,
  shareUserId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/share/${encodeURIComponent(shareUserId)}`,
      options.datapackId,
    ),
    "DELETE",
    undefined,
    options,
  );
}

export async function signup(payload: SignupPayload) {
  return request<SignupResponse>("/auth/signup", "POST", payload);
}

export async function confirmSignup(payload: ConfirmSignupPayload) {
  return request<{ message: string }>("/auth/confirm-signup", "POST", payload);
}

export type ResendConfirmationPayload = {
  email: string;
};

/**
 * POST /auth/resend-confirmation — asks AWS Cognito to email a new verification
 * code for an account in UNCONFIRMED state (user signed up but never entered the code).
 * Rate-limited by Cognito; does not sign the user in.
 */
export async function resendSignupConfirmation(payload: ResendConfirmationPayload) {
  return request<{ message: string }>("/auth/resend-confirmation", "POST", {
    email: payload.email.trim(),
  });
}

export type ForgotPasswordStartPayload = {
  email: string;
};

/**
 * POST /auth/forgot-password — Cognito sends a password-reset code to the user’s email.
 * 404 with NOT_FOUND when no pool user exists for that email (typical Cognito behavior).
 */
export async function forgotPasswordSendCode(payload: ForgotPasswordStartPayload) {
  return request<{ message: string }>("/auth/forgot-password", "POST", {
    email: payload.email.trim(),
  });
}

export type ForgotPasswordConfirmPayload = {
  email: string;
  code: string;
  new_password: string;
  confirm_password: string;
};

/** POST /auth/forgot-password/confirm — Cognito ConfirmForgotPassword (sets new password). */
export async function forgotPasswordConfirm(payload: ForgotPasswordConfirmPayload) {
  return request<{ message: string }>("/auth/forgot-password/confirm", "POST", {
    email: payload.email.trim(),
    code: payload.code.trim(),
    new_password: payload.new_password,
    confirm_password: payload.confirm_password,
  });
}

/**
 * Signs in via POST /auth/login (Cognito USER_PASSWORD_AUTH).
 *
 * - When tokens are issued: sets auth cookies server-side and dispatches
 *   {@link AUTH_LOGIN_SUCCESS_EVENT}.
 * - When Cognito returns {@link AUTH_CHALLENGE_NEW_PASSWORD_REQUIRED}, the API
 *   returns `{ challenge, session, email }` with no cookies; the client must not
 *   treat that as a logged-in session — drive POST /auth/complete-new-password instead.
 *
 * If the user never confirmed their email, Cognito rejects login; the API maps
 * that to HTTP 403 with `error.code` {@link API_ERROR_CODE_USER_NOT_CONFIRMED}
 * so the sign-in page can offer resend + navigation to the confirm-code step.
 */
export async function login(payload: LoginPayload) {
  const data = await request<LoginResponse>("/auth/login", "POST", payload);
  if ("access_token" in data && Boolean(data.access_token)) {
    dispatchAppEvent(AUTH_LOGIN_SUCCESS_EVENT);
  }
  return data;
}

/**
 * POST /auth/complete-new-password — finishes NEW_PASSWORD_REQUIRED; sets auth cookies like login.
 * Does not dispatch {@link AUTH_LOGIN_SUCCESS_EVENT}: the UI typically signs the user out and
 * sends them to `/login` so they sign in fresh with the new password.
 */
export async function completeNewPassword(payload: CompleteNewPasswordPayload) {
  return request<AuthSessionTokens>("/auth/complete-new-password", "POST", {
    email: payload.email.trim(),
    session: payload.session,
    new_password: payload.new_password,
    confirm_password: payload.confirm_password,
    first_name: payload.first_name.trim(),
    last_name: payload.last_name.trim(),
  });
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
  /** Falls back to the truncated first message when MCP didn't pass a
   *  title to conversation_create. May be empty for very old sessions
   *  that have neither. */
  title?: string;
  started_at: string;
  /** MAX(observation.end_time) across the trace; equals started_at for
   *  sessions whose only observation is the zero-duration bookmark. */
  last_activity_at: string;
  /** Verbatim user prompt of the first message in the session. Empty when
   *  the session has no messages yet — the UI renders a styled
   *  placeholder for that case rather than relying on a server-side
   *  string. */
  first_message: string;
  messages: number;
  /** Count of *persisted* mem0 rows attributed to this conversation in
   *  datapacks.meko_mem0_collection — i.e. the same number the Memories
   *  tab shows for this session, after Mem0's fact extractor and dedup
   *  ran. NOT the count of `memory_add` tool invocations: those can
   *  return zero or many facts per call, so the two diverge in practice
   *  (vague text → 0 persisted, rich text → multiple). */
  memories_added: number;
  llm_calls: number;
};

export type ListDatapackConversationsResponse = {
  datapack_id: string;
  datapack_name: string;
  /** Langfuse project_id resolved from meko_system.langfuse_project_mapping
   *  for this datapack. Empty when no Langfuse mapping exists yet. The UI
   *  uses it to build per-conversation deep links into the embedded
   *  Observe iframe (/observe/project/<pid>/sessions/<sid>). */
  langfuse_project_id?: string;
  limit: number;
  offset: number;
  /** True when the page returned `limit` rows — there is *probably* at
   *  least one more page. Drives the viewer's infinite-scroll. */
  has_more: boolean;
  conversations: DatapackConversation[];
};

/**
 * List Langfuse sessions (= conversations) for a datapack, newest first.
 * Caller must have datapack access (owner or accepted share). Returns 503
 * if the api server isn't wired to a Langfuse Postgres.
 */
export async function listDatapackConversations(
  datapackName: string,
  params: { limit?: number; offset?: number } = {},
  options: DatapackRouteOptions = {},
) {
  const qs = new URLSearchParams();
  if (params.limit !== undefined) qs.set("limit", String(params.limit));
  if (params.offset !== undefined) qs.set("offset", String(params.offset));
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<ListDatapackConversationsResponse>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/conversations${suffix}`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

/** One user/assistant exchange inside a conversation. `reasoning` is
 *  the chain-of-thought from the matched Reasoning generation; empty
 *  when the client didn't pass one to conversation_add_message.
 *
 *  `memories_extracted` counts Memory Add tool spans in this message
 *  window (extraction attempts observed in traces), not guaranteed
 *  persisted rows in mem0. */
export type DatapackThreadMessage = {
  message_id: string;
  observation_id: string;
  started_at: string;
  input: string;
  output: string;
  reasoning?: string;
  /** Wall-clock duration of the parent `Conversation Add Message(Meko MCP)`
   *  TOOL span — the bookmark span itself is zero-duration and not
   *  useful for timing. */
  latency_ms: number;
  memories_extracted: number;
  /** mem0 row IDs attributed to this message's time window. Present only
   *  when memories_extracted > 0. Used to deep-link from the Memories tab
   *  to the specific message that extracted a given memory. */
  memory_ids?: string[];
  llm_calls: number;
};

export type DatapackConversationDetail = DatapackConversation & {
  thread: DatapackThreadMessage[];
  thread_limit: number;
  thread_offset: number;
  thread_has_more: boolean;
};

export type GetDatapackConversationResponse = {
  datapack_id: string;
  datapack_name: string;
  langfuse_project_id?: string;
  conversation: DatapackConversationDetail;
};

/**
 * Fetch one conversation header + a paginated thread of turns.
 * Returns 404 if the conversation does not belong to this datapack's
 * Langfuse project. Caller must have datapack access.
 */
export async function getDatapackConversation(
  datapackName: string,
  conversationId: string,
  params: { messagesLimit?: number; messagesOffset?: number } = {},
  options: DatapackRouteOptions = {},
) {
  const qs = new URLSearchParams();
  if (params.messagesLimit !== undefined) qs.set("messages_limit", String(params.messagesLimit));
  if (params.messagesOffset !== undefined) qs.set("messages_offset", String(params.messagesOffset));
  const base = `/datapacks/${encodeURIComponent(datapackName)}/conversations/${encodeURIComponent(conversationId)}`;
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<GetDatapackConversationResponse>(
    withDatapackID(`${base}${suffix}`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

/** One observation in the windowed span tree for a single message turn.
 *  `parent_observation_id` is empty for the root of the windowed sub-
 *  tree. `duration_ms` and `offset_ms` are derived from start_time /
 *  end_time relative to the message bookmark. */
export type DatapackTraceSpan = {
  id: string;
  parent_observation_id?: string;
  name: string;
  type: string;
  duration_ms: number;
  offset_ms: number;
  model?: string;
  /** Raw JSONB string from langfuse usage column (token counts,
   *  prompt/completion split, etc.). UI parses on demand. */
  usage?: string;
  total_cost?: string;
  /** Raw output blob, populated only for memory-related spans.
   *  May be a JSON string; UI parses and surfaces the memory text. */
  output?: string;
};

export type GetDatapackMessageTraceResponse = {
  datapack_id: string;
  datapack_name: string;
  langfuse_project_id?: string;
  conversation_id: string;
  message_id: string;
  spans: DatapackTraceSpan[];
};

/**
 * Fetch the per-message span tree (all observations between this
 * message's bookmark and the next message's bookmark). Caller must
 * have datapack access.
 */
export async function getDatapackMessageTrace(
  datapackName: string,
  conversationId: string,
  messageId: string,
  options: DatapackRouteOptions = {},
) {
  return request<GetDatapackMessageTraceResponse>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/conversations/${encodeURIComponent(
        conversationId,
      )}/messages/${encodeURIComponent(messageId)}/trace`,
      options.datapackId,
    ),
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
 * datapack. Caller must have datapack access (owner or accepted share).
 * Returns 503 if the api server isn't wired to a Langfuse Postgres.
 */
export async function getDatapackConversationCount(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<DatapackConversationCount>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/conversations/count`, options.datapackId),
    "GET",
    undefined,
    options,
  );
}

export async function ensureLangfuseProjectKeys(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<LangfuseProjectKeys>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/langfuse/project-keys`,
      options.datapackId,
    ),
    "POST",
    undefined,
    options,
  );
}

/**
 * Per-datapack provisioning details (ysql conn string + username +
 * password, plus the account-level MCP URL/key for convenience).
 * Backed by GET /datapacks/:name/provision-status; the API allows the
 * datapack owner or any user with shared access (accepted invite + grant).
 */
export async function getDatapackProvisionStatus(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<ProvisionStatus>(
    withDatapackID(`/datapacks/${encodeURIComponent(datapackName)}/provision-status`, options.datapackId),
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
  options: DatapackRouteOptions = {},
) {
  return request<{ url: string; key: string }>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/upload-url`,
      options.datapackId,
    ),
    "POST",
    { filename, content_type: contentType, size },
    options,
  );
}

export async function confirmUpload(
  datapackName: string,
  key: string,
  filename: string,
  size: number,
  options: DatapackRouteOptions = {},
) {
  return request<{ message: string; key: string }>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/upload-complete`,
      options.datapackId,
    ),
    "POST",
    { key, filename, size },
    options,
  );
}

export async function createKnowledgeBase(
  datapackName: string,
  options: DatapackRouteOptions = {},
) {
  return request<{
    source_id: string;
    index_name: string;
    source_uri: string;
    status: string;
  }>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/create`,
      options.datapackId,
    ),
    "POST",
    undefined,
    options,
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

export async function getKBStatus(datapackName: string, options: DatapackRouteOptions = {}) {
  return request<KBPipelineStatus>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/status`,
      options.datapackId,
    ),
    "GET",
    undefined,
    options,
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
  options: DatapackRouteOptions = {},
): Promise<KnowledgeBaseDocument[]> {
  // if (useMockKnowledgeBaseDocuments()) {
  //   return Promise.resolve([...MOCK_KB_DOCUMENTS_SAMPLE]);
  // }
  return request<KnowledgeBaseDocument[]>(
    withDatapackID(
      `/datapacks/${encodeURIComponent(datapackName)}/knowledge-bases/documents`,
      options.datapackId,
    ),
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
  conversation_id?: string;
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
  langfuse_project_id?: string;
};

const MEMORIES_MOCK_ON =
  import.meta.env.VITE_MOCK_MEMORIES === "true" || import.meta.env.VITE_MOCK_MEMORIES === "1";
const MEMORIES_MOCK_OFF = import.meta.env.VITE_MOCK_MEMORIES === "false";

function useMockMemoriesList(): boolean {
  if (MEMORIES_MOCK_OFF) return false;
  if (MEMORIES_MOCK_ON) return true;
  return import.meta.env.DEV;
}

/**
 * GET /memories. In dev, returns {@link buildMockMemoryListResponse} unless `VITE_MOCK_MEMORIES=false`;
 * set `VITE_MOCK_MEMORIES=true` in any mode to force mock; omit both to use the real API in production.
 */
export async function listMemories(
  params: MemoryListFilter,
  options: { signal?: AbortSignal } = {},
): Promise<MemoryListResponse> {
  const qs = new URLSearchParams();
  qs.set("datapack_id", params.datapack_id);
  qs.set("user_id", params.user_id);
  qs.set("user_type", params.user_type ?? MEMORY_LIST_DEFAULT_USER_TYPE);
  qs.set("page", String(params.page));
  qs.set("page_size", String(params.page_size));
  return request<MemoryListResponse>(`/memories?${qs.toString()}`, "GET", undefined, options);
}

/**
 * Body for POST /api/v1/promote-memories.
 *
 * Identifier semantics:
 * - `user_id`:     Meko `account_id` of the logged-in user (same semantics
 *                  as `MemoryListFilter.user_id`). The backend both filters
 *                  the mem0 fetch on `payload->>'user_id' = user_id` and
 *                  writes this value into `knowledge_base.user_id`.
 * - `datapack_id`: Datapack UUID; routes the promotion to the correct tenant DB
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
export type PromoteMemoriesRequest = {
  user_id: string;
  datapack_id: string;
  agent_id?: string;
  memory_ids: string[];
};

export type PromoteMemoriesResult = {
  inserted_ids: string[];
  updated_ids: string[];
  not_found_ids: string[];
};

export async function promoteMemories(payload: PromoteMemoriesRequest) {
  return request<PromoteMemoriesResult>("/promote-memories", "POST", payload);
}

// --- Feedback (POST /feedback) ---

/** Allowed values for `category` — must match `meko_system.feedback_category`. */
export const FEEDBACK_CATEGORIES = ["bug", "confusion", "feature_request", "other"] as const;
export type FeedbackCategory = (typeof FEEDBACK_CATEGORIES)[number];

/** Allowed values for `source` — must match `meko_system.feedback_source`. */
export type FeedbackSource = "web_app" | "docs" | "discord" | "email" | "cli" | "mcp_server";

/**
 * Body for POST /api/v1/feedback. Mirrors `models.CreateFeedbackRequest`.
 *
 * - `user_id` (required) — must match the authenticated account.
 * - `feedback_text` (required) — non-empty after trim.
 * - `category` / `source` (required) — must be a recognized enum value.
 * - `created_at` — server falls back to NOW() if omitted; we send the client time as ISO 8601.
 * - `datapack_id` / `datapack_name` — only set when feedback is filed from inside a datapack page.
 * - `user_path` — current pathname; only sent when the user opted in to share session context.
 * - `source_identifier` — typically the signed-in email; the BE auto-fills it from account.email if blank.
 */
export type CreateFeedbackRequest = {
  user_id: string;
  feedback_text: string;
  category: FeedbackCategory;
  source: FeedbackSource;
  created_at?: string | null;
  datapack_id?: string | null;
  datapack_name?: string | null;
  user_path?: string | null;
  source_identifier?: string | null;
  meko_version?: string | null;
};

export type CreateFeedbackResponse = {
  feedback_id: string;
  status: string;
  message: string;
};

export async function submitFeedback(payload: CreateFeedbackRequest) {
  return request<CreateFeedbackResponse>("/feedback", "POST", payload);
}
// --- Promoted memories list (Collective Memory tab + summary route) ---

/** One row from GET /promoted-memories. Mirrors `models.PromotedMemory`. */
export type PromotedMemory = {
  id: string;
  conversation_id?: string | null;
  agent_id?: string;
  data: string;
  promoted_to_kb_at?: string | null;
};

/** Query params for GET /promoted-memories. Mirrors `models.PromotedMemoryListFilter`. */
export type PromotedMemoryListFilter = {
  datapack_id: string;
  page: number;
  page_size: number;
};

/** Response body for GET /promoted-memories. Mirrors `models.PromotedMemoryListResponse`. */
export type PromotedMemoryListResponse = {
  criteria: PromotedMemoryListFilter;
  promoted_memories: PromotedMemory[];
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
};

/**
 * GET /promoted-memories — paginated list of memories already promoted into the
 * tenant knowledge base for a datapack.
 */
export async function listPromotedMemories(
  params: PromotedMemoryListFilter,
  options: { signal?: AbortSignal } = {},
): Promise<PromotedMemoryListResponse> {
  const qs = new URLSearchParams();
  qs.set("datapack_id", params.datapack_id);
  qs.set("page", String(params.page));
  qs.set("page_size", String(params.page_size));
  return request<PromotedMemoryListResponse>(
    `/promoted-memories?${qs.toString()}`,
    "GET",
    undefined,
    options,
  );
}

