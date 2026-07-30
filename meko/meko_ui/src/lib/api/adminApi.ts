// Admin console API calls, kept separate from the core mekoApi surface. These
// back the in-app Admin view (Data Plane, Metrics, Executive Dashboard, Users &
// tiers, AMP registration) and hit api_server's /admin/* endpoints, which are
// gated by the X-Meko-Admin-Secret header.
//
// The browser never holds that secret: it calls same-origin /api/v1/admin/*,
// and meko_ui's Express server injects the header server-side (see
// server/index.ts). Those routes are only active when SHOW_ADMIN_VIEW is on and
// ADMIN_API_KEY is set. Reuses mekoApi's `request` (envelope unwrap + error
// handling) and the standard Meko API base.

import { MekoApiRequestError, request } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

/** Append a query string (skipping undefined values) to a path. */
function withQuery(path: string, params: Record<string, string | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") q.set(k, v);
  }
  const qs = q.toString();
  return qs ? `${path}?${qs}` : path;
}

/* ---- users + tiers ---- */

export type AdminUser = {
  id: string;
  email: string;
  first_name: string;
  last_name: string;
  tier: "free" | "pro";
  created_at: string;
};

/** GET /admin/users — every user (newest first) for the scope dropdown + table. */
export async function fetchAdminUsers(options: RouteOptions = {}): Promise<AdminUser[]> {
  const res = await request<{ users: AdminUser[] }>("/admin/users", "GET", undefined, options);
  return res.users;
}

/** PATCH /admin/users/:id/tier — override a single user's tier (Free <-> Pro). */
export async function updateUserTier(
  userId: string,
  tier: "free" | "pro",
): Promise<{ id: string; tier: string }> {
  return request<{ id: string; tier: string }>(
    `/admin/users/${encodeURIComponent(userId)}/tier`,
    "PATCH",
    { tier },
  );
}

/* ---- usage metrics ---- */

export type UsageMetrics = {
  scope: string; // "all" or a user id
  total_users: number;
  free_users: number;
  pro_users: number;
  tier: "free" | "pro" | null; // per-user only
  datapacks: number;
  shared_datapacks: number;
  kb_documents: number;
  kb_chunks: number;
  private_memories: number;
  collective_memories: number;
  // Langfuse-derived; zero/empty when langfuse_enabled is false.
  conversations: number;
  conversations_by_agent: { agent: string; conversations: number }[];
  token_burn: number;
  langfuse_enabled: boolean;
};

/** GET /admin/metrics — usage metrics overall ("all") or for a single user,
 *  optionally windowed by an inclusive date range (YYYY-MM-DD). */
export async function fetchUsageMetrics(
  userId: string,
  start?: string,
  end?: string,
  options: RouteOptions = {},
): Promise<UsageMetrics> {
  const path = withQuery("/admin/metrics", { user_id: userId, start, end });
  return request<UsageMetrics>(path, "GET", undefined, options);
}

/* ---- data plane (YBA universe) ---- */

export type DataPlaneInfo = {
  configured: boolean; // false when no YBA token is set on api_server
  available: boolean; // false when YBA is configured but the universe can't be read
  universe_name: string;
  provider: string;
  software_version: string;
  nodes: number; // logical node count (userIntent.numNodes)
  live_nodes: number;
  total_nodes: number; // node/pod entries in nodeDetailsSet (for the health pill)
  replication_factor: number;
  instance_type: string; // cloud providers; empty for kubernetes
  tserver_cores: number; // per-process specs (kubernetes); 0 for cloud VMs
  tserver_memory_gib: number;
  master_cores: number;
  master_memory_gib: number;
  region: string;
  volume_size_gb: number;
  num_volumes: number;
  cpu_percent?: number;
  memory_percent?: number;
  disk_percent?: number;
};

/** GET /admin/dataplane — live YugabyteDB Anywhere universe config + best-effort utilization. */
export async function fetchDataPlane(options: RouteOptions = {}): Promise<DataPlaneInfo> {
  return request<DataPlaneInfo>("/admin/dataplane", "GET", undefined, options);
}

/* ---- executive dashboard ---- */

export type ExecUserSavings = {
  user_id: string;
  email: string;
  first_name: string;
  last_name: string;
  request_count: number;
  tokens_saved: number;
};

export type ExecutiveSummary = {
  total_users: number;
  total_datapacks: number;
  total_conversations: number;
  langfuse_enabled: boolean;
  total_requests: number;
  total_tokens_saved: number;
  top_users: ExecUserSavings[];
};

/** GET /admin/token-savings — executive rollup: headcount totals, account-wide
 *  contextual-search token savings, and the top searchers by tokens saved. */
export async function fetchExecutiveSummary(options: RouteOptions = {}): Promise<ExecutiveSummary> {
  return request<ExecutiveSummary>("/admin/token-savings", "GET", undefined, options);
}

/* ---- AMP controller registration (singleton) ---- */

export type AmpRegistration = { name: string; url: string; username?: string };

/** GET /admin/amp-registration — the current registration, or null if unregistered. */
export async function getAmpRegistration(options: RouteOptions = {}): Promise<AmpRegistration | null> {
  try {
    return await request<AmpRegistration>("/admin/amp-registration", "GET", undefined, options);
  } catch (e) {
    if (e instanceof MekoApiRequestError && e.httpStatus === 404) return null;
    throw e;
  }
}

/** POST /admin/amp-registration — register (or replace) the AMP controller endpoint. */
export async function registerAmp(payload: {
  name: string;
  url: string;
  username?: string;
  password?: string;
}): Promise<AmpRegistration> {
  return request<AmpRegistration>("/admin/amp-registration", "POST", payload);
}

/** PATCH /admin/amp-registration — partial update (empty password keeps the stored one). */
export async function updateAmpRegistration(
  patch: Partial<{ name: string; url: string; username: string; password: string }>,
): Promise<AmpRegistration> {
  return request<AmpRegistration>("/admin/amp-registration", "PATCH", patch);
}

/** DELETE /admin/amp-registration — unregister AMP. */
export async function unregisterAmp(): Promise<void> {
  await request<Record<string, never>>("/admin/amp-registration", "DELETE");
}
