// AWS Bedrock on-demand per-token rates for the models "Run a Review" lets a
// user choose between (see ReviewAgentConfigPanel's MODEL_OPTIONS) plus the
// Claude reference model. Sourced September 2026: Ministral 14B confirmed
// directly from https://aws.amazon.com/bedrock/pricing/; Claude Sonnet 4.5,
// Llama 4 Maverick 17B, and Nova Lite from converging third-party trackers
// (AWS's own page didn't list all four in one fetch). Bedrock pricing does
// change -- reconcile against the live AWS page before treating a dollar
// figure derived from this table as authoritative, not just illustrative.
export const BEDROCK_PRICE_PER_MILLION_TOKENS: Record<string, { input: number; output: number }> = {
  "anthropic.claude-sonnet-4-5-20250929-v1:0": { input: 3.0, output: 15.0 },
  "mistral.ministral-3-14b-instruct": { input: 0.2, output: 0.2 },
  "us.meta.llama4-maverick-17b-instruct-v1:0": { input: 0.24, output: 0.97 },
  "amazon.nova-lite-v1:0": { input: 0.06, output: 0.24 },
};

/** Real cost for one call's real token counts, in dollars. Returns null for
 *  a model id not in the table above rather than silently returning 0 --
 *  a missing rate should be visibly missing, not read as "free." */
export function estimateCost(
  modelId: string,
  usage: { input_tokens: number; output_tokens: number },
): number | null {
  const rate = BEDROCK_PRICE_PER_MILLION_TOKENS[modelId];
  if (!rate) return null;
  return (usage.input_tokens * rate.input + usage.output_tokens * rate.output) / 1_000_000;
}

/**
 * "% less than Claude" for CostPayoffCard, from two already-computed costs.
 * Returns null (skip rendering, not "0% less") whenever the comparison isn't
 * meaningful: either cost unknown (a model with no rate in the table above),
 * or the reference cost is exactly 0 -- e.g. anthropicReferenceReview.ts's
 * fixture hasn't been populated with a real capture yet, so dividing by it
 * would render a bogus "Infinity%"/"NaN%" line. Pulled out of PrReviewDemoApp
 * as its own function specifically so this zero-reference-cost path stays
 * covered by a real test even after that fixture is eventually populated
 * with non-zero numbers (see this function's own test for the zero case).
 */
export function computeSavingsPct(slmCost: number | null, claudeCost: number | null): number | null {
  if (slmCost === null || claudeCost === null || claudeCost <= 0) return null;
  return Math.round((1 - slmCost / claudeCost) * 100);
}
