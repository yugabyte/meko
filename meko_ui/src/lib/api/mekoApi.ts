import axios from "axios";

import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import type { KbHit, MemoryHit, ProcessingStep } from "../inferenceGateway";
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
 * Returned from POST /auth/login when an admin-created user's temporary password
 * has expired. The server has already flipped the account to CONFIRMED, so the
 * client should direct the user to the (now-working) Forgot Password flow.
 */
export const API_ERROR_CODE_INVITE_PASSWORD_EXPIRED = "INVITE_PASSWORD_EXPIRED" as const;

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
  /** mem0 rows still awaiting a decision (meko_promotion_user_action IS NULL). */
  learnings_count?: number;
  /** Memories already promoted into the shared knowledge base. */
  collective_memory_count?: number;
  datapack_owner_name?: string;
  /** Owner account email from `users.user_email` (list + describe). */
  datapack_owner_email_id?: string;
  /**
   * From `GET /datapacks`: `"owner"` for datapacks you created; invite grant (`viewer` | `contributor` | `maintainer`)
   * for datapacks shared with you.
   */
  grant?: DatapackShareGrant | null;
};

/** Linked agents from `GET /datapacks/:datapack_id/agents` (matches `models.Agent` JSON). */
export type Agent = {
  agent_id: string;
  user_id?: string;
  agent_details?: unknown;
  created_at?: string;
  updated_at?: string;
};

/** Knowledge sources from `GET /datapacks/:datapack_id/knowledge-bases` (matches `models.KnowledgeBase` JSON). */
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
  /**
   * Whether the account's email is verified. False for a federated (LinkedIn)
   * signup whose IdP sent no email — those users carry a synthesized
   * placeholder and must supply + OTP-verify a real address before using the
   * app (see CompleteAccountModal, gated on the onboarding page). Optional so
   * older BE deploys that don't emit it are treated as verified (no gate).
   */
  email_verified?: boolean;
  has_completed_onboarding: boolean;
  /**
   * Billing tier from `meko_system.users.user_type`. Optional because the
   * BE may not return it yet on older deploys — UI falls back to "free".
   * Values mirror the planned PlanId enum: free | starter | growth | scale
   * | enterprise. Today's BE only emits free | pro | enterprise; mapping
   * lives in `features/billing/hooks/useCurrentPlan`.
   */
  user_type?: string;
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
  deploy_env: "prod" | "dev";
};

const API_BASE = (import.meta.env.VITE_MEKO_API_BASE_URL || "/api/v1").replace(/\/$/, "");
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

// Exported so per-feature API modules under src/lib/api/* can reuse the
// shared envelope unwrap + auth-expired handling without duplicating it.
export async function request<T>(
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
    if (path === "/auth/login" || path === "/auth/me" || path === "/auth/complete-new-password") {
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
export function getApiErrorMessage(
  err: unknown,
  fallback = "Something went wrong. Please try again.",
): string {
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

type DatapackRouteOptions = { signal?: AbortSignal };

function datapackRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}`;
}

/** GET /datapacks/:datapack_id — full datapack row (includes `description` when set). */
export async function getDatapack(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<Datapack>(datapackRoute(datapackId), "GET", undefined, options);
}

export async function listAgents(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<Agent[]>(`${datapackRoute(datapackId)}/agents`, "GET", undefined, options);
}

export async function listKnowledgeBases(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<KnowledgeBase[]>(
    `${datapackRoute(datapackId)}/knowledge-bases`,
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
 * Body for PATCH /datapacks/:datapack_id — matches `api_server/internal/service.UpdateRequest` JSON tags.
 * Only send fields you want to change; omitted keys are left unchanged on the server.
 */
export type UpdateDatapackPayload = {
  name?: string;
  description?: string;
  datapack_ysql_connection_string?: string;
};

/** `data` from PATCH /datapacks/:datapack_id — matches handler `ok(..., map[string]string{message})`. */
export type UpdateDatapackResponse = {
  message: string;
};

/** `data` from DELETE /datapacks/:datapack_id. */
export type DeleteDatapackResponse = {
  message: string;
};

export async function updateDatapack(
  datapackId: string,
  payload: UpdateDatapackPayload,
  options: DatapackRouteOptions = {},
) {
  return request<UpdateDatapackResponse>(datapackRoute(datapackId), "PATCH", payload, options);
}

export async function deleteDatapack(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<DeleteDatapackResponse>(datapackRoute(datapackId), "DELETE", undefined, options);
}

// --- Datapack share / invites (paths are under `API_BASE`, typically `/api/v1`) ---

export type { DatapackShareGrant };
export {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
  DATAPACK_SHARE_GRANTS,
  isDatapackShareGrant,
} from "@/lib/datapackShareGrants";

/** Recipient action on a pending invite. */
export type DatapackInviteUserAction = "accept" | "decline";

/** Body for POST /datapacks/:datapack_id/share */
export type InviteDatapackSharePayload = {
  recipient_email: string;
  grant: DatapackShareGrant;
};

/** `data` from POST /datapacks/:datapack_id/share */
export type InviteDatapackShareResponse = {
  status: string;
  invite_id: string;
  recipient_email: string;
  grant: DatapackShareGrant;
};

/**
 * Invite someone to a datapack.
 * POST /datapacks/:datapack_id/share
 */
export async function inviteDatapackShare(
  datapackId: string,
  payload: InviteDatapackSharePayload,
  options: DatapackRouteOptions = {},
) {
  return request<InviteDatapackShareResponse>(
    `${datapackRoute(datapackId)}/share`,
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
  /** Email of the datapack owner who sent the invite. */
  owner_email: string;
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

/** `data` from GET /datapacks/:datapack_id/outgoing-shares (pending invites only; owner or maintainer). */
export type DatapackOutgoingSharesResponse = {
  pending_invites: DatapackOutgoingPendingInvite[];
};

/** `data` from GET /datapacks/:datapack_id/member-list (accepted members; any datapack access). */
export type DatapackMembersResponse = {
  members: DatapackOutgoingMember[];
};

/**
 * List pending outgoing invites for a datapack.
 * Owner or maintainer only (403 for viewer/contributor).
 * GET /datapacks/:datapack_id/outgoing-shares
 */
export async function listDatapackOutgoingShares(
  datapackId: string,
  options: DatapackRouteOptions = {},
) {
  return request<DatapackOutgoingSharesResponse>(
    `${datapackRoute(datapackId)}/outgoing-shares`,
    "GET",
    undefined,
    options,
  );
}

/**
 * List accepted members for a datapack (including owner row).
 * Any grant with datapack access. GET /datapacks/:datapack_id/member-list
 */
export async function listDatapackMembers(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<DatapackMembersResponse>(
    `${datapackRoute(datapackId)}/member-list`,
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
 * POST /datapacks/:datapack_id/invite/:inviteId/resend
 */
export async function resendDatapackInvite(
  datapackId: string,
  inviteId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    `${datapackRoute(datapackId)}/invite/${encodeURIComponent(inviteId)}/resend`,
    "POST",
    undefined,
    options,
  );
}

/**
 * Revoke a pending invite before acceptance (owner).
 * DELETE /datapacks/:datapack_id/invite/:inviteId
 */
export async function revokeDatapackInvite(
  datapackId: string,
  inviteId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    `${datapackRoute(datapackId)}/invite/${encodeURIComponent(inviteId)}`,
    "DELETE",
    undefined,
    options,
  );
}

/** Body for PATCH /datapacks/:datapack_id/share/:shareUserId */
export type UpdateDatapackMemberGrantPayload = {
  grant: DatapackShareGrant;
};

/**
 * TODO: This is a stretch goal for the MVP.
 * Update an active member’s grant (owner).
 * PATCH /datapacks/:datapack_id/share/:shareUserId
 */
export async function updateDatapackMemberGrant(
  datapackId: string,
  shareUserId: string,
  payload: UpdateDatapackMemberGrantPayload,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    `${datapackRoute(datapackId)}/share/${encodeURIComponent(shareUserId)}`,
    "PATCH",
    payload,
    options,
  );
}

/**
 * TODO: This is a stretch goal for the MVP.
 * Remove a member’s access / delete active share (owner).
 * DELETE /datapacks/:datapack_id/share/:shareUserId
 */
export async function removeDatapackMemberShare(
  datapackId: string,
  shareUserId: string,
  options: DatapackRouteOptions = {},
) {
  return request<unknown>(
    `${datapackRoute(datapackId)}/share/${encodeURIComponent(shareUserId)}`,
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

/**
 * Absolute URL of the browser-driven federated sign-out (GET /auth/logout).
 * The browser must *navigate* here (not fetch it): the endpoint revokes the
 * Cognito tokens, clears our auth cookies, then 302s through Cognito's
 * /logout to drop the hosted session cookie so the next login re-federates
 * through the IdP (Google's account chooser instead of a silent re-login).
 */
export function federatedLogoutUrl() {
  return `${API_BASE}/auth/logout`;
}

export async function getCurrentUser(options: { signal?: AbortSignal } = {}) {
  return request<CurrentUser>("/auth/me", "GET", undefined, options);
}

/**
 * Per-account usage counters used by the free-tier gating. Numbers only
 * — the limit thresholds live FE-side in `freeTier.ts` so we can tune
 * them without redeploying the BE.
 *
 * `num_conversations` and `token_burn` are scoped to WorkbenchLM
 * specifically (`agent_id = "WorkbenchLM"`); third-party agents the
 * user has connected over MCP are NOT included.
 */
export type AccountUsage = {
  datapack_count: number;
  kb_storage_file_size: number;
  num_conversations: number;
  token_burn: number;
};

export async function getCurrentUserUsage(options: { signal?: AbortSignal } = {}) {
  return request<AccountUsage>("/auth/me/usage", "GET", undefined, options);
}

export async function updateOnboardingStatus(hasCompletedOnboarding: boolean) {
  return request<{ message: string; has_completed_onboarding: boolean }>("/auth/me", "PATCH", {
    has_completed_onboarding: hasCompletedOnboarding,
  });
}

// Update the authenticated user's first/last name. Used by the one-time
// name-confirmation step shown after a first Google signup (the IdP's name
// data may be incomplete).
export async function updateProfile(firstName: string, lastName: string) {
  return request<{ message: string }>("/auth/me/profile", "PATCH", {
    first_name: firstName,
    last_name: lastName,
  });
}

// --- Post-signup email collection/verification (federated, no-email signups) ---
//
// For a LinkedIn signup whose IdP sent no email, the account carries a
// synthesized placeholder and `email_verified` is false. These three calls
// drive the blocking onboarding modal that collects + OTP-verifies a real
// address, writing it to both Cognito and the local DB.

// isPlaceholderEmail reports whether an address is the synthesized, non-real
// placeholder the inbound-federation Lambda mints for a federated signup whose
// IdP sent no email. Shape: `<local>@no-reply.<provider>.meko.invalid`. Mirrors
// the backend detector (service.isPlaceholderEmail: "no-reply." host prefix +
// ".meko.invalid" suffix). Used to avoid surfacing the placeholder in the UI —
// we show a friendlier label until the user verifies a real address.
export function isPlaceholderEmail(email: string | null | undefined): boolean {
  if (!email) return false;
  // Trim to match the backend's isPlaceholderEmail (which TrimSpace-es first),
  // so stray whitespace can't cause a client-side false negative.
  const trimmed = email.trim();
  const at = trimmed.lastIndexOf("@");
  if (at === -1) return false;
  const host = trimmed.slice(at + 1).toLowerCase();
  return host.startsWith("no-reply.") && host.endsWith(".meko.invalid");
}

// Set the real email; the BE writes it to Cognito, which emails a 6-digit OTP
// to that address. Throws MekoApiRequestError; notable codes:
//   - 400 VALIDATION_ERROR — malformed address
//   - 409 EMAIL_TAKEN      — the address already belongs to another account
export async function startEmailChange(email: string) {
  return request<{ message: string }>("/auth/me/email/start", "POST", {
    email: email.trim(),
  });
}

// Re-send the OTP to the pending email.
export async function resendEmailChangeCode() {
  return request<{ message: string }>("/auth/me/email/resend", "POST");
}

// Confirm the OTP; on success email_verified flips to true in Cognito + DB,
// clearing the gate. Throws ApiError (400 INVALID_CODE / CODE_EXPIRED).
export async function verifyEmailChange(code: string) {
  return request<{ message: string }>("/auth/me/email/verify", "POST", {
    code: code.trim(),
  });
}
// --- Meko personal access tokens (MCP PATs) ---
//
// Self-service tokens minted from Settings → Security. Used in place of
// the legacy datapack-scoped mcp_api_key when connecting MCP clients.
// Plaintext is returned exactly once from POST /me/meko-tokens; thereafter
// only the `token_prefix` (first ~16 chars) is visible to the user.

/** One row of `meko_system.mcp_personal_access_tokens` as returned by the
 *  `GET /me/meko-tokens` list endpoint. Mirrors `models.MCPPersonalAccessToken`.
 *  The full plaintext token is NEVER part of this shape — see {@link CreateMekoTokenResponse}. */
export type MekoToken = {
  id: string;
  name: string;
  token_prefix: string;
  scopes: string[];
  created_at: string;
  expires_at: string;
  last_used_at?: string | null;
  revoked_at?: string | null;
};

/** Body for `POST /me/meko-tokens`. `ttl_days` and `scopes` are optional —
 *  the server applies defaults when omitted. */
export type CreateMekoTokenPayload = {
  name: string;
  ttl_days?: number;
  scopes?: string[];
};

/** Response from `POST /me/meko-tokens`. `token` is the plaintext value
 *  returned exactly once; persist it client-side immediately or it's lost. */
export type CreateMekoTokenResponse = {
  token: string;
  token_record: MekoToken;
};

export async function listMekoTokens(options: { signal?: AbortSignal } = {}) {
  return request<MekoToken[]>("/me/meko-tokens", "GET", undefined, options);
}

export async function createMekoToken(
  payload: CreateMekoTokenPayload,
  options: { signal?: AbortSignal } = {},
) {
  return request<CreateMekoTokenResponse>("/me/meko-tokens", "POST", payload, options);
}

export async function revokeMekoToken(tokenId: string, options: { signal?: AbortSignal } = {}) {
  // DELETE returns 204 No Content. The request<T> helper expects an
  // envelope; the underlying handler treats null bodies as `data: null`
  // so we just claim void here.
  return request<void>(
    `/me/meko-tokens/${encodeURIComponent(tokenId)}`,
    "DELETE",
    undefined,
    options,
  );
}

/** Payload for the signed-in change-password flow. Mirrors Cognito's
 *  `ChangePassword` API (PreviousPassword + ProposedPassword) but takes
 *  a `confirm_password` so the BE can echo the same validation we run
 *  in the UI. */
export type ChangePasswordPayload = {
  old_password: string;
  new_password: string;
  confirm_password: string;
};

/**
 * POST /auth/change-password — Cognito ChangePassword on the current
 * session. Distinct from the unauthenticated forgot-password flow
 * (which goes through email + code). Requires the user to be signed in;
 * the BE uses the session cookies to identify the account.
 *
 * NOTE: Backend handler is not yet implemented at the time of writing —
 * the UI will surface the standard 404/501 message until the Go handler
 * lands. Once the handler exists this client function works as-is.
 */
export async function changePassword(payload: ChangePasswordPayload) {
  return request<{ message: string }>("/auth/change-password", "POST", {
    old_password: payload.old_password,
    new_password: payload.new_password,
    confirm_password: payload.confirm_password,
  });
}

export async function bootstrapObserveSession() {
  return request<{ message: string; org_id?: string }>("/observe/session/bootstrap", "POST");
}

export type DatapackConversation = {
  id: string;
  agent_id?: string;
  /** account_id of the user who owns this conversation. Lets the
   *  WorkbenchLM "previous chats" dropdown scope to the signed-in user's
   *  own sessions (the datapack may be shared with several members). */
  user_id?: string;
  /** Email of that user — display convenience alongside `user_id`. */
  user_email_id?: string;
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
  datapackId: string,
  params: { limit?: number; offset?: number; q?: string } = {},
  options: DatapackRouteOptions = {},
) {
  const qs = new URLSearchParams();
  if (params.limit !== undefined) qs.set("limit", String(params.limit));
  if (params.offset !== undefined) qs.set("offset", String(params.offset));
  if (params.q) qs.set("q", params.q);
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<ListDatapackConversationsResponse>(
    `${datapackRoute(datapackId)}/conversations${suffix}`,
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
  /** Surviving mem0 memory texts for this message window, in the same
   *  order as `memory_ids`. Excludes promoted/evicted rows, so it stays
   *  in lockstep with `memories_extracted` — used by the hover card. */
  memory_texts?: string[];
  /** Memories extracted on this turn that were since promoted to the KB
   *  (now in promoted_collection, evicted from mem0). Kept out of
   *  `memories_extracted`/`memory_ids`/`memory_texts`; surfaced separately
   *  tagged "Promoted to KB" and matched for Collective-Memory deep-links. */
  promoted_memory_ids?: string[];
  promoted_memory_texts?: string[];
  llm_calls: number;
  /** Sum of `usage_details.total` across every GENERATION span in this
   *  turn's window. Always present; 0 when no usage was reported. */
  total_tokens: number;
  /** "conversation_cache" when this turn was answered from a prior
   *  conversation (LLM skipped); empty otherwise. */
  answered_from?: string;
  /** LLM identifier sourced from Langfuse's `provided_model_name` on the
   *  matched Reasoning generation. Empty when the agent's harness didn't
   *  supply it. */
  model?: string;
  /** Structured checklist persisted on the Reasoning generation's
   *  `metadata.plan`. Empty / undefined for legacy rows where the plan
   *  was embedded in the reasoning prose. */
  plan?: string[];
  /** Same structured shape as the Playground response. Populated from
   *  Langfuse observations so the DecisionTrace card can render retrieval
   *  + steps above historical assistant replies. */
  memory_hits?: MemoryHit[];
  kb_hits?: KbHit[];
  processing_steps?: ProcessingStep[];
};

export type DatapackConversationDetail = DatapackConversation & {
  thread: DatapackThreadMessage[];
  thread_limit: number;
  thread_offset: number;
  thread_has_more: boolean;
  /** True when this WorkbenchLM conversation is owned by the viewer and not
   *  yet in the conversation cache — drives the "Index conversation" button. */
  can_index?: boolean;
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
  datapackId: string,
  conversationId: string,
  params: { messagesLimit?: number; messagesOffset?: number } = {},
  options: DatapackRouteOptions = {},
) {
  const qs = new URLSearchParams();
  if (params.messagesLimit !== undefined) qs.set("messages_limit", String(params.messagesLimit));
  if (params.messagesOffset !== undefined) qs.set("messages_offset", String(params.messagesOffset));
  const base = `${datapackRoute(datapackId)}/conversations/${encodeURIComponent(conversationId)}`;
  const suffix = qs.toString() ? `?${qs.toString()}` : "";
  return request<GetDatapackConversationResponse>(`${base}${suffix}`, "GET", undefined, options);
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
  /** Statement executed by a DB span — SQL for `Execute Query (...)`
   *  spans, Cypher for `Execute Cypher Query (...)` spans. Bind params
   *  (embedding vectors) are stripped backend-side. Empty for non-DB
   *  spans; UI shows it in an expandable "query" disclosure. */
  input?: string;
  /** Backend-expanded memory rows for this span with exact metrics. */
  memory_items?: Array<{
    memory_id?: string;
    text: string;
    /** True when this memory was promoted to the KB and evicted from mem0.
     *  The vector/entities/relations counts are meaningless in that case
     *  (the mem0 anchor row is gone) — render text + a "Promoted to KB"
     *  tag instead of the count badges. */
    promoted?: boolean;
    vector: number;
    entities: number;
    relations: number;
    entity_names?: string[];
    relation_names?: string[];
  }>;
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
  datapackId: string,
  conversationId: string,
  messageId: string,
  options: DatapackRouteOptions = {},
) {
  return request<GetDatapackMessageTraceResponse>(
    `${datapackRoute(datapackId)}/conversations/${encodeURIComponent(
      conversationId,
    )}/messages/${encodeURIComponent(messageId)}/trace`,
    "GET",
    undefined,
    options,
  );
}

/** Slim per-message decision-trace payload — the exact subset of fields
 *  the `DecisionTrace` card reads (reasoning / plan / retrieval hits /
 *  processing steps + per-turn telemetry). Mirrors the BE
 *  `MessageDecisionTrace` struct; the server projects it out of one
 *  message's window in the Langfuse thread, so no whole-thread payload
 *  is transferred. */
export type DatapackMessageDecisionTrace = {
  message_id: string;
  model?: string;
  latency_ms: number;
  total_tokens: number;
  llm_calls: number;
  memories_extracted: number;
  reasoning?: string;
  plan?: string[];
  memory_hits?: MemoryHit[];
  kb_hits?: KbHit[];
  processing_steps?: ProcessingStep[];
};

export type GetDatapackMessageDecisionTraceResponse = {
  datapack_id: string;
  datapack_name: string;
  langfuse_project_id?: string;
  conversation_id: string;
  decision_trace: DatapackMessageDecisionTrace;
};

/**
 * Fetch the decision-trace for a SINGLE message by message_id (the same
 * id `POST /chat` returns). Used by the WorkbenchLM live chat to lazily
 * populate a turn's "Decision trace" block when the user expands it —
 * by which point Langfuse has typically finished indexing the turn.
 * Caller must have datapack access.
 */
export async function getDatapackMessageDecisionTrace(
  datapackId: string,
  conversationId: string,
  messageId: string,
  options: DatapackRouteOptions = {},
) {
  return request<GetDatapackMessageDecisionTraceResponse>(
    `${datapackRoute(datapackId)}/conversations/${encodeURIComponent(
      conversationId,
    )}/messages/${encodeURIComponent(messageId)}/decision-trace`,
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
  datapackId: string,
  options: DatapackRouteOptions = {},
) {
  return request<DatapackConversationCount>(
    `${datapackRoute(datapackId)}/conversations/count`,
    "GET",
    undefined,
    options,
  );
}

export async function ensureLangfuseProjectKeys(
  datapackId: string,
  options: DatapackRouteOptions = {},
) {
  return request<LangfuseProjectKeys>(
    `${datapackRoute(datapackId)}/langfuse/project-keys`,
    "POST",
    undefined,
    options,
  );
}

/**
 * Per-datapack provisioning details (ysql conn string + username +
 * password, plus the account-level MCP URL/key for convenience).
 * Backed by GET /datapacks/:datapack_id/provision-status; the API allows the
 * datapack owner or any user with shared access (accepted invite + grant).
 */
export async function getDatapackProvisionStatus(
  datapackId: string,
  options: DatapackRouteOptions = {},
) {
  return request<ProvisionStatus>(
    `${datapackRoute(datapackId)}/provision-status`,
    "GET",
    undefined,
    options,
  );
}

// --- Knowledge Base file upload & RAG APIs ---

export { sha256Hex, canUseWebCryptoSha256, isNonSecureHttpOrigin } from "../sha256File";

export async function requestUploadURL(
  datapackId: string,
  filename: string,
  contentType: string,
  size: number,
  contentHash: string,
  options: DatapackRouteOptions = {},
) {
  return request<{
    url: string;
    key: string;
    already_uploaded: boolean;
    existing_filename?: string;
  }>(
    `${datapackRoute(datapackId)}/knowledge-bases/upload-url`,
    "POST",
    { filename, content_type: contentType, size, content_hash: contentHash },
    options,
  );
}

export async function confirmUpload(
  datapackId: string,
  key: string,
  filename: string,
  size: number,
  options: DatapackRouteOptions = {},
) {
  return request<{ message: string; key: string }>(
    `${datapackRoute(datapackId)}/knowledge-bases/upload-complete`,
    "POST",
    { key, filename, size },
    options,
  );
}

export async function createKnowledgeBase(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<{
    source_id: string;
    index_name: string;
    source_uri: string;
    status: string;
  }>(`${datapackRoute(datapackId)}/knowledge-bases/create`, "POST", undefined, options);
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

export async function getKBStatus(datapackId: string, options: DatapackRouteOptions = {}) {
  return request<KBPipelineStatus>(
    `${datapackRoute(datapackId)}/knowledge-bases/status`,
    "GET",
    undefined,
    options,
  );
}

/** One row from GET /datapacks/:datapack_id/knowledge-bases/documents.
 *
 *  The repo's SQL deduplicates per `document_uri` and joins
 *  `dist_rag.vector_index_pipeline_details` for `chunks_processed` +
 *  the most recent `document_status`. `uploaded_at` is the S3
 *  LastModified timestamp stored in `document_details->meko->uploaded_at`
 *  and is shown as "Uploaded" in the files table. `completed_at` is the
 *  RAG pipeline completion time.
 *
 *  Embedding model + dimensions are deliberately NOT on this row —
 *  they're properties of the vector index, and Meko has one global
 *  index today so the value is the same across every file. The UI
 *  reads them from {@link KnowledgeBaseSummary} once and passes
 *  them down to per-row detail views as a prop.
 *
 *  All enrichment fields are nullable: a doc that hasn't finished
 *  embedding may have no pipeline row yet (chunks/status nil), and
 *  legacy rows pre-dating the schema may lack a checksum or type. */
export type KnowledgeBaseDocument = {
  id: string;
  document_name: string;
  document_uri: string;
  document_type?: string | null;
  document_checksum?: string | null;
  chunks_processed?: number | null;
  completed_at?: string | null;
  /** S3 LastModified — the actual moment the file was written to the
   *  bucket. Null for legacy documents uploaded before this field was
   *  added; the UI falls back to `completed_at` in that case. */
  uploaded_at?: string | null;
  status: string;
  /** Per-file provenance from `dist_rag.documents.document_details->meko`.
   *  - `uploaded_by_user_id`: authenticated uploader at CreateKB time.
   *  - `uploaded_by_email`:   resolved server-side via JOIN on
   *                            meko_system.users; null when the user
   *                            row has been deleted or the file is legacy.
   *  - `size_bytes`:          raw byte size from S3 Contents.Size. */
  uploaded_by_user_id?: string | null;
  uploaded_by_email?: string | null;
  size_bytes?: number | null;
};

/** Paginated envelope returned by GET .../knowledge-bases/documents. */
export type KnowledgeBaseDocumentsPage = {
  items: KnowledgeBaseDocument[];
  total: number;
  page: number;
  page_size: number;
  has_more: boolean;
};

/**
 * GET /datapacks/:datapack_id/knowledge-bases/documents — paginated.
 *
 * @param page       1-based page number (default 1)
 * @param page_size  items per page (default 50, max 200)
 * @param q          filename substring search
 * @param type_filter file-extension suffix, e.g. ".md"
 * @param indexed_only restrict to fully-indexed (COMPLETED) documents
 */
export async function getKnowledgeBaseDocuments(
  datapackId: string,
  options: DatapackRouteOptions & {
    page?: number;
    page_size?: number;
    q?: string;
    type_filter?: string;
    indexed_only?: boolean;
  } = {},
): Promise<KnowledgeBaseDocumentsPage> {
  const { page, page_size, q, type_filter, indexed_only, ...rest } = options;
  const qs = new URLSearchParams();
  if (page != null) qs.set("page", String(page));
  if (page_size != null) qs.set("page_size", String(page_size));
  if (q) qs.set("q", q);
  if (type_filter) qs.set("type_filter", type_filter);
  if (indexed_only) qs.set("indexed_only", "true");
  const suffix = qs.size > 0 ? `?${qs.toString()}` : "";
  const resp = await request<KnowledgeBaseDocumentsPage>(
    `${datapackRoute(datapackId)}/knowledge-bases/documents${suffix}`,
    "GET",
    undefined,
    rest,
  );
  // Defensive: a BE that ships `items: null` for an empty page would
  // crash callers that .map / .slice the field.
  return { ...resp, items: resp?.items ?? [] };
}

/** Aggregate stats + auto-generated text for the SharedKnowledge header
 *  tiles. One round trip — the BE pre-computes `description` and
 *  `files_summary` from the same numbers it exposes here so the UI never
 *  has to compose copy itself.
 *
 *  All embedding-model fields are nullable: the vector index may not
 *  exist yet for a fresh datapack (KB not created). Heaviest-source
 *  fields are nullable when no document has a known chunk count. */
export type KnowledgeBaseSummary = {
  total_files: number;
  total_chunks: number;
  last_file_uploaded_at?: string | null;
  heaviest_source_name?: string | null;
  heaviest_source_chunks?: number | null;
  embedding_model?: string | null;
  embedding_dimensions?: number | null;
  /** Upstream provider for the global vector index
   *  (`dist_rag.vector_indexes.ai_provider`, e.g. "OPENAI"). Nullable
   *  when no vector index row exists. Single source of truth for the
   *  provider — /promoted-memories etc. do NOT carry duplicates. */
  ai_provider?: string | null;
  /** Chunking strategy size — UI renders as "semantic·{chunk_size}" in
   *  the file-row expand panel. Nullable when no vector index exists. */
  chunk_size?: number | null;
  promoted_memories_count: number;
  description: string;
  files_summary: string;
};

export async function getKnowledgeBaseSummary(
  datapackId: string,
  options: DatapackRouteOptions = {},
) {
  return request<KnowledgeBaseSummary>(
    `${datapackRoute(datapackId)}/knowledge-bases/summary`,
    "GET",
    undefined,
    options,
  );
}

/** One entry from GET /knowledge-bases/pending-uploads — a file that
 *  was PUT to S3 via the presigned-URL flow but never registered with
 *  dist_rag (no matching document row exists in any status). Surfaced
 *  in the upload dialog so the user can resume with a single
 *  Create/Add-to-Knowledge-Base click instead of re-picking the file. */
export type PendingUpload = {
  /** Full S3 key, e.g. "<user_id>/<datapack_id>/<sha>/<filename>". */
  key: string;
  filename: string;
  /** S3 object size in bytes; 0 = unknown. */
  size: number;
  /** S3 LastModified, RFC3339 UTC. Empty string on legacy objects
   *  where S3 omitted the timestamp. */
  uploaded_at?: string;
};

export type PendingUploadsResponse = {
  items: PendingUpload[];
  count: number;
};

/** DELETE /datapacks/:datapack_id/knowledge-bases/pending-uploads?key=… —
 *  removes a single S3 orphan. Just an S3 DeleteObject on the BE — no
 *  dist_rag row / chunks exist for orphans by definition. */
export async function deletePendingUpload(
  datapackId: string,
  key: string,
  options: DatapackRouteOptions = {},
) {
  return request<{ key: string }>(
    `${datapackRoute(datapackId)}/knowledge-bases/pending-uploads?key=${encodeURIComponent(key)}`,
    "DELETE",
    undefined,
    options,
  );
}

/** GET /datapacks/:datapack_id/knowledge-bases/pending-uploads — returns
 *  S3 objects that never made it into dist_rag.documents. Owner or
 *  maintainer only (same gate as the upload path that produces them). */
export async function listPendingUploads(
  datapackId: string,
  options: DatapackRouteOptions = {},
): Promise<PendingUploadsResponse> {
  const resp = await request<PendingUploadsResponse>(
    `${datapackRoute(datapackId)}/knowledge-bases/pending-uploads`,
    "GET",
    undefined,
    options,
  );
  return { items: resp?.items ?? [], count: resp?.count ?? 0 };
}


/** Discriminated row for the SharedKnowledge "Activity log" feed.
 *  `type` selects which side of the BE's UNION populated this row — the
 *  fields keyed off the other side stay null/undefined. */
export type KBActivityLogItem = {
  type: "file_upload" | "memory_promotion";
  timestamp: string;
  // file_upload
  document_name?: string | null;
  document_type?: string | null;
  chunks_processed?: number | null;
  size_bytes?: number | null;
  uploaded_by_email?: string | null;
  // memory_promotion
  memory_id?: string | null;
  content?: string | null;
  agent_id?: string | null;
  conversation_id?: string | null;
};

/** Activity-log page envelope.
 *
 *  Keyset pagination: `next_cursor` is an opaque, server-generated
 *  string the client passes back verbatim on the next request to
 *  fetch the page after the last one returned. Empty string means
 *  the feed is exhausted (matches `has_more === false`). The client
 *  must never parse the cursor — its shape is owned by the server. */
export type KBActivityLogResponse = {
  items: KBActivityLogItem[];
  has_more: boolean;
  next_cursor: string;
  limit: number;
};

/** Short-lived presigned S3 GET URL response from
 *  POST /knowledge-bases/download-url. UI redirects to `url` (or opens
 *  in a new tab) right after receiving it; the link expires after
 *  `expires_in` seconds. */
export type KnowledgeBaseDownloadURL = {
  url: string;
  key: string;
  expires_in: number;
};

/** POST /knowledge-bases/download-url — exchange a `document_uri`
 *  (full `s3://...` URI from the documents list) for a presigned GET
 *  URL the browser can navigate to. Server verifies the document
 *  belongs to this datapack before signing. */
export async function requestKnowledgeBaseDownloadURL(
  datapackId: string,
  documentURI: string,
  options: DatapackRouteOptions = {},
) {
  return request<KnowledgeBaseDownloadURL>(
    `${datapackRoute(datapackId)}/knowledge-bases/download-url`,
    "POST",
    { document_uri: documentURI },
    options,
  );
}

/** Wire shape of the per-file DELETE response. `chunks_deleted` is the
 *  embedding row count actually removed; `s3_deleted` is true iff the
 *  S3 object was also removed (best-effort — an orphan S3 object is
 *  tolerated when the DB delete succeeded but S3 failed). */
export type KnowledgeBaseDocumentDeleteResult = {
  document_id: string;
  chunks_deleted: number;
  s3_deleted: boolean;
};

/** DELETE /datapacks/:datapack_id/knowledge-bases/documents/:document_id —
 *  removes a single KB file end-to-end: chunks + dist_rag rows + S3
 *  object. BE enforces permission (owner | maintainer | file uploader);
 *  callers that don't satisfy the policy get 403. */
export async function deleteKnowledgeBaseDocument(
  datapackId: string,
  documentID: string,
  options: DatapackRouteOptions = {},
) {
  return request<KnowledgeBaseDocumentDeleteResult>(
    `${datapackRoute(datapackId)}/knowledge-bases/documents/${encodeURIComponent(documentID)}`,
    "DELETE",
    undefined,
    options,
  );
}

export async function getKnowledgeBaseActivityLog(
  datapackId: string,
  params: { limit?: number; cursor?: string } = {},
  options: DatapackRouteOptions = {},
) {
  const limit = params.limit ?? 10;
  const cursor = params.cursor ?? "";
  const q = new URLSearchParams();
  q.set("limit", String(limit));
  // Only attach `cursor` when non-empty so the URL stays clean on the
  // first-page request (BE treats empty + missing identically).
  if (cursor) q.set("cursor", cursor);
  return request<KBActivityLogResponse>(
    `${datapackRoute(datapackId)}/knowledge-bases/activity-log?${q.toString()}`,
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
 *
 * `user_id` + `user_name` + `user_email` identify which account added the
 * memory. In a shared datapack the listing returns memories from every
 * member, so the UI uses these to label the author and to gate the
 * delete affordance (only memories where `user_id === current account`
 * are deletable).
 */
export type Memory = {
  id: string;
  data: string;
  agent_id?: string;
  conversation_id?: string;
  user_id?: string;
  user_name?: string;
  user_email?: string;
  created_at?: string;
};

/**
 * Render the author of a memory as a single short string. Used by both the
 * Playground panel and the datapack-detail Memory tiles so the fallback
 * chain (full name → email → short UUID → fallback) stays in one place.
 *
 * Pass `currentUserId` + `selfLabel` to special-case "you authored this"
 * views: the Playground shows "you" on your own rows while the detail
 * roster shows the actual name even on your own rows (no selfLabel).
 */
export function formatMemoryAuthor(
  memory: Pick<Memory, "user_id" | "user_name" | "user_email">,
  opts?: { currentUserId?: string; selfLabel?: string; fallback?: string },
): string {
  const fallback = opts?.fallback ?? "-";
  if (
    opts?.selfLabel &&
    opts.currentUserId &&
    memory.user_id &&
    memory.user_id === opts.currentUserId
  ) {
    return opts.selfLabel;
  }
  const name = (memory.user_name ?? "").trim();
  if (name) return name;
  if (memory.user_email) return memory.user_email;
  if (memory.user_id) return memory.user_id.slice(0, 8);
  return fallback;
}

/**
 * GET /memories query params — only these keys are sent:
 * `datapack_id`, `user_id`, `user_type`, `page`, `page_size`.
 *
 * `user_type` defaults to {@link MEMORY_LIST_DEFAULT_USER_TYPE} when omitted.
 */
export type MemoryListFilter = {
  datapack_id: string;
  user_id?: string;
  user_type?: MemoryUserType;
  page: number;
  page_size: number;
  /** Case-insensitive substring filter over the memory text. */
  q?: string;
  /** "learnings" → rows still awaiting a decision (NULL action).
   *  "memories"  (default) → NULL OR explicitly kept. */
  scope?: "learnings" | "memories";
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

/**
 * GET /memories.
 *
 * Default behaviour calls the real API.
 */
export async function listMemories(
  params: MemoryListFilter,
  options: { signal?: AbortSignal } = {},
): Promise<MemoryListResponse> {
  const qs = new URLSearchParams();
  qs.set("datapack_id", params.datapack_id);
  if (params.user_id) qs.set("user_id", params.user_id);
  qs.set("user_type", params.user_type ?? MEMORY_LIST_DEFAULT_USER_TYPE);
  qs.set("page", String(params.page));
  qs.set("page_size", String(params.page_size));
  if (params.q) qs.set("q", params.q);
  if (params.scope) qs.set("scope", params.scope);
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

/** Body for POST /api/v1/keep-as-memory. Stamps each mem0 row with
 *  `meko_promotion_user_action = 'keep_as_memory'` so it drops off the
 *  Learnings list while still appearing in Memories. One-way. */
export type KeepAsMemoryRequest = {
  datapack_id: string;
  memory_ids: string[];
};

export type KeepAsMemoryResult = {
  updated: number;
};

export async function keepAsMemory(payload: KeepAsMemoryRequest) {
  return request<KeepAsMemoryResult>("/keep-as-memory", "POST", payload);
}

/**
 * DELETE /memories/:memory_id?datapack_id=...
 *
 * The backend resolves the caller from the bearer token and only deletes
 * rows where `meko_user_id` matches that account — so the UI does not
 * pass `user_id` itself. Memories belonging to other members of a shared
 * datapack return 404 (collapsed with "doesn't exist" on purpose).
 */
export async function deleteMemory(
  memoryId: string,
  datapackId: string,
  options: { signal?: AbortSignal } = {},
): Promise<Memory> {
  const qs = new URLSearchParams();
  qs.set("datapack_id", datapackId);
  return request<Memory>(
    `/memories/${encodeURIComponent(memoryId)}?${qs.toString()}`,
    "DELETE",
    undefined,
    options,
  );
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

/** One row from GET /promoted-memories. Mirrors `models.PromotedMemory`.
 *
 *  Per-row enrichment (resolved server-side via the shared
 *  `LookupUserDisplays` on systemDB):
 *   - `promoted_by_name` / `promoted_by_email`: identity of the user
 *     who clicked "Promote". Either or both may be absent — the user
 *     row may have empty first/last name columns, or have been deleted,
 *     or the row was promoted without a tracked user_id.
 */
export type PromotedMemory = {
  id: string;
  conversation_id?: string | null;
  agent_id?: string;
  data: string;
  promoted_to_kb_at?: string | null;
  promoted_by_name?: string | null;
  promoted_by_email?: string | null;
};

/** Query params for GET /promoted-memories. Mirrors `models.PromotedMemoryListFilter`. */
export type PromotedMemoryListFilter = {
  datapack_id: string;
  page: number;
  page_size: number;
};

/** Response body for GET /promoted-memories. Mirrors `models.PromotedMemoryListResponse`.
 *
 *  Index-level constants (embedding_model / embedding_dimensions /
 *  ai_provider) deliberately do NOT live on this response — they're
 *  carried by GET /knowledge-bases/summary as the single source of
 *  truth for properties of the global vector index. Pages that need
 *  both fetch /summary explicitly. */
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
