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
    else if (EXECUTE_NAMES.has(s.name)) execute.push(s);
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
      PREPROCESS_NAMES.has(steps[i].name) &&
      (!next || !PREPROCESS_NAMES.has(next.name));
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
  if (kbHits.length > 0)
    parts.push(`${kbHits.length} ${kbHits.length === 1 ? "chunk" : "chunks"}`);
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
    default:
      return name;
  }
}

export function stepKindLabel(name: string): string {
  if (
    name === "memory_search" ||
    name === "knowledgebase_search" ||
    name === "conversation_search" ||
    name === "memory_add"
  )
    return "DB";
  if (name === "llm_invoke" || name === "conversation_summarize") return "LLM";
  if (name === "context_assemble") return "FMT";
  if (name === "conversation_add_message") return "LOG";
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
      const n = step.conversation_chunks?.length ?? 0;
      return n > 0
        ? `Reused ${n} prior conversation ${n === 1 ? "answer" : "answers"}`
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
    default:
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
