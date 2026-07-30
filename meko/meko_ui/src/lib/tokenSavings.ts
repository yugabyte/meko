// tokenSavings.ts — turn the token-savings API response into the numbers the
// TOKEN BURN tile renders: the headline context-reduction % and the dollars
// saved versus each premier model.
//
// Two independent things are being valued here:
//   1. Context reduction (model-agnostic): contextual_prominence_search sends
//      a summary / a few memories instead of a full 10-chunk KB dump. That's a
//      pure token count, identical for every model → the headline %.
//   2. Cheaper generation model: inference runs GPT-OSS-20B, not a frontier
//      model. That only changes cost (price per token) → the per-model bars.
//
// Worked example (1000 requests, 6,000,000 tokens saved):
//   tokens_if_raw = 1000 × 10,000            = 10,000,000
//   tokens_sent   = 10,000,000 − 6,000,000   =  4,000,000
//   headline %    = 6,000,000 / 10,000,000   = 60% saved
//   $ saved vs a premier model = (tokens_if_raw × price_premier
//                                 − tokens_sent × price_oss) / 1e6
//     vs Opus    (@ $15/1M): (10M×15 − 4M×0.10)/1e6 = $149.60
//     vs GPT-5.6 (@ $10/1M): (10M×10 − 4M×0.10)/1e6 =  $99.60
//     vs Fable   (@  $5/1M): (10M× 5 − 4M×0.10)/1e6 =  $49.60

import type { TokenSavingsSummary } from "@/lib/api/mekoApi";
import { readRuntimeEnv } from "@/lib/runtimeEnv";

// Mirrors the BE/MCP `MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH` flag. The
// token-savings data is only produced when contextual prominence search is on,
// so the "Meko efficiency" tiles are hidden otherwise. Off by default.
export const CONTEXTUAL_PROMINENCE_SEARCH_ENABLED: boolean = ["1", "true", "yes"].includes(
  (readRuntimeEnv("MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH") ?? "").trim().toLowerCase(),
);

// Baseline every saving is measured against: a plain KB search sends
// KB_CHUNK_COUNT_ESTIMATE chunks of KB_CHUNK_TOKEN_ESTIMATE tokens each.
// MUST mirror meko-mcp-server src/tools.py (KB_CHUNK_COUNT_ESTIMATE ×
// KB_CHUNK_TOKEN_ESTIMATE, default 10 × 1000). If those env vars change on the
// server, update this constant to match — the API returns tokens_saved but not
// the baseline, so the UI reconstructs it from request count.
export const BASELINE_TOKENS_PER_REQUEST = 10_000;

// Blended $/1M-token rates. ILLUSTRATIVE placeholders — swap in real card
// rates. GPT-OSS-20B is what inference actually runs; the rest are the premier
// models we compare against, ordered most→least expensive so the bars descend.
export const OUR_INFERENCE_MODEL = {
  key: "gpt-oss-20b",
  label: "GPT-OSS-20B",
  usdPerMTok: 0.1,
} as const;

export const PREMIER_MODELS = [
  { key: "opus", label: "Opus", usdPerMTok: 15 },
  { key: "gpt-5.6", label: "GPT-5.6", usdPerMTok: 10 },
  { key: "fable", label: "Fable", usdPerMTok: 5 },
] as const;

export type ModelDollarsSaved = {
  key: string;
  label: string;
  dollars: number;
};

const clamp01 = (n: number): number => (n < 0 ? 0 : n > 1 ? 1 : n);

// Coerce a possibly-missing/NaN API number to 0. Guards against bad rows
// (e.g. a null field mid-migration) leaking NaN into the headline % or a
// bar's CSS width — the type says `number`, but the wire may disagree.
const num = (value: number | null | undefined): number =>
  typeof value === "number" && Number.isFinite(value) ? value : 0;

/** Baseline context tokens a naive full-RAG turn would have sent. */
export function tokensIfRaw(totalRequests: number): number {
  return Math.max(0, num(totalRequests)) * BASELINE_TOKENS_PER_REQUEST;
}

/** Context tokens actually sent = baseline − saved (never negative). */
export function tokensSent(summary: TokenSavingsSummary): number {
  return Math.max(0, tokensIfRaw(summary.total_requests) - num(summary.total_tokens_saved));
}

/**
 * Headline "token burn saved" fraction (0..1), model-agnostic:
 *   total_tokens_saved / (total_requests × BASELINE_TOKENS_PER_REQUEST)
 * Returns 0 when there are no requests yet.
 */
export function tokenBurnFraction(summary: TokenSavingsSummary): number {
  const raw = tokensIfRaw(summary.total_requests);
  return raw > 0 ? clamp01(num(summary.total_tokens_saved) / raw) : 0;
}

/** Same as tokenBurnFraction, rounded to a whole percent for display. */
export function tokenBurnPercent(summary: TokenSavingsSummary): number {
  return Math.round(tokenBurnFraction(summary) * 100);
}

/**
 * Dollars saved by running GPT-OSS-20B with reduced context instead of each
 * premier model on a full-RAG baseline:
 *   (tokens_if_raw × price_premier − tokens_sent × price_oss) / 1e6
 * Same order as PREMIER_MODELS (Opus → GPT-5.6 → Fable).
 */
export function dollarsSavedByModel(summary: TokenSavingsSummary): ModelDollarsSaved[] {
  const raw = tokensIfRaw(summary.total_requests);
  const sent = tokensSent(summary);
  return PREMIER_MODELS.map((m) => ({
    key: m.key,
    label: m.label,
    dollars: Math.max(0, (raw * m.usdPerMTok - sent * OUR_INFERENCE_MODEL.usdPerMTok) / 1_000_000),
  }));
}

// Human-readable labels for the source keys `contextual_prominence_search`
// writes into `tokens_saved_by_source` (see meko_system.token_savings_summary
// migration `000007_token_savings.up.sql`). The API uses the raw enum values
// straight from the MCP tool; the tile shouldn't render them verbatim (users
// don't know what "knowledge_base+memory" means). Unknown source keys fall
// back to the raw string so a new backend source doesn't silently disappear
// from the breakdown.
const SOURCE_LABELS: Record<string, string> = {
  conversation: "Conversation history",
  memory: "Memory",
  "knowledge_base+memory": "Knowledge Base",
};

/** Ordered so the two biggest signals in a typical Meko turn (KB retrieval
 *  and memory) surface at the top, with conversation history last. */
const SOURCE_DISPLAY_ORDER = ["knowledge_base+memory", "memory", "conversation"];

export type SourceBreakdown = {
  /** Raw source key from the API — kept for keys/analytics; not shown. */
  key: string;
  /** Human-readable label the tile renders. */
  label: string;
  /** Tokens saved via this source. */
  tokens: number;
  /** Share of total tokens saved, 0..1 (rounded to whole percent by consumer). */
  fraction: number;
};

/**
 * Break the total tokens saved down by retrieval source so the tile can
 * answer "how did we save?". Returns entries with a nonzero token count,
 * ordered by the canonical display order (KB → Memory → Conversation), with
 * any unknown source keys appended after in descending token order.
 */
export function sourceBreakdown(summary: TokenSavingsSummary): SourceBreakdown[] {
  const bySource = summary.tokens_saved_by_source ?? {};
  const total = num(summary.total_tokens_saved);
  const entries = Object.entries(bySource)
    .map(([key, tokens]) => ({
      key,
      label: SOURCE_LABELS[key] ?? key,
      tokens: num(tokens),
    }))
    .filter((e) => e.tokens > 0);

  const orderIndex = new Map(SOURCE_DISPLAY_ORDER.map((k, i) => [k, i]));
  entries.sort((a, b) => {
    const aIdx = orderIndex.get(a.key);
    const bIdx = orderIndex.get(b.key);
    if (aIdx != null && bIdx != null) return aIdx - bIdx;
    if (aIdx != null) return -1;
    if (bIdx != null) return 1;
    return b.tokens - a.tokens;
  });

  return entries.map((e) => ({
    ...e,
    fraction: total > 0 ? clamp01(e.tokens / total) : 0,
  }));
}

/** Compact token count for tile copy: 1_234 → "1.2K", 3_500_000 → "3.5M". */
export function formatTokens(tokens: number): string {
  const n = num(tokens);
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(Math.round(n));
}
