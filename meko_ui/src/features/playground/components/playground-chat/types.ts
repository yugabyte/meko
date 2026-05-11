// Shared types for the Playground chat module. Pulled into its own file so
// the bubble / meta-row / enrichment / download submodules can reference the
// `Message` shape without circular imports back to `PlaygroundChat.tsx`.

import type {
  KbHit,
  MemoryHit,
  ProcessingStep,
} from "@/lib/inferenceGateway";

/**
 * Knowledge readiness for the active datapack:
 *   - `loading`    — first fetch hasn't returned yet
 *   - `absent`     — no documents uploaded yet
 *   - `processing` — has docs but none are COMPLETED yet
 *   - `ready`      — at least one doc is COMPLETED
 *
 * The chat input is enabled only on `ready`; the other states surface a
 * tailored sub-line under the input so the user knows what to do.
 */
export type KnowledgeReadiness = "loading" | "absent" | "processing" | "ready";

/** One bubble in the chat transcript. Assistant turns carry optional
 *  Langfuse + telemetry fields populated after `POST /chat` returns and
 *  the follow-up `getDatapackConversation` enrichment fetch lands. */
export type Message = {
  id: string;
  role: "user" | "assistant";
  content: string;
  /** Langfuse message_id (assistant turns only). Returned by the gateway
   *  alongside the assistant text — drives the "View trace" button so we
   *  can deep-link into the same TraceDrawer the Conversations viewer uses.
   *  Empty when persistence missed (the button stays hidden in that case). */
  langfuseMessageId?: string;
  /** Per-turn telemetry pulled from `getDatapackConversation` after the
   *  chat response lands. Same fields the Conversations viewer renders
   *  (latency, memories extracted, llm calls). All undefined while the
   *  enrichment fetch is in flight or if Langfuse hasn't surfaced the
   *  message yet — the meta row simply skips missing pieces. */
  latencyMs?: number;
  memoriesExtracted?: number;
  llmCalls?: number;
  // NOTE: `langfuseProjectId` lives at the page level — it's a per-datapack
  // constant fetched once per session and passed to each bubble via prop,
  // rather than carried per-message. See `PlaygroundPage`.
  /** Retrieval context the gateway fed into the system prompt for this
   *  turn — bulleted text blocks of memories / KB chunks. Kept for the
   *  "Show me the exact prompt" debug UX; the Agent Reasoning panel
   *  renders the structured `memoryHits` / `kbHits` arrays instead. */
  memoryContext?: string;
  kbContext?: string;
  /** Structured retrieval hits — drives the per-bubble Agent Reasoning
   *  panel. `kbHits` are grouped by `document_id` in the UI; both
   *  carry per-hit `score` and `char_count` for relevance / cost display. */
  memoryHits?: MemoryHit[];
  kbHits?: KbHit[];
  /** Per-turn pipeline timeline. Rendered as a short list at the
   *  bottom of the Agent Reasoning panel ("Steps"). */
  processingSteps?: ProcessingStep[];
};
