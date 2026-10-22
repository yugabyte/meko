// Meko's API from the browser: /api/v1 routes in the {data, error} envelope, and MCP tools at
// /mcp for what has no REST route (search). Both send the client token from lib/token.

import { getToken } from "./token";
import type {
  ContextSearch,
  ConversationDetail,
  Conversation,
  Datapack,
  DocumentList,
  Health,
  MemoryPage,
  Metrics,
  PromotedMemory,
  ProvisionStatus,
  UploadOffer,
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

function headers(json: boolean): HeadersInit {
  const h: Record<string, string> = {};
  const token = getToken();
  if (token) h.Authorization = `Bearer ${token}`;
  if (json) h["Content-Type"] = "application/json";
  return h;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    headers: headers(body !== undefined),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const env = (await res.json().catch(() => null)) as
    | { data: T; error: { code: string; message: string } | null }
    | null;
  if (!res.ok || !env || env.error) {
    throw new ApiError(env?.error?.message ?? res.statusText, res.status, env?.error?.code ?? "HTTP_ERROR");
  }
  return env.data;
}

async function callTool<T>(name: string, args: Record<string, unknown>): Promise<T> {
  const res = await fetch("/mcp", {
    method: "POST",
    headers: { ...headers(true), Accept: "application/json, text/event-stream" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args } }),
  });
  if (!res.ok) throw new ApiError(res.statusText, res.status, "HTTP_ERROR");
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
  metrics: (id: string) => request<Metrics>("GET", `/datapacks/${id}/metrics`),
  connect: (id: string) => request<ProvisionStatus>("GET", `/datapacks/${id}/provision-status`),

  memories: (id: string, opts: { scope: "memories" | "learnings"; q: string; page: number }) =>
    request<MemoryPage>(
      "GET",
      `/memories?datapack_id=${id}&scope=${opts.scope}&q=${enc(opts.q)}&page=${opts.page}`,
    ),
  deleteMemory: (id: string, memoryId: string) =>
    request<unknown>("DELETE", `/memories/${memoryId}?datapack_id=${id}`),
  promote: (id: string, memoryIds: string[]) =>
    request<unknown>("POST", "/promote-memories", { datapack_id: id, memory_ids: memoryIds }),
  keep: (id: string, memoryIds: string[]) =>
    request<unknown>("POST", "/keep-as-memory", { datapack_id: id, memory_ids: memoryIds }),
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
    const res = await fetch(new URL(offer.url).pathname, { method: "POST", body: form });
    if (!res.ok) {
      const env = await res.json().catch(() => null);
      throw new ApiError(env?.error?.message ?? res.statusText, res.status, env?.error?.code ?? "UPLOAD_FAILED");
    }
    return offer;
  },

  search: (id: string, query: string) =>
    callTool<ContextSearch>("context_search", {
      query,
      datapack_id: id,
      agent_id: UI_AGENT_ID,
      conversation_id: UI_CONVERSATION_ID,
    }),
};
