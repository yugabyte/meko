// Shared types for the WorkbenchLM chat module. Pulled into its own file so
// the bubble / meta-row / enrichment / download submodules can reference the
// `Message` shape without circular imports back to `WorkbenchLMChat.tsx`.

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
  /** True when this assistant turn was answered from the conversation-history
   *  cache (a prior conversation summarized by the small model) rather than
   *  the full memory + KB + LLM path. Detected from the gateway's
   *  `processing_steps` (a `conversation_summarize` step) at send time.
   *  Drives the "Regenerate with full search" affordance on the bubble. */
  answeredFromCache?: boolean;
  /** The user prompt that produced this assistant turn — kept on the
   *  assistant message so the "Regenerate with full search" button can
   *  re-send the exact original question with the cache bypassed. Set only
   *  for cache-hit assistant turns. */
  sourceUserMessage?: string;
  // NOTE: `langfuseProjectId` lives at the page level — it's a per-datapack
  // constant fetched once per session and passed to each bubble via prop,
  // rather than carried per-message. See `WorkbenchLMPage`.
  //
  // The turn's retrieval hits / KB chunks / processing steps are NOT
  // stored on the message: the bubble's DecisionTrace fetches them
  // per-message via `getDatapackMessageDecisionTrace` (react-query
  // cached). For historical turns they come inline from the loaded
  // thread (`DatapackThreadMessage`). So nothing here mirrors the
  // gateway's `memory_hits` / `kb_hits` / `processing_steps`.
};
