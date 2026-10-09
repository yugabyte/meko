// What one review costs, as a function of the two choices Chapter 2 asks the
// user to make: whether the agent reads the datapack, and which model runs.
//
// The context side is DERIVED FROM THE READER'S OWN KNOWLEDGE BASE rather
// than hardcoded, because the ratio is the argument: without a datapack the
// prompt carries every chunk, with one it carries the few that matched, so
// the saving is simply (chunks in the KB / chunks retrieved). That grows on
// its own as the knowledge base does, and nothing has to be invented.
//
// It replaces a fixed 1,180-vs-16,400 pair, which was worse than imprecise:
// this lab's own bundled standards document is about 375 tokens, so the
// "without datapack" figure was some forty times the real corpus and the
// comparison inverted on real data.
//
// Chapter 2 runs BEFORE the review, so it assumes a typical retrieval
// (TYPICAL_CHUNKS_RETRIEVED) and says so. Chapter 3 assumes nothing: it
// reads kb_chunks_retrieved off the run.

import { BEDROCK_PRICE_PER_MILLION_TOKENS } from "./bedrockPricing";

/** How many chunks a search typically matches. A search returns what is
 *  relevant to one diff, not everything under its limit, so this stays small
 *  however large the knowledge base grows. Chapter 2 has no run to read;
 *  Chapter 3 uses the real count off the run. */
export const TYPICAL_MATCHING_CHUNKS = 1;

/** Output is the review itself, which barely moves between strategies — the
 *  agent writes about the same number of comments either way. */
const OUTPUT_TOKENS = 700;

/** The model a team reaches for when it does NOT trust its context: the whole
 *  point of the lesson is that good retrieval removes the need for it. */
export const FRONTIER_MODEL_ID = "anthropic.claude-sonnet-4-5-20250929-v1:0";

/** What one knowledge chunk is priced at when Chapter 3 estimates the cost of
 *  sending the whole knowledge base. A round number on purpose: it is the
 *  only figure on that side of the comparison, so it has to be one a reader
 *  can hold in their head. Anything quoting it must say it is an estimate. */
export const LAB_TOKENS_PER_CHUNK = 1_000;

/** What a review with no datapack to query has to carry: every chunk in the
 *  knowledge base, because there is no way to send a subset. Null when the
 *  chunk count is unknown, so a caller shows nothing rather than zero. */
export function coldContextTokens(kbChunksTotal: number | null | undefined): number | null {
  if (!kbChunksTotal || kbChunksTotal <= 0) return null;
  return kbChunksTotal * LAB_TOKENS_PER_CHUNK;
}

/** A knowledge chunk is a paragraph or more; a memory is usually one line. So
 *  when the run's input is divided between the two searches, a chunk claims
 *  more of it than a memory does. */
const KB_WEIGHT = 2;
const MEMORY_WEIGHT = 1;

/**
 * Divides a run's measured input tokens between the two searches that
 * produced its context, in proportion to what each retrieved and weighted
 * toward knowledge.
 *
 * APPORTIONMENT, not measurement. The real prompt also carries the diff and
 * the system prompt, so the two halves here add up to more retrieval than
 * actually happened; Bedrock exposes no pre-call tokenizer, so no exact
 * per-section count exists to use instead. Every caller says so.
 */
export function splitInputTokens(
  inputTokens: number,
  kbChunksRetrieved: number,
  memoriesRetrieved: number,
): { kb: number; memory: number } {
  const kbWeight = Math.max(0, kbChunksRetrieved) * KB_WEIGHT;
  const memoryWeight = Math.max(0, memoriesRetrieved) * MEMORY_WEIGHT;
  const total = kbWeight + memoryWeight;
  if (total <= 0 || inputTokens <= 0) return { kb: 0, memory: 0 };
  const kb = Math.round((inputTokens * kbWeight) / total);
  // The remainder rather than a second round, so the two always add back up
  // to the number the Cost tab reports.
  return { kb, memory: inputTokens - kb };
}

export type ReviewCost = {
  inputTokens: number;
  outputTokens: number;
  /** Dollars for one review. Null when the model has no published rate. */
  dollars: number | null;
  /** How the retrieved context scores, in the terms Chapter 3 then measures. */
  quality: "high" | "low";
};

/**
 * Context tokens either way, for a knowledge base of `kbChunks`.
 *
 * A search sends the chunks that match the diff; no search sends all of
 * them. Both sides come off the reader's own chunk count, so the saving is
 * the base divided by what matched and it grows as the base does.
 */
export function contextTokens(kbChunks: number | null | undefined): {
  withDatapack: number;
  withoutDatapack: number;
  matched: number;
  kbChunks: number;
} {
  const chunks = Math.max(0, kbChunks ?? 0);
  // A knowledge base smaller than one match sends all of it, which is the
  // true answer for a datapack that holds almost nothing.
  const matched = Math.min(TYPICAL_MATCHING_CHUNKS, chunks);
  return {
    withDatapack: matched * LAB_TOKENS_PER_CHUNK,
    withoutDatapack: chunks * LAB_TOKENS_PER_CHUNK,
    matched,
    kbChunks: chunks,
  };
}

export function reviewCost(
  modelId: string,
  useDatapack: boolean,
  kbChunks?: number | null,
): ReviewCost {
  const context = contextTokens(kbChunks);
  const inputTokens = useDatapack ? context.withDatapack : context.withoutDatapack;
  const rate = BEDROCK_PRICE_PER_MILLION_TOKENS[modelId];
  return {
    inputTokens,
    outputTokens: OUTPUT_TOKENS,
    dollars: rate ? (inputTokens * rate.input + OUTPUT_TOKENS * rate.output) / 1_000_000 : null,
    // Without the datapack the agent has the document but not your team's
    // memories, and nothing tying a comment to a rule — which is what
    // Chapter 1's cold run demonstrated.
    quality: useDatapack ? "high" : "low",
  };
}

/** How many times more one review costs than another. Null when either side
 *  has no rate, so a caller shows nothing rather than "0×". */
export function costMultiple(dearer: ReviewCost, cheaper: ReviewCost): number | null {
  if (dearer.dollars === null || cheaper.dollars === null || cheaper.dollars <= 0) return null;
  return dearer.dollars / cheaper.dollars;
}
