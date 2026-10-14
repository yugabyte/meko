import { priceFor } from '../shared/pricing.js';

// The estimate of what a Meko search saved. Without Meko, Claude would have found the same thing
// by exploring: several tool calls, each re-reading the whole context and adding file contents to
// it. With Meko it took one call and added the search result. The constants come from this host's
// Claude Code transcripts (98 sessions and their subagents, 2026-09-29).

/**
 * Tool calls one search that found something stands in for: a search of the code and two reads.
 * A session makes a median of 14 exploring calls (Read, Grep, Glob, Bash) before its first edit.
 */
export const EXPLORE_CALLS = 3;
/**
 * Tokens each of those calls adds to the context. The median Read result is about 5,800 tokens
 * and the median Bash result about 150, so one read and two shell calls add about 6,000.
 */
export const EXPLORE_RESULT_TOKENS = 2000;
/** Output tokens of a response that makes a tool call: the median over 6,009 of them. */
export const CALL_OUTPUT_TOKENS = 240;
/**
 * Later responses counted as re-reading the extra content. The median search is followed by 211
 * more, but compaction cuts the context, so only the next 10 count, which keeps this on the low side.
 */
export const CARRY_TURNS = 10;

/** The Meko tools that count: searches, not fetches by id. */
export const SEARCH_TOOL_NAMES = ['memory_search', 'knowledgebase_search', 'context_search'] as const;
export const SEARCH_TOOLS: ReadonlySet<string> = new Set(SEARCH_TOOL_NAMES);

/**
 * The lowest score of a memory worth giving Claude: AgentK's search at start (`recall.ts`) leaves
 * out memories below it, and a search whose memories are all below it, with nothing from the
 * knowledge base or the conversation cache, found nothing relevant (`mekousage.ts`). Memories
 * Meko ranks high can still be off-topic: unrelated ones have scored 0.74–0.76 (2026-10-13).
 */
export const MIN_MEMORY_SCORE = 0.75;

export interface FoundSearch {
  /** Input, cache read and cache write of the response that made the call. */
  contextTokens: number;
  resultTokens: number;
  /** Responses after the call in the same conversation or subagent. */
  laterResponses: number;
  /** Model of the response that made the call. */
  model: string | null;
}

/** Tokens and USD one search that found something saved, or one seed AgentK passed at start (usd is null for a model with no known price). */
export function searchSaving(s: FoundSearch): { tokens: number; usd: number | null } {
  const explored = EXPLORE_CALLS * EXPLORE_RESULT_TOKENS;
  const carry = Math.min(CARRY_TURNS, Math.max(0, s.laterResponses));
  const rereads = (EXPLORE_CALLS - 1) * s.contextTokens; // fewer passes over the context
  const output = (EXPLORE_CALLS - 1) * CALL_OUTPUT_TOKENS; // fewer tool-calling responses
  const content = explored - s.resultTokens; // less new content (negative for a very large result)
  const carried = content * carry; // …re-read by the next responses
  const tokens = rereads + output + content + carried;
  const p = s.model ? priceFor(s.model) : null;
  // New content is written to the 1-hour cache (2× input), as Claude Code does.
  const usd = p ? (rereads * p.cacheRead + output * p.output + content * p.input * 2 + carried * p.cacheRead) / 1e6 : null;
  return { tokens, usd };
}
