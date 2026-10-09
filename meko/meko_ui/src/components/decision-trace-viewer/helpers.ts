// Pure helpers shared by the Decision Trace components.
//
// Kept separate from the React tree so the formatters / parsers are
// trivially unit-testable and so the component files can stay focused
// on JSX. Nothing in this module touches React or the DOM.

import type { KbHit, MemoryHit, ProcessingStep } from "@/lib/inferenceGateway";

/** Steps rendered under the "Preprocess" phase. `context_assemble` is
 *  synthesized client-side (see `insertSyntheticContextAssemble`) for
 *  Conversations because the external agent does the assembly outside
 *  our process and Langfuse never sees it. */
export const PREPROCESS_NAMES = new Set([
  "memory_search",
  "knowledgebase_search",
  "conversation_search",
  "context_assemble",
  // Agentic chat (MEKO-407): the router's route decision precedes retrieval.
  "tool_route",
]);

/** Steps rendered under the "Execute" phase. */
export const EXECUTE_NAMES = new Set([
  "conversation_summarize",
  "llm_invoke",
  "memory_add",
  "conversation_add_message",
]);

/** Bucket processing steps into the three Decision Trace phases.
 *  Unknown step names fall through silently so future names don't
 *  require a frontend deploy. */
export function groupStepsByPhase(steps: ProcessingStep[]): {
  preprocess: ProcessingStep[];
  execute: ProcessingStep[];
} {
  const preprocess: ProcessingStep[] = [];
  const execute: ProcessingStep[] = [];
  for (const s of steps) {
    if (PREPROCESS_NAMES.has(s.name)) preprocess.push(s);
    // Agentic tool executions are dynamic names (agent_tool:<mcp tool>).
    else if (EXECUTE_NAMES.has(s.name) || s.name.startsWith("agent_tool:")) execute.push(s);
  }
  return { preprocess, execute };
}

// `reasoning` and `plan` are independent fields, but for legacy rows
// the plan steps still live inside the reasoning prose under a
// `**Plan:**` heading (the server used to render them inline before
// the structured-plan field shipped). This helper:
//   1. Prefers `messagePlan` when the structured field is populated.
//   2. Otherwise extracts a `**Plan:**` block from the prose and parses
//      its numbered / bulleted list items into a string[].
//   3. Returns the reasoning prose with the extracted block stripped
//      so we never render the same plan twice.
const PLAN_HEADING_RE = /\n+\s*\*\*Plan:\*\*\s*\n+([\s\S]+)$/;
const PLAN_LIST_ITEM_RE = /^\s*(?:\d+\.|[-*])\s+(.+)$/;

export function derivePlanAndReasoning(
  reasoningRaw: string,
  messagePlan: string[] | undefined,
): { reasoningText: string; planSteps: string[] } {
  if (messagePlan && messagePlan.length > 0) {
    const cleaned = reasoningRaw.replace(PLAN_HEADING_RE, "").trim();
    return { reasoningText: cleaned, planSteps: messagePlan.slice() };
  }
  if (!reasoningRaw) {
    return { reasoningText: "", planSteps: [] };
  }
  const match = reasoningRaw.match(PLAN_HEADING_RE);
  if (!match) {
    return { reasoningText: reasoningRaw, planSteps: [] };
  }
  const planSteps = match[1]
    .split(/\n/)
    .map((line) => line.match(PLAN_LIST_ITEM_RE)?.[1]?.trim() ?? "")
    .filter((s) => s.length > 0);
  const prose = reasoningRaw.slice(0, match.index).trim();
  return { reasoningText: prose, planSteps };
}

/** Inject a synthesized `context.assemble` step after the last preprocess
 *  step (or at the head if there isn't one) so the user can see how much
 *  retrieved context fed into the prompt. The real observation doesn't
 *  exist for Conversations — the external agent assembles the prompt in
 *  its own process — so we derive char-count and token estimate from the
 *  retrieved hits. */
export function insertSyntheticContextAssemble(
  steps: ProcessingStep[],
  memoryHits: MemoryHit[],
  kbHits: KbHit[],
): ProcessingStep[] {
  if (memoryHits.length === 0 && kbHits.length === 0) return steps;
  const memoryTokens = estimateTokensFromChars(sumChars(memoryHits.map((h) => h.text)));
  const knowledgeTokens = estimateTokensFromChars(sumChars(kbHits.map((h) => h.chunk_text)));
  const promptTokens = memoryTokens + knowledgeTokens;
  if (promptTokens === 0) return steps;

  const synthetic: ProcessingStep = {
    name: "context_assemble",
    duration_ms: 0,
    status: "ok",
    detail: `${memoryHits.length} mem + ${kbHits.length} chunks`,
    tool_args: {
      memory_tokens: memoryTokens,
      knowledge_tokens: knowledgeTokens,
      total_prompt_tokens: promptTokens,
    },
  };
  const out: ProcessingStep[] = [];
  let inserted = false;
  for (let i = 0; i < steps.length; i++) {
    out.push(steps[i]);
    const next = steps[i + 1];
    const isLastPreprocess =
      PREPROCESS_NAMES.has(steps[i].name) && (!next || !PREPROCESS_NAMES.has(next.name));
    if (!inserted && isLastPreprocess) {
      out.push(synthetic);
      inserted = true;
    }
  }
  if (!inserted) out.unshift(synthetic);
  return out;
}

function sumChars(xs: Array<string | undefined>): number {
  let n = 0;
  for (const s of xs) n += (s ?? "").length;
  return n;
}

/** Rough char→token estimate. We don't ship a real tokenizer in the
 *  browser, so we use the same ~4-chars-per-token heuristic OpenAI
 *  publishes (https://platform.openai.com/tokenizer). The synthetic
 *  context_assemble step labels the value as an estimate so users
 *  aren't misled. */
export function estimateTokensFromChars(chars: number): number {
  return Math.ceil(chars / 4);
}

export function preprocessTagline(
  steps: ProcessingStep[],
  memoryHits: MemoryHit[],
  kbHits: KbHit[],
): string {
  if (steps.length === 0 && memoryHits.length === 0 && kbHits.length === 0) {
    return "No retrieval recorded";
  }
  const parts: string[] = [];
  if (memoryHits.length > 0)
    parts.push(`${memoryHits.length} ${memoryHits.length === 1 ? "memory" : "memories"}`);
  if (kbHits.length > 0) parts.push(`${kbHits.length} ${kbHits.length === 1 ? "chunk" : "chunks"}`);
  if (parts.length === 0) {
    return `${steps.length} step${steps.length === 1 ? "" : "s"}`;
  }
  const tokens = estimateTokensFromChars(
    sumChars(memoryHits.map((h) => h.text)) + sumChars(kbHits.map((h) => h.chunk_text)),
  );
  const head = `Gathered ${parts.join(" + ")}`;
  return tokens > 0 ? `${head} · ~${tokens.toLocaleString()} tokens assembled` : head;
}

export function planTagline(planSteps: string[], hasReasoning: boolean): string {
  if (planSteps.length > 0) {
    return `Decomposed into ${planSteps.length} sub-decision${planSteps.length === 1 ? "" : "s"}`;
  }
  return hasReasoning ? "Chain-of-thought captured" : "No plan recorded";
}

export function executeTagline(steps: ProcessingStep[], memoriesExtracted: number): string {
  if (steps.length === 0) return "Nothing recorded";
  const llmSteps = steps.filter((s) => s.name === "llm_invoke").length;
  const parts: string[] = [];
  // Lead with the human-readable outcome ("Generated response") when an
  // LLM actually ran, matching the design language. Fall back to the raw
  // count for the (rare) multi-LLM case so we still surface multiplicity.
  if (llmSteps === 1) parts.push("Generated response");
  else if (llmSteps > 1) parts.push(`${llmSteps} LLM calls`);
  if (memoriesExtracted > 0)
    parts.push(`extracted ${memoriesExtracted} ${memoriesExtracted === 1 ? "memory" : "memories"}`);
  return parts.length ? parts.join(" · ") : `${steps.length} step${steps.length === 1 ? "" : "s"}`;
}

export function prettifyStepName(name: string): string {
  switch (name) {
    case "memory_search":
      return "memory.search";
    case "knowledgebase_search":
      return "knowledge.retrieve";
    case "conversation_search":
      return "conversation.search";
    case "conversation_summarize":
      return "conversation.summarize";
    case "memory_add":
      return "memory.add";
    case "context_assemble":
      return "context.assemble";
    case "conversation_add_message":
      return "conversation.persist";
    case "llm_invoke":
      return "llm.completion";
    case "tool_route":
      return "tool.route";
    default:
      if (name.startsWith("agent_tool:")) {
        return `tool.${name.slice("agent_tool:".length)}`;
      }
      return name;
  }
}

/** The one `answered_from` value meaning the main model was skipped and the
 *  conversation_search chunks ARE the answer. `conversation_context` is the
 *  blended case — the chunks fed the prompt and the model still ran — and
 *  `conversation_cache_rejected` is a full re-answer. Chunk presence does not
 *  imply any of the three: the step carries whatever the search retrieved. */
export const ANSWERED_FROM_CACHE = "conversation_cache";

/** Steps that read or write a datapack DB row, in the order the header's
 *  `db` chip lists them. Shared with DecisionTrace's breakdown so a step
 *  cannot be badged `DB` and left out of the count — which is exactly what
 *  happened to conversation_search once context_search started recording its
 *  conversation leg on every turn. conversation_add_message is deliberately
 *  absent: it is badged `LOG`, writing a Langfuse audit row, not a datapack
 *  row. */
export const DB_STEP_NAMES = [
  "memory_search",
  "knowledgebase_search",
  "conversation_search",
  "memory_add",
] as const;

/** One row of the header's `db` chip breakdown. */
export type DbBreakdownEntry = {
  name: string;
  prettyName: string;
  count: number;
  ms: number;
};

/** Per-step counts and durations behind the header's `db` chip, in
 *  DB_STEP_NAMES order — the same list `stepKindLabel` badges from, so a step
 *  cannot be badged `DB` and left out of the count. Steps with no occurrence
 *  are omitted rather than shown as zero. */
export function computeDbBreakdown(steps: ProcessingStep[]): DbBreakdownEntry[] {
  const acc = new Map<string, { count: number; ms: number }>();
  for (const s of steps) {
    if (!(DB_STEP_NAMES as readonly string[]).includes(s.name)) continue;
    const cur = acc.get(s.name) ?? { count: 0, ms: 0 };
    cur.count += 1;
    cur.ms += s.duration_ms ?? 0;
    acc.set(s.name, cur);
  }
  return DB_STEP_NAMES.filter((n) => acc.has(n)).map((n) => ({
    name: n,
    prettyName: prettifyStepName(n),
    count: acc.get(n)!.count,
    ms: acc.get(n)!.ms,
  }));
}

/** Rough client-side estimate of the tokens a cache hit saved, from the prior
 *  conversation content it answered with — what a full regeneration would have
 *  had to produce, at the usual ~4 chars/token. The BE threads no token count
 *  for this, deliberately.
 *
 *  Gated on `answeredFrom`, not on the chunks being there. Chunk presence used
 *  to imply a cache hit because only a cache hit wrote them; the step now
 *  carries whatever the conversation search retrieved, on turns the full model
 *  answered and on turns from agents that never touch the gateway. The blended
 *  case (`conversation_context`) is excluded for the same reason: those chunks
 *  fed the prompt and the model still ran, so nothing was saved. */
export function estimateTokensSaved(
  steps: ProcessingStep[],
  answeredFrom: string | undefined,
): number {
  if (answeredFrom !== ANSWERED_FROM_CACHE) return 0;
  const convStep = steps.find((s) => s.name === "conversation_search");
  const chunks = convStep?.conversation_chunks ?? [];
  if (chunks.length === 0) return 0;
  const chars = chunks.reduce((sum, c) => sum + (c.content?.length ?? 0), 0);
  return Math.round(chars / 4);
}

export function stepKindLabel(name: string): string {
  if ((DB_STEP_NAMES as readonly string[]).includes(name)) return "DB";
  if (name === "llm_invoke" || name === "conversation_summarize" || name === "tool_route")
    return "LLM";
  if (name === "context_assemble") return "FMT";
  if (name === "conversation_add_message") return "LOG";
  if (name.startsWith("agent_tool:")) return "MCP";
  return "";
}

export function stepSummary(step: ProcessingStep): string {
  const query = typeof step.query === "string" ? step.query : "";
  switch (step.name) {
    case "memory_search":
      return query ? `Searched memories for "${query}"` : "Searched memories";
    case "knowledgebase_search":
      return query ? `Retrieved chunks for "${query}"` : "Retrieved knowledge chunks";
    case "conversation_search": {
      // "Matched", not "Reused": these chunks are what the search returned,
      // which the turn may or may not have answered from. Whether it did is
      // `answered_from`, which this step does not carry.
      const n = step.conversation_chunks?.length ?? 0;
      return n > 0
        ? `Matched ${n} prior conversation ${n === 1 ? "turn" : "turns"}`
        : "Searched prior conversations";
    }
    case "conversation_summarize": {
      const model = (step.tool_args?.summary_model as string | undefined) ?? "small model";
      return `Summarized the matched conversation with ${model}`;
    }
    case "memory_add":
      return step.detail ? `Stored ${step.detail}` : "Extracted memories from this turn";
    case "context_assemble":
      return "Built final prompt from retrieved context";
    case "conversation_add_message":
      return "Persisted message to conversation log";
    case "llm_invoke":
      return "Called the model";
    case "tool_route":
      return step.detail ? `Routed to ${step.detail}` : "Routed the turn";
    default:
      if (step.name.startsWith("agent_tool:")) {
        const tool = step.name.slice("agent_tool:".length);
        return step.status === "skipped"
          ? `Proposed ${tool} (awaiting confirmation)`
          : `Executed ${tool}`;
      }
      return "";
  }
}

export function statusTextColor(status: ProcessingStep["status"]): string {
  switch (status) {
    case "error":
      return "text-red-600 dark:text-red-300";
    case "skipped":
      return "text-muted-foreground/70";
    default:
      return "text-emerald-600 dark:text-emerald-300";
  }
}

export function formatDuration(ms: number): string {
  if (!ms || ms <= 0) return "0ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

export function truncateMiddle(s: string, max: number): string {
  if (s.length <= max) return s;
  const head = Math.ceil((max - 1) / 2);
  const tail = Math.floor((max - 1) / 2);
  return `${s.slice(0, head)}…${s.slice(-tail)}`;
}
