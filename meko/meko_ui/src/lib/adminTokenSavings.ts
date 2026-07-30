// adminTokenSavings.ts — turns raw token-savings counters (request_count +
// tokens_saved) into the numbers the Admin Executive Dashboard renders: the
// headline context-reduction % and the dollars saved versus each premier model.
//
// This deliberately mirrors meko_admin/src/lib/tokenSavings.ts (a `(requests,
// tokensSaved)` API), NOT meko_ui's own src/lib/tokenSavings.ts, which takes a
// TokenSavingsSummary object for the per-datapack TOKEN BURN tile. The two share
// the same formulas but different call shapes, so the admin views keep their own
// copy. api_server returns tokens_saved and a request count but NOT the
// baseline, so — like both existing helpers — we reconstruct the baseline from
// the request count (10 chunks × 1000 tokens per request). If meko-mcp-server's
// KB_CHUNK_COUNT_ESTIMATE × KB_CHUNK_TOKEN_ESTIMATE changes, update
// BASELINE_TOKENS_PER_REQUEST here (and in the two mirrors) to match.

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
// leaking NaN into the headline % or a bar's CSS width.
const num = (value: number | null | undefined): number =>
  typeof value === "number" && Number.isFinite(value) ? value : 0;

/** Baseline context tokens a naive full-RAG turn would have sent. */
export function tokensIfRaw(requests: number): number {
  return Math.max(0, num(requests)) * BASELINE_TOKENS_PER_REQUEST;
}

/** Context tokens actually sent = baseline − saved (never negative). */
export function tokensSent(requests: number, tokensSaved: number): number {
  return Math.max(0, tokensIfRaw(requests) - num(tokensSaved));
}

/**
 * Headline "token burn saved" fraction (0..1), model-agnostic:
 *   tokens_saved / (requests × BASELINE_TOKENS_PER_REQUEST)
 * Returns 0 when there are no requests yet.
 */
export function tokenBurnFraction(requests: number, tokensSaved: number): number {
  const raw = tokensIfRaw(requests);
  return raw > 0 ? clamp01(num(tokensSaved) / raw) : 0;
}

/** Same as tokenBurnFraction, rounded to a whole percent for display. */
export function tokenBurnPercent(requests: number, tokensSaved: number): number {
  return Math.round(tokenBurnFraction(requests, tokensSaved) * 100);
}

/**
 * Dollars saved by running GPT-OSS-20B with reduced context instead of each
 * premier model on a full-RAG baseline:
 *   (tokens_if_raw × price_premier − tokens_sent × price_oss) / 1e6
 * Same order as PREMIER_MODELS (Opus → GPT-5.6 → Fable).
 */
export function dollarsSavedByModel(requests: number, tokensSaved: number): ModelDollarsSaved[] {
  const raw = tokensIfRaw(requests);
  const sent = tokensSent(requests, tokensSaved);
  return PREMIER_MODELS.map((m) => ({
    key: m.key,
    label: m.label,
    dollars: Math.max(0, (raw * m.usdPerMTok - sent * OUR_INFERENCE_MODEL.usdPerMTok) / 1_000_000),
  }));
}

/**
 * Single headline dollars-saved figure vs the most expensive premier model
 * (Opus) — the biggest, most executive-friendly number. Used for per-user rows
 * where the full three-bar breakdown would be too dense.
 */
export function dollarsSavedVsTopModel(requests: number, tokensSaved: number): number {
  return dollarsSavedByModel(requests, tokensSaved)[0]?.dollars ?? 0;
}

/** Compact money formatter: $1.2k, $338k, $1.4M. */
export function formatUsd(dollars: number): string {
  const n = num(dollars);
  if (n >= 1_000_000) return `$${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `$${(n / 1_000).toFixed(1)}k`;
  return `$${n.toFixed(n < 10 ? 2 : 0)}`;
}
