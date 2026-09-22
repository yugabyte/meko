// Everything the datapack metrics surfaces derive from one DatapackMetrics
// payload: token footprints, savings per query, and retrieval quality shares.
//
// Pure and in one place because the overview tiles and the analytics section
// both need the same numbers — when each computed its own, the tile and the
// card below it could disagree about the same datapack.

import type { DatapackMetrics } from "@/lib/api/mekoApi";

// Matches KB_CHUNK_COUNT_ESTIMATE in meko-mcp-server/src/tools.py.
// Used only when real chunk counts are unavailable.
const KB_CHUNKS_PER_FILE = 10;
// Matches KB_CHUNK_TOKEN_ESTIMATE — tokens per chunk.
export const KB_TOKENS_PER_CHUNK = 1024;
/** Rough per-memory token cost, for the pre-first-load estimate only. */
const MEMORY_TOKENS_ESTIMATE = 200;
/** The window the "Window used" gauge measures against. */
export const CONTEXT_WINDOW = 128_000;

/** Counts the detail page already has from its own queries, used to estimate
 *  a footprint before the metrics payload lands. */
export type DatapackContentCounts = {
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
};

export type DatapackMetricsDerived = {
  /** Estimated token footprint, split by source. */
  tokens: { knowledge: number; memory: number; conversation: number; total: number };
  /** Stored bytes, split the same way. */
  bytes: { knowledge: number; memory: number; conversation: number; total: number };
  /** Null when there is no retrieval data to derive savings from. */
  savedPct: number | null;
  avgTokensSent: number | null;
  avgTokensIfRaw: number | null;
  avgTokensSaved: number | null;
  /** Avg tokens sent as a share of CONTEXT_WINDOW, 0–1. */
  windowUsed: number | null;
  scores: { high: number; mid: number; low: number; total: number };
  /** (high + mid) / total, 0–1. Zero when nothing has been scored. */
  qualityScore: number;
  /** True once anything at all has been indexed, searched or scored. */
  hasAnyData: boolean;
};

export function deriveDatapackMetrics(
  metrics: DatapackMetrics | undefined,
  counts: DatapackContentCounts,
): DatapackMetricsDerived {
  const knowledgeTokens =
    (metrics?.kb_chunk_count ?? 0) > 0
      ? metrics!.kb_chunk_count * KB_TOKENS_PER_CHUNK
      : counts.knowledgeCount * KB_CHUNKS_PER_FILE * KB_TOKENS_PER_CHUNK;
  const memoryTokens = metrics
    ? metrics.memory_chunk_count * KB_TOKENS_PER_CHUNK
    : (counts.memoryCount + counts.collectiveMemoryCount) * MEMORY_TOKENS_ESTIMATE;
  const conversationTokens = metrics ? metrics.conversation_chunk_count * KB_TOKENS_PER_CHUNK : 0;

  const knowledgeBytes = metrics?.kb_bytes ?? 0;
  const memoryBytes = metrics?.memory_bytes ?? 0;
  const conversationBytes = metrics?.conversation_bytes ?? 0;

  // Savings are only meaningful once at least one request has been recorded
  // AND the backend reported what the raw cost would have been.
  const hasSavings =
    !!metrics && metrics.total_requests > 0 && (metrics.tokens_if_raw_total ?? 0) > 0;
  const avgTokensIfRaw = hasSavings
    ? Math.round(metrics!.tokens_if_raw_total / metrics!.total_requests)
    : null;
  const avgTokensSent = hasSavings
    ? Math.round(
        (metrics!.tokens_if_raw_total - metrics!.tokens_saved_total) / metrics!.total_requests,
      )
    : null;
  const avgTokensSaved = hasSavings
    ? Math.round(metrics!.tokens_saved_total / metrics!.total_requests)
    : null;

  const high = metrics?.score_high_total ?? 0;
  const mid = metrics?.score_mid_total ?? 0;
  const low = metrics?.score_low_total ?? 0;
  const scoreTotal = high + mid + low;

  const tokensTotal = knowledgeTokens + memoryTokens + conversationTokens;

  return {
    tokens: {
      knowledge: knowledgeTokens,
      memory: memoryTokens,
      conversation: conversationTokens,
      total: tokensTotal,
    },
    bytes: {
      knowledge: knowledgeBytes,
      memory: memoryBytes,
      conversation: conversationBytes,
      total: knowledgeBytes + memoryBytes + conversationBytes,
    },
    savedPct: hasSavings ? metrics!.savings_pct : null,
    avgTokensSent,
    avgTokensIfRaw,
    avgTokensSaved,
    windowUsed: avgTokensSent != null ? avgTokensSent / CONTEXT_WINDOW : null,
    scores: { high, mid, low, total: scoreTotal },
    qualityScore: scoreTotal > 0 ? (high + mid) / scoreTotal : 0,
    hasAnyData: tokensTotal > 0 || scoreTotal > 0 || hasSavings,
  };
}
