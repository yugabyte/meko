// Meko's API from the browser: /api/v1 routes in the {data, error} envelope, and MCP tools at
// /mcp for what has no REST route (search). The credential is the session cookie (or, with local
// access, none), which the browser sends itself; a 401 UNAUTHORIZED tells the app through
// onUnauthorized.

import type {
  Artifact,
  ArtifactList,
  AuthStatus,
  ContextSearch,
  ConversationDetail,
  Conversation,
  Datapack,
  DocumentList,
  Extension,
  ExtensionAction,
  Health,
  KnowledgeActivity,
  MemoryChange,
  MemoryPage,
  Metrics,
  PromotedMemory,
  ProvisionStatus,
  ServerStatus,
  TotpEnroll,
  UploadOffer,
  Usage,
  UsageStats,
} from "./types";

// The agent the UI's own MCP calls are recorded as in the call log.
export const UI_AGENT_ID = "meko_web_ui";
// context_search needs a conversation; the UI's searches share this one.
const UI_CONVERSATION_ID = "meko_web_ui";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
  }
}

// The app's handler for a request Meko refused for want of a credential (a session that ended, or a
// request that didn't reach Meko as a local one).
let unauthorized: () => void = () => {};

export function onUnauthorized(handler: () => void): void {
  unauthorized = handler;
}

// A failed reply's error, from Meko's envelope when it has one.
async function failure(res: Response, fallback: string): Promise<ApiError> {
  const env = (await res.json().catch(() => null)) as { error?: { code: string; message: string } } | null;
  const err = new ApiError(env?.error?.message ?? res.statusText, res.status, env?.error?.code ?? fallback);
  // A 401 without an envelope comes from no other route either (as /mcp's might).
  if (err.code === "UNAUTHORIZED" || (res.status === 401 && !env?.error)) unauthorized();
  return err;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw await failure(res, "HTTP_ERROR");
  const env = (await res.json().catch(() => null)) as
    | { data: T; error: { code: string; message: string } | null }
    | null;
  if (!env || env.error) {
    throw new ApiError(env?.error?.message ?? res.statusText, res.status, env?.error?.code ?? "HTTP_ERROR");
  }
  return env.data;
}

async function callTool<T>(name: string, args: Record<string, unknown>): Promise<T> {
  const res = await fetch("/mcp", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args } }),
  });
  if (!res.ok) throw await failure(res, "HTTP_ERROR");
  const rpc = await res.json();
  if (rpc.error) throw new ApiError(rpc.error.message, 500, "MCP_ERROR");
  const text: string = rpc.result?.content?.[0]?.text ?? "{}";
  const reply = JSON.parse(text);
  if (reply && typeof reply === "object" && "error" in reply && typeof reply.error === "string") {
    throw new ApiError(reply.detail ?? reply.error, 400, reply.error);
  }
  return reply as T;
}

const enc = encodeURIComponent;

export type SearchSource = "auto" | "memory" | "knowledge_base" | "conversation";

export const api = {
  health: async (): Promise<Health> => {
    const res = await fetch("/healthz");
    return (await res.json()).data;
  },

  datapacks: () => request<Datapack[]>("GET", "/datapacks"),
  datapack: (id: string) => request<Datapack>("GET", `/datapacks/${id}`),
  createDatapack: (name: string, description: string) =>
    request<Datapack>("POST", "/datapacks", { name, description }),
  updateDatapack: (id: string, fields: { name?: string; description?: string }) =>
    request<unknown>("PATCH", `/datapacks/${id}`, fields),
  deleteDatapack: (id: string) => request<unknown>("DELETE", `/datapacks/${id}`),
  // days 0 is the server's default window: 90 days of totals, 30 of activity.
  metrics: (id: string, days: number) => request<Metrics>("GET", `/datapacks/${id}/metrics?days=${days}`),
  artifacts: (id: string, q: string, page: number) =>
    request<ArtifactList>("GET", `/datapacks/${id}/artifacts?q=${enc(q)}&page=${page}`),
  artifact: (id: string, hash: string) => request<Artifact>("GET", `/datapacks/${id}/artifacts/${hash}`),
  // The datapack as one JSON file, and the name the server suggests for it.
  exportDatapack: async (id: string): Promise<{ filename: string; blob: Blob }> => {
    const res = await fetch(`/api/v1/datapacks/${id}/export`, { credentials: "same-origin" });
    if (!res.ok) throw await failure(res, "EXPORT_FAILED");
    const m = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "");
    return { filename: m?.[1] ?? "meko-export.json", blob: await res.blob() };
  },
  usage: (id: string, days: number) => request<Usage>("GET", `/datapacks/${id}/usage?days=${days}`),
  server: () => request<ServerStatus>("GET", "/server"),
  connect: (id: string) => request<ProvisionStatus>("GET", `/datapacks/${id}/provision-status`),

  // Sign-in (backend/docs/web-ui.md): the status needs no credential, nor do setup and login.
  authStatus: () => request<AuthStatus>("GET", "/auth/status"),
  setup: (token: string, username: string, password: string) =>
    request<unknown>("POST", "/auth/setup", { token, username, password }),
  login: (username: string, password: string, totp?: string) =>
    request<unknown>("POST", "/auth/login", { username, password, totp }),
  logout: () => request<unknown>("POST", "/auth/logout", {}),
  changePassword: (current: string, next: string) => request<unknown>("POST", "/auth/password", { current, next }),
  totpEnroll: () => request<TotpEnroll>("POST", "/auth/totp/enroll", {}),
  totpConfirm: (code: string) => request<unknown>("POST", "/auth/totp/confirm", { code }),
  totpDisable: (code: string) => request<unknown>("POST", "/auth/totp/disable", { code }),
  extensions: () => request<Extension[]>("GET", "/extensions"),
  // Starts an action as the extension's job; the list shows how it goes.
  extensionAction: (name: string, action: ExtensionAction) =>
    request<Extension>("POST", `/extensions/${enc(name)}/${action}`),

  usageStats: () => request<UsageStats>("GET", "/settings/usage-stats"),
  setUsageStats: (enabled: boolean) => request<UsageStats>("PATCH", "/settings/usage-stats", { enabled }),

  memories: (id: string, opts: { scope: "memories" | "learnings"; q: string; page: number }) =>
    request<MemoryPage>(
      "GET",
      `/memories?datapack_id=${id}&scope=${opts.scope}&q=${enc(opts.q)}&page=${opts.page}`,
    ),
  memoryHistory: (id: string, memoryId: string) =>
    request<{ history: MemoryChange[] }>("GET", `/memories/${memoryId}/history?datapack_id=${id}`),
  deleteMemory: (id: string, memoryId: string) =>
    request<unknown>("DELETE", `/memories/${memoryId}?datapack_id=${id}`),
  promote: (id: string, memoryIds: string[]) =>
    request<unknown>("POST", "/promote-memories", { datapack_id: id, memory_ids: memoryIds }),
  keep: (id: string, memoryIds: string[]) =>
    request<unknown>("POST", "/keep-as-memory", { datapack_id: id, memory_ids: memoryIds }),
  deletePromoted: (id: string, memoryId: string) =>
    request<unknown>("DELETE", `/promoted-memories/${memoryId}?datapack_id=${id}`),
  promoted: (id: string) =>
    request<{ promoted_memories: PromotedMemory[]; total: number }>("GET", `/promoted-memories?datapack_id=${id}`),
  addMemory: (id: string, text: string) =>
    callTool<unknown>("memory_add", {
      datapack_id: id,
      agent_id: UI_AGENT_ID,
      conversation_id: UI_CONVERSATION_ID,
      text,
    }),

  conversations: (id: string, q: string, offset: number) =>
    request<{ conversations: Conversation[]; has_more: boolean }>(
      "GET",
      `/datapacks/${id}/conversations?q=${enc(q)}&offset=${offset}`,
    ),
  conversation: (id: string, cid: string) =>
    request<{ conversation: ConversationDetail }>("GET", `/datapacks/${id}/conversations/${cid}`),

  documents: (id: string, q: string) =>
    request<DocumentList>("GET", `/datapacks/${id}/knowledge-bases/documents?q=${enc(q)}&page_size=200`),
  deleteDocument: (id: string, docId: string) =>
    request<unknown>("DELETE", `/datapacks/${id}/knowledge-bases/documents/${docId}`),
  activity: (id: string, cursor: string) =>
    request<{ items: KnowledgeActivity[]; has_more: boolean; next_cursor: string }>(
      "GET",
      `/datapacks/${id}/knowledge-bases/activity-log?limit=20&cursor=${enc(cursor)}`,
    ),
  downloadURL: (id: string, uri: string) =>
    request<{ url: string }>("POST", `/datapacks/${id}/knowledge-bases/download-url`, { document_uri: uri }),

  // Cloud's presigned flow: ask for a link with the file's size and SHA-256, then post it there.
  upload: async (id: string, file: File): Promise<UploadOffer> => {
    const bytes = await file.arrayBuffer();
    const digest = await crypto.subtle.digest("SHA-256", bytes);
    const hash = Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
    const offer = await request<UploadOffer>("POST", `/datapacks/${id}/knowledge-bases/upload-url`, {
      filename: file.name,
      size: file.size,
      content_hash: hash,
      client: "web_ui",
    });
    if (offer.already_uploaded) return offer;
    const form = new FormData();
    for (const [k, v] of Object.entries(offer.fields ?? {})) form.append(k, v);
    form.append("file", file);
    const res = await fetch(new URL(offer.url).pathname, { method: "POST", credentials: "same-origin", body: form });
    if (!res.ok) throw await failure(res, "UPLOAD_FAILED");
    return offer;
  },

  // source narrows it to one of memory, knowledge_base or conversation; auto searches all three.
  search: (id: string, query: string, source: SearchSource = "auto") =>
    callTool<ContextSearch>("context_search", {
      query,
      force_source: source,
      datapack_id: id,
      agent_id: UI_AGENT_ID,
      conversation_id: UI_CONVERSATION_ID,
    }),
};
