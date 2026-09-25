import axios from "axios";
import { adminHttp as http } from "./authContext";

export type DailyLoginPoint = {
  date: string;
  count: number;
};

export type CognitoUser = {
  email: string;
  status: string;
  createdAt: string;
};

export type AdminMetrics = {
  usage: {
    dailyLogins: DailyLoginPoint[];
    neverLoggedInUsers: CognitoUser[];
    totalUsers: number;
    userRegistrations: DailyLoginPoint[];
  };
  system: {
    queuedWorkItems: number;
    datapacksCreated: number;
    documentsProcessed: number;
    embeddingsGenerated: number;
  };
};

export async function fetchAdminMetrics(): Promise<AdminMetrics> {
  const { data } = await http.get<AdminMetrics>("/metrics");
  return data;
}

/* ---- AMP controller registration ---- */

export type AmpRegistration = { name: string; url: string; username?: string };

/** GET the current AMP registration, or null if AMP is not registered. */
export async function getAmpRegistration(): Promise<AmpRegistration | null> {
  try {
    const { data } = await http.get<AmpRegistration>("/amp-registration");
    return data;
  } catch (e) {
    if (axios.isAxiosError(e) && e.response?.status === 404) return null;
    throw e;
  }
}

/** POST to register (or replace) the AMP controller endpoint. */
export async function registerAmp(payload: {
  name: string;
  url: string;
  username?: string;
  password?: string;
}): Promise<AmpRegistration> {
  const { data } = await http.post<AmpRegistration>("/amp-registration", payload);
  return data;
}

/** PATCH a partial update of the registration. */
export async function updateAmpRegistration(
  patch: Partial<{ name: string; url: string; username: string; password: string }>,
): Promise<AmpRegistration> {
  const { data } = await http.patch<AmpRegistration>("/amp-registration", patch);
  return data;
}

/** DELETE the registration (unregister AMP). */
export async function unregisterAmp(): Promise<void> {
  await http.delete("/amp-registration");
}

/* ---- usage metrics (proxied from api_server) ---- */

// Canonical account tiers (meko_system.account_type). Enterprise is sales-led
// and never set from the admin console, but a user may already have it.
export type Tier = "free" | "standard" | "pro" | "enterprise";

export type AdminUser = {
  id: string;
  email: string;
  first_name: string;
  last_name: string;
  tier: Tier;
  created_at: string;
};

// The per-user metrics tab is split into six panels the page fetches in
// parallel (mirroring the executive dashboard) so a heavy user's slow KB/Langfuse
// query never blocks the fast tiles or times out the whole tab. Every count is
// scoped to the user and windowed to the selected range.

/** Overview panel: tier, owned datapacks (all-time) and shares in range. */
export type UserOverview = {
  scope: string;
  tier: Tier;
  datapacks: number;
  shared_datapacks: number;
};

/** Memories panel: private + collective memory counts in range. */
export type UserMemories = {
  private_memories: number;
  collective_memories: number;
};

/** Conversations panel: total + by-agent breakdown in range (Langfuse). */
export type UserConversations = {
  conversations: number;
  conversations_by_agent: AgentConversationCount[];
  langfuse_enabled: boolean;
};

/** Tokens panel: token burn in range (Langfuse). */
export type UserTokens = {
  token_burn: number;
  langfuse_enabled: boolean;
};

/** Searches panel: KB + memory + context search counts in range (free-tier
 *  counters). context_search is a distinct unit and does not double-count into
 *  the other two. */
export type UserSearches = {
  kb_searches: number;
  memory_searches: number;
  context_searches: number;
};

/** Knowledge panel: indexed KB docs + chunks and the by-file-type breakdown. */
export type UserKnowledge = {
  kb_documents: number;
  kb_chunks: number;
  kb_documents_by_type: ExecDocTypeCount[];
};

/** List users for the dashboard scope dropdown. */
export async function fetchAdminUsers(): Promise<AdminUser[]> {
  const { data } = await http.get<{ users: AdminUser[] }>("/usage/users");
  return data.users;
}

async function fetchUserPanel<T>(
  panel: string,
  userId: string,
  start?: string,
  end?: string,
): Promise<T> {
  const params: Record<string, string> = { user_id: userId };
  if (start) params.start = start;
  if (end) params.end = end;
  const { data } = await http.get<T>(`/usage/${panel}`, { params });
  return data;
}

export const fetchUserOverview = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserOverview>("overview", userId, start, end);
export const fetchUserMemories = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserMemories>("memories", userId, start, end);
export const fetchUserConversations = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserConversations>("conversations", userId, start, end);
export const fetchUserTokens = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserTokens>("tokens", userId, start, end);
export const fetchUserSearches = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserSearches>("searches", userId, start, end);
export const fetchUserKnowledge = (userId: string, start?: string, end?: string) =>
  fetchUserPanel<UserKnowledge>("knowledge", userId, start, end);

/* ---- data plane (YBA universe, proxied from api_server) ---- */

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

/** Live YugabyteDB Anywhere universe configuration + best-effort utilization. */
export async function fetchDataPlane(): Promise<DataPlaneInfo> {
  const { data } = await http.get<DataPlaneInfo>("/usage/dataplane");
  return data;
}

/* ---- executive dashboard (proxied from api_server) ---- */

export type ExecUserSavings = {
  user_id: string;
  email: string;
  first_name: string;
  last_name: string;
  request_count: number;
  tokens_saved: number;
};

/** One file-type bucket for the "indexed documents by file type" chart. `type`
 *  is the stored MIME type (e.g. application/pdf); the UI maps it to a label. */
export type ExecDocTypeCount = {
  type: string;
  count: number;
};

/** One week's datapack-creation count for the week-over-week chart.
 *  `week_start` is the Monday of the ISO week (YYYY-MM-DD). */
export type ExecWeekCount = {
  week_start: string;
  count: number;
};

/** One agent's conversation count for the conversations-by-agent chart. */
export type AgentConversationCount = { agent: string; conversations: number };

// The executive dashboard is split into four panels the page fetches in
// parallel; every count is windowed to the selected date range.

/** Growth panel: users + datapacks created in range, plus the weekly trend. */
export type ExecGrowth = {
  users_created: number;
  datapacks_created: number;
  datapacks_by_week: ExecWeekCount[];
};

/** Engagement panel: conversations (total + by agent) and promoted memories. */
export type ExecEngagement = {
  conversations: number;
  conversations_by_agent: AgentConversationCount[];
  promoted_memories: number;
  langfuse_enabled: boolean;
};

/** Knowledge panel: indexed KB docs + chunks and the by-file-type breakdown. */
export type ExecKnowledge = {
  kb_documents: number;
  kb_chunks: number;
  kb_documents_by_type: ExecDocTypeCount[];
};

/** Savings panel: contextual-search requests + tokens saved and top savers. */
export type ExecSavings = {
  total_requests: number;
  total_tokens_saved: number;
  top_users: ExecUserSavings[];
};

async function fetchExecPanel<T>(panel: string, start?: string, end?: string): Promise<T> {
  const params: Record<string, string> = {};
  if (start) params.start = start;
  if (end) params.end = end;
  const { data } = await http.get<T>(`/usage/executive/${panel}`, { params });
  return data;
}

export const fetchExecGrowth = (start?: string, end?: string) =>
  fetchExecPanel<ExecGrowth>("growth", start, end);
export const fetchExecEngagement = (start?: string, end?: string) =>
  fetchExecPanel<ExecEngagement>("engagement", start, end);
export const fetchExecKnowledge = (start?: string, end?: string) =>
  fetchExecPanel<ExecKnowledge>("knowledge", start, end);
export const fetchExecSavings = (start?: string, end?: string) =>
  fetchExecPanel<ExecSavings>("savings", start, end);

/** Override a single user's tier (Free <-> Pro). */
export async function updateUserTier(
  userId: string,
  tier: string,
): Promise<{ id: string; tier: string }> {
  const { data } = await http.patch<{ id: string; tier: string }>(`/usage/users/${userId}/tier`, {
    tier,
  });
  return data;
}

/** One tier-change audit event: who changed a user's tier, old→new, when. */
export type TierAuditEntry = {
  actor_email: string;
  actor_uuid?: string;
  source: string;
  old_tier: string;
  new_tier: string;
  created_at: string;
};

/** Fetch a user's tier-change history (newest first). */
export async function fetchUserTierAudit(userId: string): Promise<TierAuditEntry[]> {
  const { data } = await http.get<TierAuditEntry[]>(`/usage/users/${userId}/tier-audit`);
  return data ?? [];
}

/* ---- feedback (paged, date-range filtered) ---- */

/** One admin feedback row. Nullable columns arrive as `null` from the
 *  JSON encoder (Go's `omitempty` drops the key, so the FE treats
 *  "missing" and "null" the same). */
export type AdminFeedbackItem = {
  id: string;
  created_at: string;
  updated_at?: string | null;
  user_id?: string | null;
  source: string;
  source_identifier?: string | null;
  datapack_id?: string | null;
  datapack_name?: string | null;
  context_route?: string | null;
  category: string;
  body: string;
  meko_version?: string | null;
  status: string;
};

export type AdminFeedbackResponse = {
  feedback: AdminFeedbackItem[];
  pagination: { total: number; limit: number; offset: number };
};

/** GET /api/admin/feedback — paged product feedback filtered by
 *  created_at range, newest first. `from` / `to` are YYYY-MM-DD;
 *  `limit` is 1..100 (api_server clamps and defaults 50); `offset`
 *  is a non-negative index. Undefined params are dropped so callers
 *  can share one function across "default 30 days" and other views. */
export async function fetchAdminFeedback(opts: {
  from?: string;
  to?: string;
  limit?: number;
  offset?: number;
}): Promise<AdminFeedbackResponse> {
  const params: Record<string, string> = {};
  if (opts.from) params.from = opts.from;
  if (opts.to) params.to = opts.to;
  if (opts.limit != null) params.limit = String(opts.limit);
  if (opts.offset != null) params.offset = String(opts.offset);
  const { data } = await http.get<AdminFeedbackResponse>("/feedback", { params });
  return data;
}
