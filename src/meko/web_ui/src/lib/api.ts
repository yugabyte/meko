// Meko's API from the browser: /api/v1 routes in the {data, error} envelope, search and adding a
// memory included (Meko's own routes, which run the MCP tools on this computer's datapack). There's
// no credential: Meko takes the browser by its access (local or remote); a 401 UNAUTHORIZED (local
// access that didn't look local) tells the app through onUnauthorized.

import type {
  AgentChange,
  Agents,
  AnswerStatus,
  Artifact,
  ArtifactList,
  AuthStatus,
  CallContents,
  CallDetail,
  CallFilter,
  CallPage,
  ContextSearch,
  ConversationDetail,
  Conversation,
  Datapack,
  DatapackExtraction,
  DocumentList,
  Extension,
  ExtractorStatus,
  ExtensionAction,
  Gap,
  GapPage,
  GapState,
  Health,
  KnowledgeActivity,
  Placement,
  Project,
  ProjectDetail,
  ProjectFolder,
  ProjectStats,
  ProjectTheme,
  MapSource,
  MemoryChange,
  MemoryMap,
  MemoryPage,
  MekoCloud,
  MekoCloudUpdate,
  Metrics,
  Passage,
  PromotedMemory,
  ProvisionStatus,
  SearchAnswer,
  ServerStatus,
  UpdateStatus,
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
  if (res.status === 204) return undefined as T;
  const env = (await res.json().catch(() => null)) as
    | { data: T; error: { code: string; message: string } | null }
    | null;
  if (!env || env.error) {
    throw new ApiError(env?.error?.message ?? res.statusText, res.status, env?.error?.code ?? "HTTP_ERROR");
  }
  return env.data;
}

const enc = encodeURIComponent;

export type SearchSource = "auto" | "memory" | "knowledge_base" | "conversation";

export const api = {
  health: async (): Promise<Health> => {
    const res = await fetch("/healthz");
    return (await res.json()).data;
  },

  // Every datapack, by name: each row's web_url says where it opens (this UI's path, or Meko Cloud's
  // web app while it's linked).
  datapacks: () => request<Datapack[]>("GET", "/datapacks"),
  datapack: (id: string) => request<Datapack>("GET", `/datapacks/${id}`),
  // Meko makes it in the linked Meko Cloud while it's linked, else on this computer; the reply's
  // web_url says where it opens.
  createDatapack: (name: string, description: string) =>
    request<Datapack>("POST", "/datapacks", { name, description }),
  updateDatapack: (id: string, fields: { name?: string; description?: string }) =>
    request<unknown>("PATCH", `/datapacks/${id}`, fields),
  deleteDatapack: (id: string) => request<unknown>("DELETE", `/datapacks/${id}`),

  projectThemes: () => request<ProjectTheme[]>("GET", "/projects/themes"),
  projects: () => request<{ owned: Project[] }>("GET", "/projects").then((r) => r.owned),
  project: (id: string) => request<ProjectDetail>("GET", `/projects/${id}`),
  createProject: (body: { project_name: string; description: string; theme_key: string; folders?: string[] }) =>
    request<Project>("POST", "/projects", body),
  updateProject: (id: string, body: { project_name?: string; description?: string }) =>
    request<unknown>("PATCH", `/projects/${id}`, body),
  deleteProject: (id: string) => request<unknown>("DELETE", `/projects/${id}`),
  projectStats: (id: string) => request<ProjectStats>("GET", `/projects/${id}/stats`),
  projectActivity: (id: string, cursor: string) =>
    request<{ items: KnowledgeActivity[]; has_more: boolean; next_cursor: string }>(
      "GET",
      `/projects/${id}/activity-log?limit=20&cursor=${enc(cursor)}`,
    ),
  createFolder: (project: string, body: { folder_name: string; folder_description: string }) =>
    request<ProjectFolder>("POST", `/projects/${project}/folders`, body),
  updateFolder: (project: string, folder: string, body: { folder_name?: string; folder_description?: string }) =>
    request<unknown>("PATCH", `/projects/${project}/folders/${folder}`, body),
  deleteFolder: (project: string, folder: string) => request<unknown>("DELETE", `/projects/${project}/folders/${folder}`),
  placeDatapack: (datapack: string, folder: string) =>
    request<unknown>("POST", `/datapacks/${datapack}/placement`, { folder_id: folder }),
  unplaceDatapack: (datapack: string) => request<unknown>("DELETE", `/datapacks/${datapack}/placement`),
  placements: () => request<Record<string, Placement>>("GET", "/projects/placements"),
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

  // How Meko takes this browser (src/meko/docs/web-ui.md): local or remote access, with no login.
  authStatus: () => request<AuthStatus>("GET", "/auth/status"),
  extensions: () => request<Extension[]>("GET", "/extensions"),
  // Starts an action as the extension's job; the list shows how it goes.
  extensionAction: (name: string, action: ExtensionAction) =>
    request<Extension>("POST", `/extensions/${enc(name)}/${action}`),
  // Checks for releases now (Meko's and every extension's) and answers with the row.
  extensionCheck: (name: string) => request<Extension>("POST", `/extensions/${enc(name)}/check`),
  setExtensionAutoUpdate: (name: string, on: boolean) =>
    request<Extension>("PATCH", `/extensions/${enc(name)}`, { auto_update: on }),

  update: () => request<UpdateStatus>("GET", "/update"),
  checkUpdate: () => request<UpdateStatus>("POST", "/update/check"),
  // Starts installing the newest release; the status's job shows how it goes, and Meko restarts.
  applyUpdate: () => request<UpdateStatus>("POST", "/update/apply"),

  // The call log, newest first, a page at a time (before: the last id of the page before).
  calls: (f: CallFilter, before?: number) => {
    const q = new URLSearchParams();
    if (f.agent) q.set("agent", f.agent);
    if (f.tool) q.set("tool", f.tool);
    if (f.client) q.set("client", f.client);
    if (f.failed) q.set("failed", "true");
    if (before) q.set("before", String(before));
    const qs = q.toString();
    return request<CallPage>("GET", `/calls${qs ? `?${qs}` : ""}`);
  },
  call: (id: number) => request<CallDetail>("GET", `/calls/${id}`),
  // "Keep call contents", the owner only to change; clearing keeps the calls.
  callContents: () => request<CallContents>("GET", "/settings/call-contents"),
  setCallContents: (keep: boolean) => request<CallContents>("PUT", "/settings/call-contents", { keep }),
  clearCallContents: () => request<CallContents>("DELETE", "/calls/contents"),

  usageStats: () => request<UsageStats>("GET", "/settings/usage-stats"),
  setUsageStats: (enabled: boolean) => request<UsageStats>("PATCH", "/settings/usage-stats", { enabled }),
  mekoCloud: () => request<MekoCloud>("GET", "/settings/meko-cloud"),
  linkMekoCloud: (u: MekoCloudUpdate) => request<MekoCloud>("PUT", "/settings/meko-cloud", u),
  unlinkMekoCloud: () => request<MekoCloud>("DELETE", "/settings/meko-cloud"),

  // The agents installed on this computer and whether each reaches Meko (or the linked Meko Cloud).
  agents: () => request<Agents>("GET", "/agents"),
  connectAgent: (name: string) => request<AgentChange>("POST", `/agents/${enc(name)}/connect`),
  disconnectAgent: (name: string) => request<AgentChange>("POST", `/agents/${enc(name)}/disconnect`),

  memories: (id: string, opts: { scope: "memories" | "learnings"; q: string; page: number }) =>
    request<MemoryPage>(
      "GET",
      `/memories?datapack_id=${id}&scope=${opts.scope}&q=${enc(opts.q)}&page=${opts.page}`,
    ),
  memoryMap: (id: string, q = "", ids: string[] = []) =>
    request<MemoryMap>("GET", `/datapacks/${id}/memory-map?q=${enc(q)}&ids=${enc(ids.join(","))}`),
  // The 100 things nearest a question among sources, and hits (what the search returned) besides.
  searchMap: (id: string, q: string, sources: MapSource[], hits: string[]) =>
    request<MemoryMap>(
      "GET",
      `/datapacks/${id}/search-map?q=${enc(q)}&sources=${sources.join(",")}&hits=${enc(hits.join(","))}`,
    ),
  memoryExtractor: () => request<ExtractorStatus>("GET", "/settings/memory-extractor"),
  answers: () => request<AnswerStatus>("GET", "/settings/answers"),
  // Turn memory extraction or Search's answers on through harness (the server finds its command), or
  // off with "". The owner only; turning one on makes one test call first.
  setMemoryExtractor: (harness: string) =>
    request<ExtractorStatus>("PUT", "/settings/memory-extractor", { harness }),
  setAnswers: (harness: string) => request<AnswerStatus>("PUT", "/settings/answers", { harness }),
  // An answer to query written from a search's passages, through the user's own Claude Code.
  answer: (id: string, query: string, passages: Passage[]) =>
    request<SearchAnswer>("POST", `/datapacks/${id}/answer`, { query, passages }),
  datapackExtraction: (id: string) => request<DatapackExtraction>("GET", `/datapacks/${id}/memory-extraction`),
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
  // memory_add and context_search on the datapack, through Meko's own routes rather than /mcp, so the
  // UI shows what an agent would get.
  addMemory: (id: string, text: string) =>
    request<unknown>("POST", `/datapacks/${id}/memories`, {
      agent_id: UI_AGENT_ID,
      conversation_id: UI_CONVERSATION_ID,
      text,
    }),

  // Context Gaps: a page of the questions in a state (open by default), after the last one shown.
  gaps: (id: string, state: GapState | "all" = "open", after?: { last_at: string; key: string }) =>
    request<GapPage>(
      "GET",
      `/datapacks/${id}/questions?state=${state}` +
        (after ? `&after=${enc(after.last_at)}&after_key=${enc(after.key)}` : ""),
    ),
  // Dismiss a question, or restore one (null): open again, what was taught kept.
  markGap: (id: string, key: string, state: "dismissed" | null) =>
    request<void>("POST", `/datapacks/${id}/questions/mark`, { key, state }),
  // Saves text as a memory in the datapack and marks the question taught.
  teachGap: (id: string, key: string, text: string) =>
    request<Gap>("POST", `/datapacks/${id}/questions/teach`, { key, text }),

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
    request<ContextSearch>("POST", `/datapacks/${id}/search`, {
      query,
      force_source: source,
      agent_id: UI_AGENT_ID,
      conversation_id: UI_CONVERSATION_ID,
    }),
};
