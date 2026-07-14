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
