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

export type AdminUser = {
  id: string;
  email: string;
  first_name: string;
  last_name: string;
  tier: "free" | "pro";
  created_at: string;
};

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
  // Langfuse-derived (Phase 2); zero/empty when langfuse_enabled is false.
  conversations: number;
  conversations_by_agent: { agent: string; conversations: number }[];
  token_burn: number;
  langfuse_enabled: boolean;
};

/** List users for the dashboard scope dropdown. */
export async function fetchAdminUsers(): Promise<AdminUser[]> {
  const { data } = await http.get<{ users: AdminUser[] }>("/usage/users");
  return data.users;
}

/** Usage metrics overall ("all") or for a single user, optionally windowed by a
 *  date range (YYYY-MM-DD, inclusive). */
export async function fetchUsageMetrics(
  userId: string,
  start?: string,
  end?: string,
): Promise<UsageMetrics> {
  const params: Record<string, string> = { user_id: userId };
  if (start) params.start = start;
  if (end) params.end = end;
  const { data } = await http.get<UsageMetrics>("/usage", { params });
  return data;
}

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

export type ExecutiveSummary = {
  total_users: number;
  total_datapacks: number;
  total_conversations: number;
  langfuse_enabled: boolean;
  total_requests: number;
  total_tokens_saved: number;
  top_users: ExecUserSavings[];
};

/** Executive dashboard rollup: headcount totals, account-wide contextual-search
 *  token savings, and the top datapack owners by tokens saved. */
export async function fetchExecutiveSummary(): Promise<ExecutiveSummary> {
  const { data } = await http.get<ExecutiveSummary>("/usage/token-savings");
  return data;
}

/** Override a single user's tier (Free <-> Pro). */
export async function updateUserTier(
  userId: string,
  tier: string,
): Promise<{ id: string; tier: string }> {
  const { data } = await http.patch<{ id: string; tier: string }>(
    `/usage/users/${userId}/tier`,
    { tier },
  );
  return data;
}
