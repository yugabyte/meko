// Axios client for the FastAPI inference gateway (`meko/inference_gateway`).
//
// Separate from `mekoApi.ts` because:
//   - Different base URL (gateway runs on its own host/port).
//   - Different response shape (no `{ data, error }` envelope; gateway returns
//     the body directly with FastAPI-style `detail` strings on errors).
//
// Defaults to the same-origin `/inference` path, which the Node server (and,
// deployed, the chart's nginx) strips before forwarding to the gateway, so the
// browser never makes a cross-origin call to a host it has to be told about
// separately. Set `VITE_INFERENCE_GATEWAY_URL` to reach a gateway directly
// instead, e.g. `http://localhost:8088` when serving the app without either.

import axios from "axios";

import { readRuntimeEnv } from "@/lib/runtimeEnv";

const BASE_URL = (import.meta.env.VITE_INFERENCE_GATEWAY_URL || "/inference").replace(/\/$/, "");

const client = axios.create({
  baseURL: BASE_URL,
  headers: { "Content-Type": "application/json" },
  // LLM calls can take 30+ seconds (parallel MCP retrieval + provider response);
  // give plenty of headroom so we don't false-timeout under load.
  timeout: 120_000,
});

/** Body of POST /chat. Mirrors `inference_gateway/routes/chat.py:ChatRequest`. */
export type GatewayChatRequest = {
  message: string;
  /** Bearer token forwarded as `Authorization: Bearer <token>` to MCP. */
  user_token: string;
  /** Authenticated account id; echoed for telemetry. */
  user_id: string;
  /** Reuse to keep the in-memory sliding window of the last 10 messages. */
  session_id?: string;
  /** Optional datapack scope. When null, MCP uses the caller's default. */
  datapack_id?: string | null;
  /** Resume an existing conversation: when set, the gateway adopts this
   *  conversation_id instead of minting a new one via MCP. The UI sends
   *  it when the user picked a previous chat from the dropdown — keeps
   *  every new turn appended to the same conversation row in Langfuse. */
  conversation_id?: string | null;
  /** When true, skip the conversation-history cache for this turn and answer
   *  via the full memory + KB + LLM path. Set by the "Regenerate with full
   *  search" button after a cache-hit answer the user wasn't happy with. */
  bypass_conversation_cache?: boolean;
  /** Named persona preset selecting this turn's system-prompt framing.
   *  Currently defined values: "review_agent". Omitted -> default
   *  WorkbenchLM playground-assistant persona. */
  persona?: string;
};

/** One structured memory hit returned by `memory_search`. `kind`
 *  separates Mem0's three parallel arrays:
 *    "mem"      — vector-matched fact
 *    "graph"    — Apache AGE graph triple (relations)
 *    "promoted" — relation elevated to the tenant KB
 */
export type MemoryHit = {
  kind: "mem" | "graph" | "promoted";
  text: string;
  /** Mem0 cosine-similarity in [0..1]. Missing for graph triples. */
  score?: number;
  char_count?: number;
};

/** One structured KB chunk hit from `knowledgebase_search`. */
export type KbHit = {
  /** `document_id` (UUID) or its basename when the source carried a path.
   *  The UI groups chunks by this value so each document shows up once. */
  document_id: string;
  /** Resolved file basename for document-sourced hits (empty for promoted
   *  memory hits or when the id couldn't be resolved). */
  document_name?: string;
  /** "memory" when this hit is a promoted memory living in the KB rather
   *  than a chunk from an uploaded document. */
  source?: string;
  chunk_text: string;
  /** Cosine similarity (1 - distance), same value shape as memory scores.
   *  Not clamped here — the UI clamps to [0..100] on display. */
  score?: number;
  /** Raw pgvector cosine distance in [0..2]; lower = closer. Clusters near
   *  ~1.0 even for relevant chunks. Shown as the KB `dist` chip. */
  distance?: number;
  char_count?: number;
};

/** One operation in the per-turn observability timeline. `name` matches
 *  the MCP tool / pipeline stage; `status` distinguishes successful
 *  invocations from errors (chain continued in best-effort mode) and
 *  skips (trivial query, missing datapack id, fire-and-forget).
 *
 *  `query` is populated for retrieval steps (`memory_search`,
 *  `knowledgebase_search`) — the exact string sent to MCP. UI shows
 *  it in the Agent Reasoning panel so users see what slice of their
 *  prompt the gateway actually searched on. None for non-retrieval
 *  steps. */
export type ProcessingStep = {
  name: string;
  duration_ms: number;
  status: "ok" | "error" | "skipped";
  detail?: string;
  query?: string;
  /** Rest of the args sent to MCP for this step — everything except
   *  `query`, which has its own dedicated field. Includes the full
   *  payload (scope, conversation_id, agent_id, limit, datapack_id)
   *  so the "MCP args" sub-expand is an honest "what we sent" view.
   *  Null for steps with no extra args worth showing (llm_invoke,
   *  conversation_add_message, fire-and-forget memory_add). */
  tool_args?: Record<string, unknown>;
  /** Langfuse observation id for this step. Used by the Decision Trace
   *  card to deep-link the row to the matching span. Missing for
   *  synthesized rows that don't map to a single observation. */
  span_id?: string;
  /** Parent step's span_id when this step ran nested inside another (e.g.
   *  conversation_search under knowledgebase_search). Used to indent it. */
  parent_span_id?: string;
  /** Sub-timings rolled up under this step (e.g. `vector_ms`, `graph_ms`
   *  for retrieval). Empty/undefined when the step has no surfaced
   *  child spans. */
  sub_timings?: Record<string, number>;
  /** Prior-conversation excerpts retrieved by this step — populated only
   *  on the `conversation_search` step. Each links back to its source
   *  conversation and the user who authored it. */
  conversation_chunks?: ConversationChunk[];
  /** Hits produced by this step (only on the matching retrieval step), so a
   *  turn with several searches shows each step's own chunks. */
  memory_hits?: MemoryHit[];
  kb_hits?: KbHit[];
};

/** One prior-conversation excerpt that fed a conversation_search hit. */
export type ConversationChunk = {
  content?: string;
  /** Cosine similarity (1 - distance), same value shape as memory and KB
   *  scores. Absent on turns traced before the gateway emitted it, and on
   *  thread turns carrying no distance. */
  score?: number;
  distance?: number;
  conversation_id?: string;
  message_id?: string;
  user_id?: string;
};

/** Response shape from POST /chat. */
export type GatewayChatResponse = {
  message: string;
  session_id: string;
  conversation_id: string;
  /** Langfuse message_id for this assistant turn. Used by the UI to
   *  deep-link into the trace drawer. Empty when persistence didn't
   *  return one (best-effort). */
  message_id?: string;
  /** Bulleted retrieval context fed into the LLM's system prompt. Kept
   *  for raw-prompt debug UX; the UI's Agent Reasoning panel renders
   *  the structured `memory_hits` / `kb_hits` arrays below instead. */
  memory_context?: string;
  kb_context?: string;
  /** Structured retrieval hits — drives the per-bubble Agent Reasoning
   *  panel (groups KB chunks by document, shows score per hit). */
  memory_hits?: MemoryHit[];
  kb_hits?: KbHit[];
  /** Per-turn pipeline timeline (conversation_create, memory_search,
   *  knowledgebase_search, llm_invoke, conversation_add_message,
   *  memory_add). Rendered as a short list in the Agent Reasoning panel
   *  so users see what ran and how long it took without opening the
   *  full trace drawer. */
  processing_steps?: ProcessingStep[];
  /** Agentic chat (MEKO-407 Phase 2): a confirm-tier action the model
   *  proposed this turn. NOT executed — render a confirmation card and
   *  resolve it via `confirmAction`. Null/absent when nothing is pending. */
  pending_action?: PendingAction | null;
};

/** A confirm-tier tool call awaiting the user's Allow/Deny. The args were
 *  frozen by the gateway at proposal time (identity fields enforced
 *  server-side); Allow executes exactly these args, Deny discards them. */
export type PendingAction = {
  action_id: string;
  tool_name: string;
  args?: Record<string, unknown>;
};

export type ConfirmActionResponse = {
  /** "executed" | "cancelled" | "error" */
  status: string;
  detail?: string;
  /** Parsed tool result on success (shape is tool-specific). */
  result?: unknown;
};

/** Resolve a pending agentic action: approve=true executes it with the
 *  frozen args, approve=false discards it without touching MCP. */
export async function confirmAction(body: {
  session_id: string;
  action_id: string;
  user_token: string;
  approve: boolean;
}): Promise<ConfirmActionResponse> {
  const { data } = await client.post<ConfirmActionResponse>("/chat/confirm-action", body);
  return data;
}

export async function postChat(body: GatewayChatRequest): Promise<GatewayChatResponse> {
  const { data } = await client.post<GatewayChatResponse>("/chat", body);
  return data;
}

/** Body of POST /index-conversation — backfill the conversation cache for one
 *  past conversation. */
export type IndexConversationRequest = {
  conversation_id: string;
  user_token: string;
  datapack_id?: string | null;
};

export type IndexConversationResponse = {
  indexed: number;
  can_index: boolean;
  reason: string;
  /** True when the plan's embedding quota ran out mid-backfill: `indexed`
   *  turns were embedded, the remaining turns were not. */
  quota_exceeded?: boolean;
};

/** Whether the conversation-search / conversation-embedding feature is on for
 *  this deployment (MEKO_ENABLE_CONVERSATION_SEARCH via runtime config — the
 *  same env var api_server folds into `can_index` and meko-mcp-server uses to
 *  register the backfill tool). Client-side mirror so feature-off deployments
 *  never render the entry point at all; truthy parsing matches the BE/MCP
 *  servers ("1" | "true" | "yes", case-insensitive). */
export function isConversationSearchEnabled(): boolean {
  const v = (readRuntimeEnv("MEKO_ENABLE_CONVERSATION_SEARCH") ?? "").trim().toLowerCase();
  return v === "1" || v === "true" || v === "yes";
}

export async function indexConversation(
  body: IndexConversationRequest,
): Promise<IndexConversationResponse> {
  const { data } = await client.post<IndexConversationResponse>("/index-conversation", body);
  return data;
}

/** Body of POST /memory/add — save a fact directly, not routed through a
 *  conversation (Labs' "add a memory" lesson). Mirrors
 *  inference_gateway/routes/chat.py's MemoryAddRequest. */
export type MemoryAddRequest = {
  text: string;
  user_token: string;
  datapack_id: string;
};

export type MemoryAddResponse = {
  ok: boolean;
  memory_id?: string | null;
};

export async function addMemory(body: MemoryAddRequest): Promise<MemoryAddResponse> {
  const { data } = await client.post<MemoryAddResponse>("/memory/add", body);
  return data;
}

/**
 * Pull a human-readable error string out of an axios failure against the
 * gateway. FastAPI surfaces structured errors via `{ detail: "..." }` (raised
 * with `HTTPException`); fall back to status / network messages otherwise.
 */
export function getInferenceGatewayErrorMessage(err: unknown, fallback = "Chat failed."): string {
  if (axios.isAxiosError(err)) {
    const detail = err.response?.data?.detail;
    if (typeof detail === "string" && detail.trim()) {
      return detail;
    }
    if (err.code === "ECONNABORTED" || /timeout/i.test(err.message ?? "")) {
      return "Chat request timed out. Try again.";
    }
    if (!err.response) {
      return "Cannot reach inference gateway. Is it running on port 8088?";
    }
    return `${err.response.status} ${err.response.statusText || "Gateway error"}`;
  }
  if (err instanceof Error && err.message.trim()) {
    return err.message;
  }
  return fallback;
}
