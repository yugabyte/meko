// The Analytics tab's numbers, derived in one place from /metrics and the datapack's own counts
// (the memory breakdown), so no two cards compute the same number differently. Formulas follow Meko
// Cloud's Analytics tab, except token estimates, which come from bytes: Meko local's chunks are 1000
// characters, not Cloud's 1024 tokens.

import type { Datapack, Metrics } from "./types";

// About four bytes of English text per token.
const BYTES_PER_TOKEN = 4;
// The window the "window used" gauge measures against, as Cloud's does.
export const CONTEXT_WINDOW = 128_000;
// Score bands, as the server counts them: cosine similarity >= 0.75 high, >= 0.5 mid.
export const BAND_HIGH = 0.75;
export const BAND_MID = 0.5;

export interface Split {
  knowledge: number;
  memory: number;
  conversation: number;
  total: number;
}

export interface Derived {
  tokens: Split;
  bytes: Split;
  savedPct: number | null;
  avgTokensSent: number | null;
  avgTokensIfRaw: number | null;
  windowUsed: number | null;
  scores: { high: number; mid: number; low: number; total: number };
  qualityScore: number | null; // (high + mid) / total
  queries: number;
  promoted: number;
  kept: number; // kept as private memories
  pending: number; // learnings, neither kept nor promoted yet
}

function split(knowledge: number, memory: number, conversation: number): Split {
  return { knowledge, memory, conversation, total: knowledge + memory + conversation };
}

export function derive(m: Metrics, d: Datapack): Derived {
  const bytes = split(m.kb_bytes, m.memory_bytes, m.conversation_bytes);
  const tok = (b: number) => Math.round(b / BYTES_PER_TOKEN);
  const hasSavings = m.total_requests > 0 && m.tokens_if_raw_total > 0;
  const avgTokensSent = hasSavings
    ? Math.round((m.tokens_if_raw_total - m.tokens_saved_total) / m.total_requests)
    : null;
  const high = m.score_high_total;
  const mid = m.score_mid_total;
  const low = m.score_low_total;
  const scored = high + mid + low;
  const promoted = d.collective_memory_count ?? 0;
  return {
    bytes,
    tokens: split(tok(bytes.knowledge), tok(bytes.memory), tok(bytes.conversation)),
    savedPct: hasSavings ? m.savings_pct : null,
    avgTokensSent,
    avgTokensIfRaw: hasSavings ? Math.round(m.tokens_if_raw_total / m.total_requests) : null,
    windowUsed: avgTokensSent !== null ? avgTokensSent / CONTEXT_WINDOW : null,
    scores: { high, mid, low, total: scored },
    qualityScore: scored > 0 ? (high + mid) / scored : null,
    queries: m.daily_activity.reduce((s, day) => s + day.hybrid + day.kb + day.memory, 0),
    promoted,
    // memory_count is learnings and kept memories; learnings are the pending part of it.
    kept: Math.max(0, (d.memory_count ?? 0) - (d.learnings_count ?? 0)),
    pending: d.learnings_count ?? 0,
  };
}

export function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

export function pct(part: number, whole: number): number {
  return whole > 0 ? Math.round((100 * part) / whole) : 0;
}
