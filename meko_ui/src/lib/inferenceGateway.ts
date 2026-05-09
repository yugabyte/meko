// Axios client for the FastAPI inference gateway (`meko/inference_gateway`).
//
// Separate from `mekoApi.ts` because:
//   - Different base URL (gateway runs on its own host/port).
//   - Different response shape (no `{ data, error }` envelope; gateway returns
//     the body directly with FastAPI-style `detail` strings on errors).
//
// Configure the base URL via `VITE_INFERENCE_GATEWAY_URL`. Defaults to
// `http://localhost:8088` for local dev.

import axios from "axios";

const BASE_URL = (
  import.meta.env.VITE_INFERENCE_GATEWAY_URL || "http://localhost:8088"
).replace(/\/$/, "");

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
};

export async function postChat(body: GatewayChatRequest): Promise<GatewayChatResponse> {
  const { data } = await client.post<GatewayChatResponse>("/chat", body);
  return data;
}

/**
 * Pull a human-readable error string out of an axios failure against the
 * gateway. FastAPI surfaces structured errors via `{ detail: "..." }` (raised
 * with `HTTPException`); fall back to status / network messages otherwise.
 */
export function getInferenceGatewayErrorMessage(
  err: unknown,
  fallback = "Chat failed.",
): string {
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
